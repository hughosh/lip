package hstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"lip/harness/cfg"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// Permanent versus transient
// ---------------------------------------------------------------------------

// permanentError marks a failure that retrying cannot fix: a coid reserved
// twice with different content, a binding for a coid that was never reserved, an
// id collision.
//
// The distinction is load-bearing in both directions. A transient failure -- a
// full disk, a locked file -- must be retried FOREVER, because the record is
// audit-grade and dropping it loses evidence of the very condition that caused
// the failure. A permanent one must NOT be retried, because a rule violation at
// the head of a strictly ordered queue would wedge every audit record behind it
// for the life of the process, which turns one caller's bug into total
// blindness.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(format string, a ...any) error {
	return permanentError{err: fmt.Errorf(format, a...)}
}

func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// ---------------------------------------------------------------------------
// Submissions
// ---------------------------------------------------------------------------

// submission is one queued record and everything the writer needs to retry it.
//
// The three anomaly progress flags are what make §13.1's ordering survive a
// retry: a record whose row committed but whose journal append failed resumes at
// the journal rather than re-inserting the row.
type submission struct {
	seq      uint64
	kind     RecordKind
	receipt  Receipt
	attempts int

	run      runRecord
	reserve  orderReservation
	order    rest.CreateOrder
	role     quote.Role
	bind     orderBinding
	fill     fillRecord
	runID    string
	state    StateEvent
	anom     anomalyRecord
	delivery DeliveryAttempt

	rowDone     bool
	journalDone bool
	journalMs   int64
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

// StoreConfig is where the two durable artifacts live.
//
// Explicit constructor arguments and NOT `cfg.Params` fields. §16 is the
// parameter table the `run` row records verbatim, and §15's own commentary is
// that a run whose configuration cannot be reconstructed is a run whose evidence
// cannot be interpreted. Where the file sits is deployment, and putting it in
// the parameter table would make two deployments of the same configuration
// differ in their recorded configuration.
type StoreConfig struct {
	// DBPath is the absolute path to harness.db. It is opened, never created
	// fresh and never deleted.
	DBPath string
	// AnomalyLogPath is the absolute path to the append-only JSONL text
	// journal of §13.1, mode 0600.
	AnomalyLogPath string
}

// writerBackoff is the PRIVATE retry ladder. Bounded delay, unbounded attempts:
// a transient write failure is retried forever because the record is evidence,
// and the ladder exists so that retrying does not become a spin.
var writerBackoff = [...]time.Duration{
	50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond,
	500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 5 * time.Second,
}

func backoffFor(attempts int) time.Duration {
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(writerBackoff) {
		return writerBackoff[len(writerBackoff)-1]
	}
	return writerBackoff[i]
}

// defaultStallBound is the writer-progress bound. A single record that has been
// in flight this long is not slow, it is stuck, and everything queued behind it
// is not durable.
//
// It is measured on a MONOTONIC source. Wall milliseconds are kept for records
// -- `first_ms`, `bound_ms`, the journal's `written_ms` -- and for nothing else:
// a wall clock that steps backwards over an NTP correction makes an elapsed
// interval negative, and a stall detector built on one silently stops detecting
// exactly when the host's clock is being fixed. F21 is the same defect measured
// on the other side.
const defaultStallBound = 5 * time.Second

// HealthPollInterval is how often a caller must re-ask for health while a write
// may be in flight.
//
// A write that never returns cannot be observed by the goroutine blocked inside
// it, so the stall is observed by whoever asks. That makes the asking part of
// the contract rather than a diagnostic nicety: a store whose writer wedged and
// whose owner stopped polling reports the health it had before the wedge
// forever. `ping.Effects.NextStepMs` carries this deadline out to `lip-3af`.
const HealthPollInterval = time.Second

// Store is §15's sole writer.
type Store struct {
	mu      sync.Mutex
	queue   []*submission
	results []Result
	nextSeq uint64

	healthy   bool
	adding    bool
	stalled   bool
	failures  uint64
	committed uint64
	recoverAt uint64
	lastErr   string

	// fault is the STICKY permanent-failure condition. A record that will
	// never be written is evidence that is gone, and no later success makes it
	// come back -- so unlike a transient failure this does not clear when the
	// backlog drains. It lasts the Store's lifetime.
	fault string

	inflight      bool
	inflightSince time.Duration

	// running is the single-writer guard. Two goroutines in Run would each
	// claim the same head and each pop on completion, discarding the record
	// behind it without ever writing it.
	running bool
	// writerGone latches when a writer that owned the FIFO returns. Nothing
	// submitted afterwards can become durable, so it is terminal: a second Run
	// is refused rather than silently resuming, because health that recovered
	// would assert durability for the records the gap swallowed.
	writerGone bool
	closed     bool

	// resultCh is the capacity-one wake channel. Capacity one and not more:
	// the signal is "there is something to take", and a caller that takes
	// everything needs to be told once.
	resultCh chan struct{}
	// workCh wakes the writer. Same shape, same reason.
	workCh chan struct{}

	back backend
	jrnl journal
	rd   *Reader
	own  *Ownership

	// nowMs stamps RECORDS. It is a wall clock and is used for nothing else.
	nowMs func() int64
	// monoNow measures ELAPSED time. It is monotonic and is never written to a
	// record.
	monoNow    func() time.Duration
	sleep      func(context.Context, time.Duration) bool
	stallBound time.Duration
}

// Open opens or creates the operational store and reconciles the crash window.
//
// Reconciliation happens BEFORE the store is returned, so no caller can observe
// a pending-delivery view that has not been squared against the text journal.
// A truncated or contradictory journal fails closed: this returns an error and
// there is no store.
func Open(c StoreConfig) (*Store, error) {
	if err := distinctArtifacts(c.DBPath, c.AnomalyLogPath); err != nil {
		return nil, err
	}
	back, err := openSQLite(c.DBPath)
	if err != nil {
		return nil, err
	}
	jrnl, err := openJournal(c.AnomalyLogPath)
	if err != nil {
		back.close()
		return nil, err
	}
	rd, err := openReader(c.DBPath)
	if err != nil {
		jrnl.close()
		back.close()
		return nil, err
	}
	s, err := openWith(back, jrnl, rd)
	if err != nil {
		rd.Close()
		jrnl.close()
		back.close()
		return nil, err
	}
	return s, nil
}

// openWith is the shared constructor. Tests substitute a blocking or failing
// backend and journal here; production goes through Open.
func openWith(back backend, jrnl journal, rd *Reader) (*Store, error) {
	start := time.Now()
	s := &Store{
		healthy:    true,
		adding:     true,
		resultCh:   make(chan struct{}, 1),
		workCh:     make(chan struct{}, 1),
		back:       back,
		jrnl:       jrnl,
		rd:         rd,
		own:        newOwnership(),
		nowMs:      func() int64 { return time.Now().UnixMilli() },
		monoNow:    func() time.Duration { return time.Since(start) },
		sleep:      realSleep,
		stallBound: defaultStallBound,
	}
	if err := s.reconcile(); err != nil {
		return nil, err
	}
	bindings, err := back.loadBindings()
	if err != nil {
		return nil, err
	}
	s.own.load(bindings)
	return s, nil
}

func realSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// reconcile squares the two journals across a crash.
//
// Three outcomes, and only the first is repair:
//
//   - a row with `journaled_ms` NULL whose id is NOT in the text journal died
//     between step 1 and step 2. Its text is appended and synced now, and only
//     then is step 3 completed. This is the crash window, and it is exactly the
//     case for which the SQLite row commits first.
//   - a row with `journaled_ms` NULL whose id IS in the journal died between
//     step 2 and step 3. Step 3 is completed with the timestamp the TEXT became
//     durable, not with now: the record is when the operator's copy was written.
//   - a journal id with no row, or a journal record that disagrees with its row,
//     is a contradiction. It fails closed. Two durable stores that disagree
//     about what happened cannot be reconciled by preferring one of them, and
//     preferring either quietly decides which half of §13.1 is authoritative.
func (s *Store) reconcile() error {
	lines, err := s.jrnl.readAll()
	if err != nil {
		return fmt.Errorf("the anomaly text journal could not be read, and it "+
			"is what the operator has when the database cannot be opened: %w",
			err)
	}
	byID := make(map[string]journalLine, len(lines))
	for _, l := range lines {
		if prev, dup := byID[l.AnomalyID]; dup && prev != l {
			return permanent("anomaly %s appears twice in the text journal "+
				"with different content; the journal contradicts itself and "+
				"nothing downstream can be trusted to be the same record",
				l.AnomalyID)
		}
		byID[l.AnomalyID] = l
	}
	for id, l := range byID {
		row, ok, err := s.back.anomalyByID(id)
		if err != nil {
			return err
		}
		if !ok {
			return permanent("the anomaly text journal holds %s but the "+
				"database has no such row; the two durable records of §13.1 "+
				"disagree about whether the event happened", id)
		}
		if row != l.record() {
			return permanent("the anomaly text journal and the database "+
				"disagree about the content of %s", id)
		}
	}

	// The OTHER direction. A row whose `journaled_ms` is set is a row asserting
	// that the operator's fallback copy of its text exists; if the line is not
	// there, that assertion is false and nothing in the database can tell which
	// of the two is right. Deleting the JSONL -- or truncating it to a prefix --
	// is otherwise completely silent, and its consequence only appears during
	// the database outage the second journal exists for.
	states, err := s.back.anomalyJournalStates()
	if err != nil {
		return err
	}
	for _, st := range states {
		if !st.journaled {
			continue
		}
		if _, ok := byID[st.rec.AnomalyID]; !ok {
			return permanent("anomaly %s is recorded as journalled but has no "+
				"line in the text journal; the operator's fallback copy has "+
				"been truncated or removed, and the database cannot say which "+
				"of the two records is the true one", st.rec.AnomalyID)
		}
	}

	// The two legitimate crash windows, repaired.
	for _, st := range states {
		if st.journaled {
			continue
		}
		a := st.rec
		l, ok := byID[a.AnomalyID]
		if !ok {
			l = journalLine{
				AnomalyID: a.AnomalyID, RunID: a.RunID, Class: a.Class,
				Sev: a.Sev, Ticker: a.Ticker, Text: a.Text,
				FirstMs: a.FirstMs, WrittenMs: time.Now().UnixMilli(),
			}
			if err := s.jrnl.appendLine(l); err != nil {
				return fmt.Errorf("the crash window could not be closed: "+
					"anomaly %s has a row but no journalled text, and it must "+
					"not become delivery-visible until it does: %w",
					a.AnomalyID, err)
			}
		}
		if err := s.back.markJournaled(a.AnomalyID, l.WrittenMs); err != nil {
			return err
		}
	}
	return nil
}

// distinctArtifacts refuses a configuration in which the database and the text
// journal are the same file.
//
// They are two durable records of the same events precisely so that losing one
// does not lose the other. Pointed at one path they are not redundant, they are
// mutually destructive: the JSONL append corrupts the SQLite header and the
// SQLite write truncates the operator's text.
func distinctArtifacts(dbPath, logPath string) error {
	db := filepath.Clean(dbPath)
	log := filepath.Clean(logPath)
	if db == log {
		return fmt.Errorf("the database and the anomaly journal are the same "+
			"path (%s); they are two records of the same events so that losing "+
			"one does not lose the other, and pointed at one file each destroys "+
			"the other", db)
	}
	// SQLite's own sidecars belong to the database.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if log == db+suffix {
			return fmt.Errorf("the anomaly journal %s is SQLite's %s sidecar "+
				"for %s", log, suffix, db)
		}
	}
	// A hard link or a symlink pair reaches the same file by two names.
	a, errA := os.Stat(db)
	b, errB := os.Stat(log)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return fmt.Errorf("the database %s and the anomaly journal %s are two "+
			"names for one file", db, log)
	}
	return nil
}

// Reader is the read-only view. The Store owns it and closes it.
func (s *Store) Reader() *Reader { return s.rd }

// Wake signals that at least one result is ready to take.
func (s *Store) Wake() <-chan struct{} { return s.resultCh }

// TakeResults drains every completed record's outcome.
func (s *Store) TakeResults() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.results
	s.results = nil
	return out
}

// Health is the store's report on itself.
//
// It is a snapshot, and taking it can OBSERVE a stall: the writer detects a
// failed write, but a write that never returns cannot be detected by the
// goroutine blocked inside it. Nothing else here mutates, and the mutation is
// one-way -- an observed stall revokes adding and cannot grant it.
func (s *Store) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.inflight && s.monoNow()-s.inflightSince >= s.stallBound:
		s.stalled = true
		s.healthy = false
		s.adding = false
		if s.lastErr == "" {
			s.lastErr = "the writer has not completed a record within the " +
				"progress bound; nothing queued behind it is durable"
		}
		if n := len(s.queue); n > 0 {
			s.recoverAt = s.queue[n-1].seq
		}
	case !s.inflight:
		s.stalled = false
	}
	return s.healthLocked()
}

// healthLocked is the single place the reported health is decided.
//
// It is a CONJUNCTION, and each term is a different way for a record to fail to
// exist. `s.healthy` is the transient write state and is the only one that ever
// recovers; a sticky permanent fault, a writer that has gone, and a closed store
// are all terminal, because in each case there is either evidence that is
// already lost or no path by which the next record could be written.
func (s *Store) healthLocked() Health {
	ok := s.healthy && s.fault == "" && !s.writerGone && !s.closed
	last := s.lastErr
	switch {
	case s.fault != "":
		last = "a record was permanently rejected and is lost: " + s.fault
	case s.writerGone && last == "":
		last = "the store's writer has exited; nothing submitted from now on " +
			"can become durable"
	case s.closed && last == "":
		last = "the store is closed"
	}
	return Health{
		healthy:   ok,
		adding:    ok,
		stalled:   s.stalled,
		pending:   len(s.queue),
		failures:  s.failures,
		committed: s.committed,
		lastErr:   last,
	}
}

// pragmas reads back the pinned settings from the WRITE connection.
//
// Private: it is a diagnostic for this package's own tests, not a supported
// read. The write connection and not the read one, because `synchronous` and
// `foreign_keys` are per-connection and asking the query-only reader would
// report the reader's settings and prove nothing about the durability of a
// write.
func (s *Store) pragmas() (Pragmas, error) { return s.back.pragmas() }

// Close releases both journals and the read connection.
//
// It REFUSES, without changing any state, while a write is in flight or the
// FIFO is non-empty. Every queued record has already been accepted, and closing
// over the top of one converts "this store told me my anomaly was recorded" into
// nothing at all -- silently, at the one moment a process is least likely to
// look. The caller drains first or learns that it cannot.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if s.inflight {
		s.mu.Unlock()
		return errors.New("refusing to close while a record is being written: " +
			"the write is in flight and its outcome is not yet known")
	}
	if n := len(s.queue); n > 0 {
		s.mu.Unlock()
		return fmt.Errorf("refusing to close with %d accepted record(s) still "+
			"queued: each was accepted as durable-in-progress, and discarding "+
			"them here loses exactly the evidence a shutdown is most likely to "+
			"be about", n)
	}
	s.closed = true
	s.healthy = false
	s.adding = false
	s.wakeLocked()
	s.mu.Unlock()

	var errs []error
	if s.rd != nil {
		errs = append(errs, s.rd.Close())
	}
	errs = append(errs, s.jrnl.close(), s.back.close())
	return errors.Join(errs...)
}

// wakeLocked signals observers that the health or result state changed.
func (s *Store) wakeLocked() {
	select {
	case s.resultCh <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// The queue
// ---------------------------------------------------------------------------

// submit appends to the FIFO and never evicts.
//
// Every pilot record is audit-grade, so there is no low-value tier to shed: a
// store that dropped rows under pressure would drop exactly the rows describing
// the pressure. Submission takes the mutex briefly and never waits for SQLite,
// which is what lets the quote path call it. `M-HS-AUDITDROP` evicts the oldest
// waiting record while the writer is stalled.
// A sticky permanent fault does NOT refuse submission. H-STORE-3 revokes adding
// and nothing else, and the records a store with lost evidence most needs to
// keep producing are the ones describing what was lost.
func (s *Store) submit(sub *submission) (Receipt, error) {
	s.mu.Lock()
	// The refusal and the append are ONE critical section. Checked outside it,
	// a submission that raced a Close would pass the check and then be
	// appended to a queue nothing will ever drain -- accepted, and never
	// written, which is the outcome the check exists to prevent.
	if s.closed {
		s.mu.Unlock()
		return Receipt{}, errors.New("the store is closed; this record would " +
			"be queued for a writer that will never run, and an audit record " +
			"that is accepted and never written is worse than one refused")
	}
	if s.writerGone {
		s.mu.Unlock()
		return Receipt{}, errors.New("the store's writer has exited; this " +
			"record would be queued for a writer that will never run")
	}
	s.nextSeq++
	sub.seq = s.nextSeq
	sub.receipt = Receipt{seq: sub.seq, kind: sub.kind}
	s.queue = append(s.queue, sub)
	s.mu.Unlock()

	select {
	case s.workCh <- struct{}{}:
	default:
	}
	return sub.receipt, nil
}

// claimWriter enforces the single-writer rule.
//
// Two goroutines in `Run` each call `beginHead`, each receive the SAME head,
// each write it, and each `popLocked` on completion -- so the record BEHIND the
// head is removed from the queue without ever being written, and its submitter
// was told it was accepted. That is a silent audit-row loss with no error
// anywhere, and on a binding it is an order whose fills can never be classified.
func (s *Store) claimWriter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.writerGone || s.closed {
		return false
	}
	s.running = true
	return true
}

// releaseWriter latches the loss of writer ownership.
//
// Terminal, and not merely "not running": a store whose writer has returned has
// a gap in it, and a later `Run` that restored health would assert durability
// for whatever was submitted during the gap. Adding is revoked and observers are
// woken so a blocked reader re-asks rather than waiting on a result that will
// never arrive.
func (s *Store) releaseWriter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.writerGone = true
	s.healthy = false
	s.adding = false
	s.inflight = false
	s.wakeLocked()
}

// Run is the writer. It owns the only connection and the only goroutine that
// touches it, and it returns when its context is cancelled.
//
// A second concurrent call returns immediately WITHOUT touching the FIFO.
func (s *Store) Run(ctx context.Context) {
	if !s.claimWriter() {
		return
	}
	defer s.releaseWriter()

	for {
		if ctx.Err() != nil {
			return
		}
		sub := s.beginHead()
		if sub == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.workCh:
			}
			continue
		}
		err := s.apply(sub)
		if s.finish(sub, err) {
			if !s.sleep(ctx, backoffFor(sub.attempts)) {
				return
			}
		}
	}
}

// beginHead claims the head of the queue without holding the mutex across the
// write. Health() must remain answerable while a write is in flight; that is the
// only way a stall is ever observed.
func (s *Store) beginHead() *submission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		s.inflight = false
		return nil
	}
	sub := s.queue[0]
	sub.attempts++
	s.inflight = true
	s.inflightSince = s.monoNow()
	return sub
}

// finish records one attempt's outcome and reports whether to retry the head.
func (s *Store) finish(sub *submission, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = false

	if err != nil {
		s.failures++
		s.lastErr = err.Error()
		if !isPermanent(err) {
			// H-STORE-3. Adding authority is revoked and NOTHING else is: the
			// reducer, the monitor, audit production, the heartbeat and this
			// retry all continue.
			s.healthy = false
			s.adding = false
			s.stalled = false
			if n := len(s.queue); n > 0 {
				s.recoverAt = s.queue[n-1].seq
			}
			return true
		}
		// PERMANENT. The head is popped so the audit rows behind it can still
		// be written -- one caller's rule violation must not blind the whole
		// trail -- but the record is GONE, and a store with a hole in it never
		// reports healthy again. `lip-eyq` and `lip-3af` raise
		// `SEV1 STORE_RECORD_REJECTED` from the terminal Result.
		s.popLocked()
		s.fault = err.Error()
		s.healthy = false
		s.adding = false
		if sub.kind == KindBindOrder {
			s.own.failBinding(sub.bind.OrderID, err)
		}
		s.publishLocked(Result{Receipt: sub.receipt, Kind: sub.kind, Err: err})
		return false
	}

	s.popLocked()
	s.committed = sub.seq
	if !s.healthy && s.fault == "" && s.committed >= s.recoverAt {
		// The failed record and everything queued behind it at the time of the
		// failure are durable. Only now, and never across a permanent fault:
		// a later sequence crossing the watermark says nothing about the row
		// that was rejected on the way past it.
		s.healthy = true
		s.adding = true
		s.stalled = false
		s.lastErr = ""
	}

	res := Result{Receipt: sub.receipt, Kind: sub.kind}
	switch sub.kind {
	case KindBeginRun:
		res.run = RunHandle{runID: sub.run.RunID, seq: sub.seq}
	case KindReserveOrder:
		res.permit = DispatchPermit{
			coid:  sub.reserve.Coid,
			order: sub.order,
			role:  sub.role,
			store: s,
		}
	case KindBindOrder:
		s.own.commitBinding(sub.bind.OrderID, sub.bind.Coid)
	}
	s.publishLocked(res)
	return false
}

func (s *Store) popLocked() {
	if len(s.queue) == 0 {
		return
	}
	s.queue = s.queue[1:]
}

func (s *Store) publishLocked(r Result) {
	s.results = append(s.results, r)
	s.wakeLocked()
}

// apply performs one record's durable write.
func (s *Store) apply(sub *submission) error {
	switch sub.kind {
	case KindBeginRun:
		return s.back.beginRun(sub.run)
	case KindReserveOrder:
		return s.back.reserveOrder(sub.reserve)
	case KindBindOrder:
		return s.back.bindOrder(sub.bind)
	case KindFill:
		return s.back.recordFill(sub.fill)
	case KindStateEvent:
		return s.back.recordState(sub.runID, sub.state)
	case KindAnomaly:
		return s.applyAnomaly(sub)
	case KindDelivery:
		return s.back.recordDelivery(sub.delivery)
	}
	return permanent("submission of unknown kind %d reached the writer",
		sub.kind)
}

// applyAnomaly is §13.1's three steps, resumable at whichever one failed.
//
// Row first with `journaled_ms` NULL, then the synced JSONL line, then the
// update that makes it delivery-visible. A record that is visible to delivery
// before its text is durable can be pushed once and then lost in a crash, which
// is the one outcome having two journals was supposed to make impossible.
func (s *Store) applyAnomaly(sub *submission) error {
	if !sub.rowDone {
		if err := s.back.insertAnomaly(sub.anom); err != nil {
			return err
		}
		sub.rowDone = true
	}
	if !sub.journalDone {
		sub.journalMs = s.nowMs()
		err := s.jrnl.appendLine(journalLine{
			AnomalyID: sub.anom.AnomalyID,
			RunID:     sub.anom.RunID,
			Class:     sub.anom.Class,
			Sev:       sub.anom.Sev,
			Ticker:    sub.anom.Ticker,
			Text:      sub.anom.Text,
			FirstMs:   sub.anom.FirstMs,
			WrittenMs: sub.journalMs,
		})
		if err != nil {
			return err
		}
		sub.journalDone = true
	}
	return s.back.markJournaled(sub.anom.AnomalyID, sub.journalMs)
}

// ---------------------------------------------------------------------------
// Submission methods
// ---------------------------------------------------------------------------

// BeginRun writes §15's `run` row: "every parameter in §16, verbatim".
//
// The configuration is encoded with `encoding/json` directly off `cfg.Params`
// and is verified to round-trip BEFORE the record is queued. Never a `String()`
// method: those are for operators, they lose precision on every duration and
// every fixed-point quantity, and a run whose parameters cannot be reconstructed
// exactly is a run whose evidence cannot be compared with any other.
func (s *Store) BeginRun(runID string, startedMs int64,
	p cfg.Params) (Receipt, error) {

	if err := rest.ValidRunID(runID); err != nil {
		return Receipt{}, err
	}
	if startedMs <= 0 {
		return Receipt{}, errors.New("a run with no start timestamp")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return Receipt{}, fmt.Errorf("cfg.Params does not encode: %w", err)
	}
	var back cfg.Params
	if err := json.Unmarshal(raw, &back); err != nil {
		return Receipt{}, fmt.Errorf("the encoded cfg.Params does not "+
			"decode: %w", err)
	}
	if !reflect.DeepEqual(back, p) {
		return Receipt{}, fmt.Errorf("cfg.Params does not round-trip through "+
			"JSON: stored %+v would read back as %+v. §15 records the "+
			"parameters verbatim, and a lossy encoding makes every run's "+
			"evidence uninterpretable in a way no later read can detect", p, back)
	}
	return s.submit(&submission{
		kind: KindBeginRun,
		run: runRecord{
			RunID: runID, StartedMs: startedMs, Params: p, ConfigJSON: raw,
		},
	})
}

// RecordFill writes one `our_fill` row. First observer wins (H-ORD-6).
func (s *Store) RecordFill(h RunHandle, f risk.FillEvent, firstSeenMs int64,
	backfilled bool) (Receipt, error) {

	rec, err := newFillRecord(h, f, firstSeenMs, backfilled)
	if err != nil {
		return Receipt{}, err
	}
	return s.submit(&submission{kind: KindFill, fill: rec})
}

// RecordGlobalState queues one §5.1 transition (A9).
func (s *Store) RecordGlobalState(h RunHandle, eventID string, tsMs int64,
	from, to quote.GlobalState, trigger quote.GlobalTrigger) (Receipt, error) {

	if !h.Valid() {
		return Receipt{}, errors.New("no run handle: a state_event row " +
			"references run(run_id)")
	}
	ev, err := NewGlobalStateEvent(eventID, tsMs, from, to, trigger)
	if err != nil {
		return Receipt{}, err
	}
	return s.submit(&submission{
		kind: KindStateEvent, runID: h.runID, state: ev,
	})
}

// RecordMarketState queues one §5.2 transition (A9).
func (s *Store) RecordMarketState(h RunHandle, eventID string, tsMs int64,
	ticker string, from, to quote.MarketState,
	trigger quote.MarketTrigger) (Receipt, error) {

	if !h.Valid() {
		return Receipt{}, errors.New("no run handle: a state_event row " +
			"references run(run_id)")
	}
	ev, err := NewMarketStateEvent(eventID, tsMs, ticker, from, to, trigger)
	if err != nil {
		return Receipt{}, err
	}
	return s.submit(&submission{
		kind: KindStateEvent, runID: h.runID, state: ev,
	})
}

// RecordAnomaly queues one §13.1 anomaly for BOTH journals.
//
// It is accepted while the store is unhealthy, deliberately. H-STORE-3 revokes
// adding and nothing else, and the records a broken store most needs to keep
// producing are the ones describing why it broke.
func (s *Store) RecordAnomaly(h RunHandle, anomalyID string, a risk.Anomaly,
	firstMs int64) (Receipt, error) {

	rec, err := newAnomalyRecord(h, anomalyID, a, firstMs)
	if err != nil {
		return Receipt{}, err
	}
	return s.submit(&submission{kind: KindAnomaly, anom: rec})
}

// RecordDeliveryAttempt folds one push's outcome into every row it represented.
//
// The ping service goes through here and never through SQL. There is one writer
// and one connection, and a delivery update issued around them would be the
// second writer this package exists to make impossible.
func (s *Store) RecordDeliveryAttempt(d DeliveryAttempt) (Receipt, error) {
	if err := d.validate(); err != nil {
		return Receipt{}, err
	}
	ids := append([]string(nil), d.AnomalyIDs...)
	d.AnomalyIDs = ids
	return s.submit(&submission{kind: KindDelivery, delivery: d})
}

// confidence: high

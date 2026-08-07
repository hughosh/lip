package ping

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"lip/harness/hstore"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// §13.2 — the rate limits
// ---------------------------------------------------------------------------

const (
	// sev1DedupMs is SEV1's deduplication window. SEV1 does NOT go through the
	// fifteen-minute bucket -- "risk state is wrong or unmanaged, act now" is
	// not something to be told about at the top of the hour -- but a condition
	// that re-fires every second must not empty the operator's battery either.
	// Five minutes is short enough that a persistent SEV1 keeps arriving.
	sev1DedupMs = 5 * 60 * 1000

	// sev2BucketMs is §13.2's one push per (class,ticker) per fifteen minutes.
	sev2BucketMs = 15 * 60 * 1000

	// healthPushDedupMs is §13.3's five-minute grant applied to the storage
	// health notice, which is the one urgent push that was outside §13.2's
	// buckets entirely.
	//
	// A disk that fails, is retried, succeeds and fails again produces a
	// healthy->unhealthy transition per repetition, and this notice is URGENT
	// -- the priority that overrides a silenced phone. One story about one disk
	// arriving twenty times is how an operator comes to mute the channel, and
	// muting it mutes the SEV1 alerts with it.
	//
	// It DEFERS and never drops. A transition inside the window stays owed and
	// is delivered when the window ends, so the operator's last word on
	// persistence is never staler than five minutes behind the truth. Recovery
	// clears what is owed, because a store that is healthy again has nothing
	// outstanding to say.
	healthPushDedupMs = 5 * 60 * 1000
)

// deliveryBackoff is the PRIVATE 1-60 second retry ladder for a failed push.
var deliveryBackoff = [...]time.Duration{
	1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
	20 * time.Second, 40 * time.Second, 60 * time.Second,
}

func deliveryBackoffMs(attempts int) int64 {
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(deliveryBackoff) {
		i = len(deliveryBackoff) - 1
	}
	return deliveryBackoff[i].Milliseconds()
}

// ---------------------------------------------------------------------------
// Effects
// ---------------------------------------------------------------------------

// PushKind distinguishes the three things this service sends.
type PushKind uint8

const (
	// PushAnomaly is one §13.2 alert, possibly aggregating suppressed
	// occurrences of the same (class,ticker).
	PushAnomaly PushKind = iota
	// PushHeartbeat is §13.3's scheduled heartbeat, which also carries the
	// SEV3 batch.
	PushHeartbeat
	// PushHealth is the immediate urgent notice sent on the healthy->unhealthy
	// storage transition. It is STATUS TELEMETRY and never the delivery of an
	// unjournalled anomaly: the anomaly whose journalling failed is precisely
	// the record that is not yet safe to deliver.
	PushHealth
)

func (k PushKind) String() string {
	switch k {
	case PushHeartbeat:
		return "heartbeat"
	case PushHealth:
		return "health"
	}
	return "anomaly"
}

// Push is one attempted notification.
type Push struct {
	Kind     PushKind
	Title    string
	Body     string
	Priority string
	// AnomalyIDs is every row this push represented, oldest first.
	AnomalyIDs []string
	// Suppressed is how many occurrences beyond the representative this push
	// carried.
	Suppressed int
	// Err is why it was not delivered. The rows stay pending.
	Err error
}

// Effects is what one Step did.
type Effects struct {
	Pushes []Push
	// Heartbeat is true when a scheduled heartbeat was due and attempted.
	Heartbeat bool
	// Deadman is true when the external check-in succeeded.
	Deadman bool
	// DeadmanErr is why it did not. It is reported, never fatal.
	DeadmanErr error
	// Err is a failure to READ the pending queue, which means this Step could
	// not know what was owed.
	Err error
	// NextHeartbeatMs is the NOMINAL heartbeat schedule: when §13.3's next
	// scheduled beat falls due, ignoring every retry.
	NextHeartbeatMs int64
	// NextStepMs is when this service must be stepped again, and it is the whole
	// of the scheduling contract.
	//
	// `Step` is deterministic and owns no clock, so every deadline it computes
	// is worthless unless it is published: a private 1-60 second retry ladder
	// whose caller only knows about the hourly heartbeat is a ladder that
	// resolves to an hour. It is the EARLIEST of the scheduled heartbeat, every
	// failed alert's next attempt, every rate-limit bucket's reopening, the
	// failed health, heartbeat and dead-man retries, and the one-second health
	// poll that is the only way a blocked writer is ever observed.
	NextStepMs int64
}

// earliest folds a candidate deadline into the running minimum. A zero or
// past-due candidate is ignored: Step has already acted on anything due now.
func earliest(cur, candidate, nowMs int64) int64 {
	if candidate <= nowMs {
		return cur
	}
	if cur == 0 || candidate < cur {
		return candidate
	}
	return cur
}

// ---------------------------------------------------------------------------
// The service
// ---------------------------------------------------------------------------

// Service is §13's delivery policy, and it is PURE with respect to time.
type Service struct {
	reader   *hstore.Reader
	store    *hstore.Store
	ntfy     *NTFYSender
	dead     *HTTPSDeadman
	interval time.Duration

	lastHeartbeatMs int64
	lastSev1        map[string]int64
	lastSev2        map[string]int64
	retryAt         map[string]int64

	healthSeen  bool
	lastHealthy bool

	// The three STATUS operations have their own retry state. A failed
	// heartbeat that waits for the next hourly slot takes the dead-man check-in
	// with it, so the external watchdog alarms on a harness that is alive; a
	// failed health notice that is never retried means the operator is never
	// told that persistence went away.
	healthOwed     bool
	healthAttempts int
	healthRetryAt  int64
	// lastHealthPushMs is when a health notice was last SENT, and it is the
	// left edge of `healthPushDedupMs`. Zero means never, so the first notice
	// of a run is never delayed by a window that has not started.
	lastHealthPushMs int64
	beatAttempts     int
	beatRetryAt      int64
	deadAttempts     int
	deadRetryAt      int64
}

// NewService requires every collaborator, including the dead man.
//
// §13.4's check-in is the only detector of F18 -- "the harness stopped and
// nobody noticed" -- that survives this process dying, and a nil-tolerant
// constructor would degrade to no detector at all in exactly the deployment that
// forgot to configure one. There is deliberately no production no-op, and
// `M-P-DEADMAN` accepts one to show what it costs.
func NewService(reader *hstore.Reader, store *hstore.Store, sender *NTFYSender,
	dead *HTTPSDeadman, interval time.Duration) (*Service, error) {

	if reader == nil {
		return nil, errors.New("no store reader: §13's pending queue IS the " +
			"anomaly table, and a service without it would keep its own list " +
			"that no restart could recover")
	}
	if store == nil {
		return nil, errors.New("no store: delivery state is written through " +
			"the store's single writer and never by direct SQL")
	}
	// `!sender.valid()` and not `sender == nil`. Both types are constructible
	// as zero values from any package, so a nil check refuses only the honest
	// mistake and admits the one that panics inside the alert loop.
	if !sender.valid() {
		return nil, errors.New("no usable ntfy sender: a zero-value sender " +
			"addresses no channel, so every alert would report success and " +
			"reach nobody")
	}
	if !dead.valid() {
		return nil, errors.New("no dead-man endpoint: §13.4 requires a " +
			"concrete external check-in, and a harness that cannot be missed " +
			"by anything outside itself is unattended in the only sense that " +
			"matters")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("heartbeat interval %v must be positive; the "+
			"heartbeat is the dead man's switch and F18 is detected by its "+
			"absence", interval)
	}
	return &Service{
		reader: reader, store: store, ntfy: sender, dead: dead,
		interval: interval,
		lastSev1: make(map[string]int64),
		lastSev2: make(map[string]int64),
		retryAt:  make(map[string]int64),
	}, nil
}

// Step performs one pass of §13's delivery policy at the caller's clock.
//
// It reads no clock and starts no goroutine: `lip-3af` owns both. That is what
// makes a fifteen-minute rate limit assertable in a test that never sleeps.
func (s *Service) Step(ctx context.Context, nowMs int64, hb Heartbeat) Effects {
	var eff Effects

	health := s.store.Health()
	hb.StoreHealthy = health.Healthy()
	if !health.Healthy() && hb.StoreDetail == "" {
		hb.StoreDetail = health.LastError()
	}

	// --- the urgent health notice ------------------------------------------
	//
	// Owed on the healthy->unhealthy transition AND on a FIRST observation that
	// is already unhealthy. The second case is not an edge: a harness that comes
	// up with its database locked or its disk full has never been healthy, so a
	// notice that requires a prior healthy observation is a notice that never
	// fires for the deployment that was broken from the start.
	//
	// It is telemetry rather than an alert delivery: the anomaly that could not
	// be journalled must NOT be pushed, because a record delivered before it is
	// durable can be delivered once and then lost. This says only that
	// persistence is unavailable.
	if health.Healthy() {
		s.healthOwed = false
		s.healthAttempts = 0
		s.healthRetryAt = 0
	} else if !s.healthSeen || s.lastHealthy {
		s.healthOwed = true
		s.healthAttempts = 0
		s.healthRetryAt = 0
	}
	s.healthSeen = true
	s.lastHealthy = health.Healthy()

	if s.healthOwed && nowMs >= s.healthPushDueMs() {
		p := s.pushHealth(ctx, hb)
		eff.Pushes = append(eff.Pushes, p)
		if p.Err == nil {
			s.healthOwed = false
			s.healthAttempts = 0
			s.healthRetryAt = 0
			s.lastHealthPushMs = nowMs
		} else {
			s.healthAttempts++
			s.healthRetryAt = nowMs + deliveryBackoffMs(s.healthAttempts)
		}
	}

	rows, err := s.reader.PendingAnomalies()
	var nextAlert int64
	if err != nil {
		eff.Err = fmt.Errorf("the pending-alert queue could not be read, so "+
			"this step does not know what is owed: %w", err)
		// The queue could not be read, so the only honest answer is "ask again
		// soon" rather than "nothing is owed until the next heartbeat".
		nextAlert = nowMs + deliveryBackoffMs(1)
	} else {
		pushes, next := s.drain(ctx, nowMs, rows)
		eff.Pushes = append(eff.Pushes, pushes...)
		nextAlert = next
	}

	// --- §13.3's scheduled heartbeat ---------------------------------------
	//
	// Due on the interval, in EVERY global state and whatever the store is
	// doing. A heartbeat suppressed while things are wrong is a heartbeat that
	// goes quiet exactly when its absence would be read as F18 -- and F18 is
	// what the dead man is watching for. `M-P-HEALTHBEAT` adds the condition.
	due := s.lastHeartbeatMs == 0 || nowMs-s.lastHeartbeatMs >= s.interval.Milliseconds()
	retryBeat := s.beatAttempts > 0 && nowMs >= s.beatRetryAt
	if due || retryBeat {
		eff.Heartbeat = true
		p := s.pushHeartbeat(ctx, nowMs, hb, rows)
		eff.Pushes = append(eff.Pushes, p)
		if p.Err == nil {
			s.beatAttempts = 0
			s.beatRetryAt = 0
		} else {
			s.beatAttempts++
			s.beatRetryAt = nowMs + deliveryBackoffMs(s.beatAttempts)
		}
		if due {
			// The NOMINAL schedule advances whether or not the push landed. A
			// failed beat is retried on the ladder; it does not push the hourly
			// cadence out by an hour every time the network hiccups.
			s.lastHeartbeatMs = nowMs
		}
	}

	// Every scheduled heartbeat checks in, and a FAILED check-in retries on the
	// ladder. Coupling the two is deliberate -- a dead man pinged on its own
	// schedule keeps reporting a harness alive whose heartbeat has stopped --
	// but a check-in that failed for a transient reason must not then wait an
	// hour, because that is exactly long enough for the watchdog to alarm.
	if eff.Heartbeat || (s.deadAttempts > 0 && nowMs >= s.deadRetryAt) {
		if err := s.dead.CheckIn(ctx); err != nil {
			eff.DeadmanErr = err
			s.deadAttempts++
			s.deadRetryAt = nowMs + deliveryBackoffMs(s.deadAttempts)
		} else {
			eff.Deadman = true
			s.deadAttempts = 0
			s.deadRetryAt = 0
		}
	}

	eff.NextHeartbeatMs = s.lastHeartbeatMs + s.interval.Milliseconds()

	next := earliest(0, eff.NextHeartbeatMs, nowMs)
	next = earliest(next, nextAlert, nowMs)
	if s.healthOwed {
		next = earliest(next, s.healthPushDueMs(), nowMs)
	}
	if s.beatAttempts > 0 {
		next = earliest(next, s.beatRetryAt, nowMs)
	}
	if s.deadAttempts > 0 {
		next = earliest(next, s.deadRetryAt, nowMs)
	}
	// The health poll. A write that never returns cannot be observed by the
	// goroutine blocked inside it, so the stall is observed by whoever asks --
	// and nobody asks unless a deadline says to. While the store has work in
	// flight, or is already unhealthy and may recover, that deadline is one
	// second away.
	//
	// Re-snapshotted, and that is the whole point of the line. This Step has
	// been SUBMITTING since the entry snapshot was taken -- every delivery
	// attempt `drain` and `pushHeartbeat` recorded went through the store's
	// writer -- so the entry reading answers "was anything in flight before I
	// started", which is not the question. A Step that pushed an alert and
	// queued its delivery record against a wedged writer would publish the
	// hourly heartbeat as the next deadline and leave the wedge unobserved for
	// an hour.
	health = s.store.Health()
	if health.Pending() > 0 || !health.Healthy() {
		next = earliest(next, nowMs+hstore.HealthPollInterval.Milliseconds(),
			nowMs)
	}
	eff.NextStepMs = next
	return eff
}

// healthPushDueMs is the earliest this Step may send the owed health notice.
//
// The LATER of two independent bounds: the 1-60 second ladder's next rung after
// a FAILED notice, and the end of `healthPushDedupMs` measured from the last
// one that was SENT. They answer different questions -- "when may this delivery
// be retried" and "when may the operator be told again" -- and taking the
// maximum is what stops a flapping store re-sending on the back of a retry that
// the ladder happened to make due first.
func (s *Service) healthPushDueMs() int64 {
	due := s.healthRetryAt
	if s.lastHealthPushMs > 0 {
		if window := s.lastHealthPushMs + healthPushDedupMs; window > due {
			due = window
		}
	}
	return due
}

// group is one (severity, class, ticker) bucket's due rows, oldest first.
type group struct {
	sev    risk.Severity
	class  string
	ticker string
	rows   []hstore.AnomalyRow
}

func (g *group) key() string {
	return fmt.Sprintf("%d|%s|%s", g.sev, g.class, g.ticker)
}

// drain sends every eligible SEV1 and SEV2 bucket, SEV1 first and oldest first.
//
// Recovery falls out of this rather than being a separate path: after a restart
// the pending queue is whatever the table holds, in `first_ms` order, from every
// run, and it is drained by the same code that handles the steady state. There
// is no "catch-up mode" to be wrong about, and every push carries the ORIGINAL
// timestamps -- an alert about something that happened four hours ago must not
// read as having just happened.
func (s *Service) drain(ctx context.Context, nowMs int64,
	rows []hstore.AnomalyRow) ([]Push, int64) {

	var next int64
	var order []*group
	byKey := make(map[string]*group)
	for _, r := range rows {
		if r.Sev == risk.SEV3 {
			// §13.2: SEV3, including STARTUP and drain-complete notices, is
			// batched into the next heartbeat and never pushed on its own.
			continue
		}
		if at := s.dueAt(r); nowMs < at {
			next = earliest(next, at, nowMs)
			continue
		}
		g := &group{sev: r.Sev, class: r.Class, ticker: r.Ticker}
		k := g.key()
		if existing, ok := byKey[k]; ok {
			existing.rows = append(existing.rows, r)
			continue
		}
		g.rows = append(g.rows, r)
		byKey[k] = g
		order = append(order, g)
	}
	// `rows` arrives oldest first, so group creation order is already oldest
	// first. A STABLE sort by severity therefore yields "due SEV1 oldest first,
	// then everything else oldest first".
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].sev > order[j].sev
	})

	var out []Push
	for _, g := range order {
		window := int64(sev2BucketMs)
		if g.sev == risk.SEV1 {
			window = sev1DedupMs
		}
		last, seen := s.bucket(g)
		if seen && nowMs-last < window {
			// Suppressed. The rows REMAIN pending: §13.2 suppresses the push,
			// not the record, and the next open bucket delivers them as one
			// aggregate carrying their count. When that bucket reopens is a
			// deadline the caller has to be told, or the aggregate waits for
			// whatever unrelated event happens to trigger the next Step.
			next = earliest(next, last+window, nowMs)
			continue
		}
		p := s.pushGroup(ctx, nowMs, g)
		out = append(out, p)
		if p.Err != nil {
			for _, id := range p.AnomalyIDs {
				next = earliest(next, s.retryAt[id], nowMs)
			}
		}
	}
	return out, next
}

// dueAt is when this row may next be attempted.
//
// It prefers this process's own memory and falls back to the PERSISTED
// `attempts` and `last_attempt_ms`. The fallback is what makes the ladder
// survive a restart: without it a process that comes up holding a row already
// attempted six times treats it as brand new, retries immediately, and -- if
// whatever broke delivery is still broken -- hammers the channel at every Step
// with no backoff at all.
func (s *Service) dueAt(r hstore.AnomalyRow) int64 {
	if at, held := s.retryAt[r.AnomalyID]; held {
		return at
	}
	if r.Attempts > 0 && r.LastAttemptMs > 0 {
		return r.LastAttemptMs + deliveryBackoffMs(r.Attempts)
	}
	return 0
}

func (s *Service) bucket(g *group) (int64, bool) {
	if g.sev == risk.SEV1 {
		v, ok := s.lastSev1[g.key()]
		return v, ok
	}
	v, ok := s.lastSev2[g.key()]
	return v, ok
}

func (s *Service) markBucket(g *group, nowMs int64) {
	if g.sev == risk.SEV1 {
		s.lastSev1[g.key()] = nowMs
		return
	}
	s.lastSev2[g.key()] = nowMs
}

// pushGroup delivers one bucket and records the attempt through the store.
func (s *Service) pushGroup(ctx context.Context, nowMs int64, g *group) Push {
	rep := g.rows[0]
	suppressed := len(g.rows) - 1

	priority := PriorityDefault
	if g.sev == risk.SEV1 {
		priority = PriorityUrgent
	}
	where := rep.Ticker
	if where == "" {
		where = "account"
	}
	title := fmt.Sprintf("%s %s %s", rep.Sev, rep.Class, where)

	body := fmt.Sprintf("%s\n\nfirst seen: %s\nanomaly: %s",
		rep.Text, stamp(rep.FirstMs), rep.AnomalyID)
	if suppressed > 0 {
		body += fmt.Sprintf("\n\n%d further occurrence(s) of %s on %s between "+
			"%s and %s were rate limited by §13.2 and are delivered by this "+
			"one push.", suppressed, rep.Class, where,
			stamp(rep.FirstMs), stamp(g.rows[len(g.rows)-1].FirstMs))
	}

	ids := make([]string, len(g.rows))
	for i, r := range g.rows {
		ids[i] = r.AnomalyID
	}

	err := s.ntfy.Send(ctx, Message{Title: title, Body: body,
		Priority: priority})
	s.record(ids, nowMs, err == nil)

	if err == nil {
		s.markBucket(g, nowMs)
		for _, id := range ids {
			delete(s.retryAt, id)
		}
	} else {
		for _, r := range g.rows {
			s.retryAt[r.AnomalyID] = nowMs + deliveryBackoffMs(r.Attempts+1)
		}
	}
	return Push{
		Kind: PushAnomaly, Title: title, Body: body, Priority: priority,
		AnomalyIDs: ids, Suppressed: suppressed, Err: err,
	}
}

// pushHeartbeat sends §13.3 and carries the SEV3 batch with it.
func (s *Service) pushHeartbeat(ctx context.Context, nowMs int64, hb Heartbeat,
	rows []hstore.AnomalyRow) Push {

	var ids []string
	var extra []string
	for _, r := range rows {
		if r.Sev != risk.SEV3 {
			continue
		}
		ids = append(ids, r.AnomalyID)
		where := r.Ticker
		if where == "" {
			where = "account"
		}
		extra = append(extra, fmt.Sprintf("  [SEV3 %s %s @ %s] %s",
			r.Class, where, stamp(r.FirstMs), r.Text))
	}
	if len(extra) > 0 {
		extra = append([]string{"notices:"}, extra...)
	}

	title, body := hb.Render(extra)
	err := s.ntfy.Send(ctx, Message{Title: title, Body: body,
		Priority: PriorityDefault})
	if len(ids) > 0 {
		s.record(ids, nowMs, err == nil)
	}
	return Push{
		Kind: PushHeartbeat, Title: title, Body: body,
		Priority: PriorityDefault, AnomalyIDs: ids, Err: err,
	}
}

// pushHealth is the immediate urgent notice on the storage transition.
func (s *Service) pushHealth(ctx context.Context, hb Heartbeat) Push {
	title := "lip STORE UNHEALTHY | persistence unavailable"
	_, body := hb.Render([]string{
		"",
		"This is a health notice, not an alert delivery: an anomaly whose " +
			"journalling failed is not yet safe to send, and it will be sent " +
			"once both journals hold it.",
	})
	err := s.ntfy.Send(ctx, Message{Title: title, Body: body,
		Priority: PriorityUrgent})
	return Push{
		Kind: PushHealth, Title: title, Body: body,
		Priority: PriorityUrgent, Err: err,
	}
}

// record writes the delivery outcome through the store's writer.
//
// Never direct SQL. There is one writer and one connection, and a delivery
// update issued around them is the second writer `hstore` exists to prevent --
// with the added property that it would race the very rows it is marking.
//
// A REFUSAL is deliberately unhandled, and the contract is worth stating
// because the obvious handling is worse than none. Submission is refused only
// from a terminal store -- closed, or a writer that has exited -- since a
// transient fault queues and retries. So the rows were never marked delivered,
// they remain durably pending in the anomaly table, and the next open §13.2
// bucket re-offers them at the ordinary cadence with no help from this
// function. A double delivery is the accepted direction; a silently dropped
// alert is not.
//
// Scheduling a retry HERE would be strictly harmful: `dueAt` prefers this
// process's own memory over the table, so a remembered deadline can only
// postpone the redelivery the pending row already guarantees. There is also
// nothing to escalate to -- this IS the escalation path -- and the refusal
// reaches the operator by the one route that still works: a store that refuses
// records reports unhealthy, and the urgent health notice says so.
func (s *Service) record(ids []string, nowMs int64, delivered bool) {
	att := hstore.DeliveryAttempt{
		AnomalyIDs: ids,
		AttemptMs:  nowMs,
		Delivered:  delivered,
	}
	if delivered {
		att.DeliveredMs = nowMs
	}
	// Both returns are discarded on purpose: there is no receipt to await and
	// no refusal to act on. The paragraph above is the handling.
	s.store.RecordDeliveryAttempt(att)
}

// stamp renders an original timestamp in UTC. Recovery pushes carry the time the
// event happened, never the time it was finally delivered.
func stamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

// confidence: high

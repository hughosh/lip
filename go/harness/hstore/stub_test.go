package hstore

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// Real stores under t.TempDir()
// ---------------------------------------------------------------------------
//
// Every test here uses REAL SQLite. The mutations this package is judged on are
// about pragmas, constraint clauses and commit ordering, and a fake store would
// pass all of them. `lip/rig.db` and every other database in the tree is off
// limits; `t.TempDir()` is the only place a test database exists.

// Short names for the enum values the state-event tests use.
const (
	globalStarting    = quote.Starting
	globalRunning     = quote.Running
	marketIdle        = quote.Idle
	marketQuoting     = quote.Quoting
	triggerReconciled = quote.GTReconciled
	triggerSelected   = quote.MTSelected
	triggerGlobalNone = quote.GTNone
	triggerMarketNone = quote.MTNone
)

// paths returns a fresh database and journal path pair.
func paths(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "harness.db"), filepath.Join(dir, "anomaly.jsonl")
}

func openAt(t *testing.T, dbPath, logPath string) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// tempStore is a store whose files vanish with the test.
func tempStore(t *testing.T) *Store {
	t.Helper()
	db, log := paths(t)
	s := openAt(t, db, log)
	t.Cleanup(func() { s.Close() })
	return s
}

// pump runs the writer to a standstill IN THE CALLING GOROUTINE.
//
// It drives the real `beginHead`/`apply`/`finish` cycle rather than a
// simplification of it, so the commit ordering and the health transitions under
// test are the production ones. It stops at the first transient failure instead
// of retrying, because a test that waited out a retry ladder would be a test
// about `time.Sleep`.
func pump(t *testing.T, s *Store) []Result {
	t.Helper()
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatalf("the writer did not reach a standstill in %d records", i)
		}
		sub := s.beginHead()
		if sub == nil {
			break
		}
		if s.finish(sub, s.apply(sub)) {
			break
		}
	}
	return s.TakeResults()
}

// begin submits a run row, drains it, and returns the durable licence.
func begin(t *testing.T, s *Store, runID string, startedMs int64) RunHandle {
	t.Helper()
	if _, err := s.BeginRun(runID, startedMs, cfg.Default()); err != nil {
		t.Fatalf("BeginRun(%s): %v", runID, err)
	}
	for _, r := range pump(t, s) {
		if h, ok := r.RunHandle(); ok {
			return h
		}
	}
	t.Fatalf("BeginRun(%s) produced no run handle", runID)
	return RunHandle{}
}

// order builds a validated §7.1 intent.
func order(t *testing.T, runID string, seq uint64, side quote.Side) rest.CreateOrder {
	t.Helper()
	coid, err := rest.Coid(runID, 0, side, seq)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}
	o, err := rest.NewCreateOrder("KXTEST-A", side, 50, num.QtyFromFloat(3),
		num.QtyFromFloat(12), coid)
	if err != nil {
		t.Fatalf("NewCreateOrder: %v", err)
	}
	return o
}

// reserve submits a reservation, drains it, and returns the committed permit.
func reserve(t *testing.T, s *Store, h RunHandle, o rest.CreateOrder,
	role quote.Role, ms int64) DispatchPermit {

	t.Helper()
	if _, err := s.ReserveOrder(h, o, role, ms); err != nil {
		t.Fatalf("ReserveOrder: %v", err)
	}
	for _, r := range pump(t, s) {
		if p, ok := r.Permit(); ok {
			return p
		}
	}
	t.Fatalf("ReserveOrder produced no dispatch permit")
	return DispatchPermit{}
}

// bind submits and drains a coid -> order-id binding.
func bind(t *testing.T, s *Store, coid, orderID string, ms int64) []Result {
	t.Helper()
	if _, err := s.BindOrder(coid, orderID, ms); err != nil {
		t.Fatalf("BindOrder: %v", err)
	}
	return pump(t, s)
}

func fill(tradeID, orderID string) risk.FillEvent {
	return risk.FillEvent{
		TradeID:      tradeID,
		OrderID:      orderID,
		Ticker:       "KXTEST-A",
		Side:         quote.SideYes,
		Price4:       5000,
		Count:        num.QtyFromFloat(3),
		Fee:          0,
		IsTaker:      false,
		ExchangeTsMs: 1_700_000_000_000,
	}
}

// ---------------------------------------------------------------------------
// Injected failure
// ---------------------------------------------------------------------------

// control makes the injected backend and journal fail or block on demand.
//
// The two failures this store is judged on -- a write that never returns, and a
// disk that will not take a byte -- cannot be produced on demand from a real
// file, and a test that tried would be a test about the machine. Blocking is a
// handshake and never a sleep: the writer signals `entered` from inside the
// blocked call, so the assertions run at a known point.
type control struct {
	mu      sync.Mutex
	backErr error
	jrnlErr error
	blockOn bool

	release chan struct{}
	entered chan struct{}
}

func newControl() *control {
	return &control{
		release: make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
}

func (c *control) setBackErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backErr = err
}

func (c *control) setJrnlErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jrnlErr = err
}

func (c *control) block() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockOn = true
}

func (c *control) unblock() {
	c.mu.Lock()
	c.blockOn = false
	c.mu.Unlock()
	close(c.release)
}

func (c *control) backGate() error {
	c.mu.Lock()
	err, blocked := c.backErr, c.blockOn
	c.mu.Unlock()
	if blocked {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.release
	}
	return err
}

func (c *control) jrnlGate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jrnlErr
}

// gatedBackend delegates to a real SQLite backend behind the control.
type gatedBackend struct {
	backend
	c *control
}

func (g *gatedBackend) beginRun(r runRecord) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.beginRun(r)
}

func (g *gatedBackend) reserveOrder(r orderReservation) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.reserveOrder(r)
}

func (g *gatedBackend) bindOrder(b orderBinding) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.bindOrder(b)
}

func (g *gatedBackend) recordFill(f fillRecord) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.recordFill(f)
}

func (g *gatedBackend) recordState(runID string, ev StateEvent) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.recordState(runID, ev)
}

func (g *gatedBackend) insertAnomaly(a anomalyRecord) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.insertAnomaly(a)
}

func (g *gatedBackend) markJournaled(id string, ms int64) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.markJournaled(id, ms)
}

func (g *gatedBackend) recordDelivery(d DeliveryAttempt) error {
	if err := g.c.backGate(); err != nil {
		return err
	}
	return g.backend.recordDelivery(d)
}

// gatedJournal delegates to a real JSONL journal behind the control.
type gatedJournal struct {
	journal
	c *control
}

func (g *gatedJournal) appendLine(l journalLine) error {
	if err := g.c.jrnlGate(); err != nil {
		return err
	}
	return g.journal.appendLine(l)
}

// testClock is an injected clock with BOTH sources: wall milliseconds for
// records and an independent monotonic elapsed reading for the progress bound.
//
// They are separate fields on purpose. A test that advanced one and read the
// other would be the defect `M-HS-STALLCLOCK` exists to catch, dressed as a
// fixture. Stall detection needs time to pass while a write is blocked, and the
// one thing a test must not do to produce that is wait.
type testClock struct {
	mu   sync.Mutex
	ms   int64
	mono time.Duration
}

func newClock() *testClock { return &testClock{ms: 1_700_000_000_000} }

func (c *testClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *testClock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// advance moves both sources together, as a healthy host does.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += d.Milliseconds()
	c.mono += d
}

// advanceWallOnly moves the WALL clock and leaves the monotonic reading where
// it is -- an NTP step forward. Nothing has actually taken any time.
func (c *testClock) advanceWallOnly(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += d.Milliseconds()
}

// gatedStore is a real store whose backend and journal can be made to fail or
// block, on an injected clock.
func gatedStore(t *testing.T) (*Store, *control, *testClock) {
	t.Helper()
	dbPath, logPath := paths(t)

	back, err := openSQLite(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	jrnl, err := openJournal(logPath)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	rd, err := openReader(dbPath)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	c := newControl()
	clk := newClock()
	s, err := openWith(&gatedBackend{backend: back, c: c},
		&gatedJournal{journal: jrnl, c: c}, rd)
	if err != nil {
		t.Fatalf("openWith: %v", err)
	}
	s.nowMs = clk.now
	s.monoNow = clk.elapsed
	t.Cleanup(func() { s.Close() })
	return s, c, clk
}

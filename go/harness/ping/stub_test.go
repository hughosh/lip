package ping

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"lip/harness/cfg"
	"lip/harness/hstore"
	"lip/harness/quote"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// A real store, a real writer, and two real HTTP servers
// ---------------------------------------------------------------------------
//
// The pending-alert queue IS the anomaly table, so these tests use a real
// `hstore` under `t.TempDir()` with its real writer goroutine. A fake queue
// would pass every restart assertion here without the property holding.

type fixture struct {
	t       *testing.T
	dbPath  string
	logPath string
	store   *hstore.Store
	run     hstore.RunHandle
	cancel  context.CancelFunc
	results map[uint64]hstore.Result
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		t:       t,
		dbPath:  filepath.Join(dir, "harness.db"),
		logPath: filepath.Join(dir, "anomaly.jsonl"),
	}
	f.open()
	f.beginRun("runa", 1_700_000_000_000)
	t.Cleanup(f.close)
	return f
}

func (f *fixture) open() {
	f.t.Helper()
	s, err := hstore.Open(hstore.StoreConfig{
		DBPath: f.dbPath, AnomalyLogPath: f.logPath})
	if err != nil {
		f.t.Fatalf("open store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	// Sequence numbers restart with each Store, so the result map does too.
	f.store, f.cancel, f.results = s, cancel, make(map[uint64]hstore.Result)
}

func (f *fixture) close() {
	if f.cancel != nil {
		f.cancel()
	}
	if f.store != nil {
		f.store.Close()
	}
}

// restart is a process death and a fresh start against the same files.
func (f *fixture) restart(runID string, startedMs int64) {
	f.t.Helper()
	f.close()
	f.open()
	f.beginRun(runID, startedMs)
}

// drain takes whatever the writer has finished and keeps it by sequence, so
// one helper waiting on its own record cannot swallow another's.
func (f *fixture) drain() {
	f.t.Helper()
	select {
	case <-f.store.Wake():
	default:
	}
	for _, r := range f.store.TakeResults() {
		if !r.OK() {
			f.t.Fatalf("record %v failed: %v", r.Kind, r.Err)
		}
		f.results[r.Receipt.Seq()] = r
	}
}

// settle waits until the queue is empty, which is exactly "every submitted
// record is durable". It yields rather than sleeps; the bound exists so a
// mutation that wedges the writer fails fast instead of hanging the package.
func (f *fixture) settle() {
	f.t.Helper()
	for i := 0; i < 200_000; i++ {
		f.drain()
		if f.store.Health().Pending() == 0 {
			f.drain()
			return
		}
		runtime.Gosched()
	}
	f.t.Fatalf("the store never drained: %+v", f.store.Health())
}

func (f *fixture) beginRun(runID string, startedMs int64) {
	f.t.Helper()
	rcpt, err := f.store.BeginRun(runID, startedMs, cfg.Default())
	if err != nil {
		f.t.Fatalf("BeginRun: %v", err)
	}
	f.settle()
	h, ok := f.results[rcpt.Seq()].RunHandle()
	if !ok {
		f.t.Fatal("BeginRun produced no run handle")
	}
	f.run = h
}

// anomaly records one anomaly and waits for BOTH journals to hold it.
func (f *fixture) anomaly(id, class string, sev risk.Severity, ticker string,
	firstMs int64) {

	f.t.Helper()
	f.anomalyAsync(id, class, sev, ticker, firstMs)
	f.settle()
}

// anomalyAsync records an anomaly WITHOUT waiting for durability. It is for the
// broken-store case, where waiting would be waiting forever.
func (f *fixture) anomalyAsync(id, class string, sev risk.Severity,
	ticker string, firstMs int64) {

	f.t.Helper()
	a := risk.Anomaly{Class: class, Sev: sev, Ticker: ticker,
		Text: "operator-readable description of " + class + " on " + ticker}
	if _, err := f.store.RecordAnomaly(f.run, id, a, firstMs); err != nil {
		f.t.Fatalf("RecordAnomaly %s: %v", id, err)
	}
}

// service wires a service against this fixture's live store.
func (f *fixture) service(sender *NTFYSender, dead *HTTPSDeadman,
	interval time.Duration) *Service {

	f.t.Helper()
	svc, err := NewService(f.store.Reader(), f.store, sender, dead, interval)
	if err != nil {
		f.t.Fatalf("NewService: %v", err)
	}
	return svc
}

// lockDatabase takes SQLite's single write lock, which makes every store write
// fail with SQLITE_BUSY -- a transient failure, so the store goes unhealthy and
// keeps retrying. It is the honest way to break persistence from outside the
// package: no injected fake, and the recovery path is the real one.
func (f *fixture) lockDatabase() func() {
	f.t.Helper()
	lock, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		f.t.Fatalf("open lock connection: %v", err)
	}
	tx, err := lock.Begin()
	if err != nil {
		f.t.Fatalf("begin lock: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO run (run_id, started_ms, config_json)
		 VALUES ('lockholder', 1, x'7b7d')`); err != nil {
		f.t.Fatalf("take write lock: %v", err)
	}
	return func() {
		tx.Rollback()
		lock.Close()
	}
}

// awaitUnhealthy spins on the store's own report rather than sleeping. The
// writer fails on its FIRST attempt, so the flip is immediate; the bound exists
// so a mutation that never revokes adding fails fast instead of hanging.
func (f *fixture) awaitUnhealthy() {
	f.t.Helper()
	for i := 0; i < 1_000_000; i++ {
		if !f.store.Health().Healthy() {
			return
		}
		runtime.Gosched()
	}
	f.t.Fatal("the store never reported unhealthy while its database was " +
		"locked against writing")
}

// awaitHealthy is the other half: the store's own report that the write it was
// retrying has landed and the backlog behind it is durable.
//
// It waits out the writer's REAL retry ladder, because that is what recovery
// is. Bounded by the wall clock rather than by iterations: the writer sleeps
// between attempts, so an iteration bound would be a bound on how fast this
// machine spins rather than on how long recovery took.
func (f *fixture) awaitHealthy() {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		f.drain()
		if f.store.Health().Healthy() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	f.t.Fatalf("the store never recovered after its database was unlocked: "+
		"%+v", f.store.Health())
}

// ---------------------------------------------------------------------------
// Recording transports
// ---------------------------------------------------------------------------

type recorded struct {
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// recorder is a stand-in ntfy (or dead-man) endpoint that remembers every
// request and can be made to fail.
type recorder struct {
	mu     sync.Mutex
	reqs   []recorded
	status int
}

func newRecorder() *recorder { return &recorder{status: http.StatusOK} }

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, recorded{
		Path: req.URL.Path, Query: req.URL.RawQuery,
		Header: req.Header.Clone(), Body: string(body),
	})
	status := r.status
	r.mu.Unlock()
	w.WriteHeader(status)
}

func (r *recorder) setStatus(s int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = s
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = nil
}

func (r *recorder) count() int { return len(r.all()) }

// testTopic writes an env file and loads the topic through the real loader.
func testTopic(t *testing.T, value string) Topic {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	body := "# an operator's env file\nKALSHI_KEY_ID=not-the-topic\n" +
		"export NTFY_TOPIC=" + value + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	tp, err := LoadNTFYTopic(path)
	if err != nil {
		t.Fatalf("LoadNTFYTopic: %v", err)
	}
	return tp
}

// senderTo points a real NTFYSender at a test server.
func senderTo(t *testing.T, srv *httptest.Server, topic string) *NTFYSender {
	t.Helper()
	s, err := newNTFYSenderAt(srv.URL, testTopic(t, topic))
	if err != nil {
		t.Fatalf("newNTFYSenderAt: %v", err)
	}
	return s
}

// deadmanTo points a real HTTPSDeadman at a TLS test server.
//
// The client is built by `newBearerClient` and only its TRANSPORT is replaced,
// so the redirect policy under test is the production one. Swapping the whole
// client would make `M-P-REDIRECT` invisible here.
func deadmanTo(t *testing.T, srv *httptest.Server) *HTTPSDeadman {
	t.Helper()
	d, err := NewHTTPSDeadman(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPSDeadman: %v", err)
	}
	c := newBearerClient()
	c.Transport = srv.Client().Transport
	d.http = c
	return d
}

// pushesOf filters an Effects by kind.
func pushesOf(eff Effects, kind PushKind) []Push {
	var out []Push
	for _, p := range eff.Pushes {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

func heartbeatFor(global quote.GlobalState) Heartbeat {
	return Heartbeat{
		Global: global,
		Markets: []MarketLine{
			{Ticker: "KXTEST-A", State: quote.Quoting, Q: KnownQty(1200)},
		},
		Capital:     KnownMoney(42_000_000),
		Integrated:  KnownFloat(0.813),
		Uptime:      KnownDuration(90 * time.Minute),
		SourceStale: KnownBool(false),
		Undelivered: KnownInt(0),
	}
}

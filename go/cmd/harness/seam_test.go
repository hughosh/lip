package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// These tests drive the COMPOSITION ROOT, and nothing smaller.
//
// Every part they join is already the most heavily unit-tested code in the
// tree: `quote.Decide` has its own suite and its own mutations, so does
// `wsx.Gate`, so does `hstore`'s writer. Until `runtime.go` existed none of
// them had ever known each other -- `quote.Decide` had no caller outside its
// own tests, `wsx.Poller` was finished and nothing consumed its channel, and
// the whole of `harness/lifecycle` was reachable only from `lifecycle_test`.
// So a further unit test of an already-proven part finds nothing. What has
// never been exercised is whether two parts are joined in the right ORDER, on
// the right GOROUTINE, with the right LIFETIME.
//
// Hence the shape: a fake exchange -- a `rest.Doer` at the HTTP level and a
// `wsx.Dialer` at the frame level, which are the two seams the whole outside
// world reaches this process through -- driving a REAL `newRig` over a REAL
// SQLite store under `t.TempDir()`, through a REAL `serve`. Nothing here
// substitutes for `hstore`: `DispatchPermit` can only be issued by a committed
// transaction, and that is the property H-ORD-6 is about, so there is nothing
// to substitute.
//
// # Why the composed tests quote the EXIT and not an adding quote
//
// §9's schedule -- `market.close_time` and `program.end_date` -- is READ, and
// `owner` starts with `hasClose` false. `closeUnknown` makes that a
// market-scoped stop, because "a market whose close_time we do not know is one
// we cannot enforce a lead on" and quoting into it would rest an adding order
// into a close no lead protected. §5.2's answer to a stop is REDUCING: the
// adding side comes off and the CAPPED REDUCER RESTS. So a composed harness
// with inventory places its exit, and a composed harness that is flat places
// nothing -- and both of those are the specified behaviour rather than a
// fixture limitation. The tests below are built on it: an order on the wire
// here is the exit, and the absence of one on the adding side is I1 holding.
//
// The owner's schedule fields are private and the owner is constructed inside
// `serve`, so a test that drives `serve` cannot hand it a close. The tests that
// need one -- `TestAnUnknownCloseStopsAddingAndLeavesTheExitAlive` and
// `TestTheEarlyCloseBackoffStopsAddingOnlyInsideItsOwnWindow` -- build an
// `owner` and drive it on the test's own goroutine instead.
//
// # The clock is virtual and the tests never sleep on the real one
//
// `seamClock` satisfies `wsx.Clock` and also backs `exchange.Mono` and
// `exchange.NowMs`, so one `Advance` moves every deadline in `wsx` -- the
// poller's cadence, the session's ping ladder, the gate's freshness -- together
// and by an exact amount. Frozen, the write bucket never refills, which bounds
// what a run can send; advanced by `position_poll_s`, exactly one poll happens
// and nothing else does. The owner's own 250 ms tick and the monitor's 1 Hz
// tick are real timers inside `serve` and are deliberately left alone: they are
// what makes "the snapshot advances on every tick" observable at all.
//
// # What is safe to read from a test goroutine while `serve` runs
//
// `rig.snap` (an atomic.Pointer), `hstore.Store` and its `Ownership` and
// `Reader` (all mutex- or connection-guarded), and the fakes' own mutexes.
// NOTHING else. `quote.Queue`, `risk.Portfolio`, `core.Rig` and `wsx.Gate`
// carry no mutex by design (H-TOP-4) and belong to the owner goroutine.

// seamTicker is the one operator-chosen market of the pilot profile
// (pilot-plan §7.1). It is distinct from `dispatchTicker` so a fixture leak
// between the two files is a visible mismatch rather than a silent share.
const seamTicker = "KXSEAM-26AUG08-T1"

// The opening book. Both sides carry depth, because `core.Book.Qualifies()`
// walks both and a target above one side's depth reports every interval as
// gated -- which reads as "nobody could have scored here" for a market that is
// in fact paying.
const (
	seamYesTouch = 40
	seamNoTouch  = 55
	seamTarget   = 10.0
)

// seamBudget bounds every wait in this file.
//
// Generous on purpose, and it is not a substitute for determinism: every
// condition waited on here is reached by an EVENT (a frame, a poll answer, a
// write result) rather than by the passage of real time, so a correct build
// satisfies all of them in milliseconds. The budget exists so that a loaded
// gate machine under -race produces a slow pass rather than a flake, and so
// that a WEDGED build fails in seconds instead of hanging the ~50-minute gate
// run behind it.
const seamBudget = 15 * time.Second

// seamStopBudget bounds the teardown. `serve` returns on ctx.Err() and nothing
// else; a run loop that will not stop is a defect worth reporting rather than
// waiting out.
const seamStopBudget = 10 * time.Second

// ---------------------------------------------------------------------------
// The fake exchange -- REST
// ---------------------------------------------------------------------------

// seamCreate is one POST as the exchange received it, decoded from the WIRE
// body and not from any Go type this process holds.
//
// The wire and not the intent, deliberately: `rest.CreateOrder` is sealed and
// `createOrderWire` is private, so the only place the harness's H-CO-1
// transform, its `post_only`, its self-trade-prevention and its coid can be
// observed as the exchange would see them is here.
type seamCreate struct {
	Ticker   string
	WireSide string
	Price    string
	Count    string
	TIF      string
	STP      string
	PostOnly bool
	Coid     string
	// OrderID is what this fake exchange assigns. Derived from the coid so a
	// test can name the order id of a create it has not seen yet.
	OrderID string
}

// seamExchange is a `rest.Doer`: a TRANSPORT, not a typed fake of the API.
//
// The seam is at the status code and the raw bytes for the reason `rest.Doer`
// gives at length -- a 409 that means "your order landed", a cursor field whose
// name differs per endpoint, a 200 whose body does not identify an order -- and
// every one of those is invisible above that line.
type seamExchange struct {
	mu sync.Mutex

	balanceCents int64
	// positions maps ticker -> `position_fp`, the YES-signed net.
	positions map[string]string
	resting   []map[string]any
	fills     []map[string]any

	creates []seamCreate
	deletes []string
	// ordersWalks counts GET /portfolio/orders pages served. It is what makes
	// "no walk could have bound this order" a measured claim rather than an
	// assumption about the fixture.
	ordersWalks int

	// writersByID counts REST WRITES per goroutine id, and inWrite/maxInWrite
	// track overlap. D3 is a goroutine count; this is the only place in the
	// process from which that count is observable.
	writersByID map[uint64]int
	inWrite     int
	maxInWrite  int

	// listCreated makes an acknowledged create join the resting book, which is
	// what a real exchange does. Off in the tests that need to prove nothing
	// but the dispatch path could have learned an order id.
	listCreated bool

	// onCreate runs on the WRITER goroutine at the instant the create request
	// is about to be answered, so whatever it observes it observes strictly
	// before the order exists. It must not call t.Fatalf: it is not the test
	// goroutine.
	onCreate func(idx int, c seamCreate)

	// The §9 schedule this exchange reports. Zero values mean "a close far
	// out, active, not early-closeable" -- see `schedulePage`.
	closeAt       time.Time
	marketStatus  string
	canCloseEarly bool
}

func newSeamExchange() *seamExchange {
	return &seamExchange{
		balanceCents: 100_000,
		positions:    map[string]string{},
		writersByID:  map[uint64]int{},
	}
}

func (f *seamExchange) Do(_ context.Context, req rest.Request) (rest.Response, error) {
	switch {
	case req.Method == "GET" && req.Path == "/portfolio/positions":
		return f.positionsPage()
	case req.Method == "GET" && req.Path == "/portfolio/orders":
		return f.ordersPage()
	case req.Method == "GET" && req.Path == "/portfolio/fills":
		return f.fillsPage()
	case req.Method == "GET" && req.Path == "/portfolio/balance":
		return f.balancePage()
	case req.Method == "GET" && strings.HasPrefix(req.Path, "/markets/"):
		return f.schedulePage(strings.TrimPrefix(req.Path, "/markets/"))
	case req.Method == "POST" && req.Path == "/portfolio/events/orders":
		return f.write(func() (rest.Response, error) { return f.create(req) })
	case req.Method == "DELETE" &&
		strings.HasPrefix(req.Path, "/portfolio/events/orders/"):
		return f.write(func() (rest.Response, error) { return f.cancel(req) })
	}
	return rest.Response{}, fmt.Errorf("seamExchange got an unscripted %s %s",
		req.Method, req.Path)
}

// schedulePage answers §9's schedule read.
//
// Without it the harness has no `close_time`, `HasClose` is false, and
// `closeUnknown` stops the market -- correctly, since a lead cannot be enforced
// from a schedule nobody holds, but it means a composed test can only ever
// observe the EXIT. Answering it is what makes the adding side reachable.
//
// The default is a close far enough out that neither §16's `close_lead` nor the
// operator's early-close backoff is due, and `can_close_early` false so the
// backoff is not the thing under test unless a test asks for it.
func (f *seamExchange) schedulePage(ticker string) (rest.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	closeAt := f.closeAt
	if closeAt.IsZero() {
		closeAt = time.Now().Add(72 * time.Hour)
	}
	status := f.marketStatus
	if status == "" {
		status = rest.MarketStatusActive
	}
	body := fmt.Sprintf(`{"market":{"ticker":%q,"status":%q,"result":"",`+
		`"close_time":%q,"can_close_early":%t}}`,
		ticker, status, closeAt.UTC().Format(time.RFC3339), f.canCloseEarly)
	return rest.Response{Status: 200, Body: []byte(body)}, nil
}

// write wraps the two REST WRITES with D3's bookkeeping.
//
// The goroutine id is read INSIDE the request, before the answer, so a second
// writer is recorded even if it never overlaps the first. The overlap counter
// is the other half: two writers that happened to interleave perfectly would
// still show two ids, and two that raced would still show a depth of 2.
func (f *seamExchange) write(fn func() (rest.Response, error)) (rest.Response, error) {
	gid := seamGoroutineID()

	f.mu.Lock()
	f.inWrite++
	if f.inWrite > f.maxInWrite {
		f.maxInWrite = f.inWrite
	}
	f.writersByID[gid]++
	f.mu.Unlock()

	resp, err := fn()

	f.mu.Lock()
	f.inWrite--
	f.mu.Unlock()
	return resp, err
}

func (f *seamExchange) create(req rest.Request) (rest.Response, error) {
	var body struct {
		Ticker              string `json:"ticker"`
		Side                string `json:"side"`
		Count               string `json:"count"`
		Price               string `json:"price"`
		TimeInForce         string `json:"time_in_force"`
		SelfTradePrevention string `json:"self_trade_prevention_type"`
		PostOnly            bool   `json:"post_only"`
		ClientOrderID       string `json:"client_order_id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return rest.Response{}, fmt.Errorf("create body: %w", err)
	}

	c := seamCreate{
		Ticker: body.Ticker, WireSide: body.Side, Price: body.Price,
		Count: body.Count, TIF: body.TimeInForce,
		STP: body.SelfTradePrevention, PostOnly: body.PostOnly,
		Coid: body.ClientOrderID, OrderID: "EX-" + body.ClientOrderID,
	}

	f.mu.Lock()
	idx := len(f.creates)
	f.creates = append(f.creates, c)
	hook := f.onCreate
	list := f.listCreated
	f.mu.Unlock()

	if hook != nil {
		hook(idx, c)
	}
	if list {
		f.mu.Lock()
		f.resting = append(f.resting, seamRestingOrder(c))
		f.mu.Unlock()
	}

	// The measured flat CreateOrderV2Response. Every field `parseAck` requires
	// is present: a response missing one is UNKNOWN rather than ACKED, and would
	// silently turn a placement test into a reconciliation test.
	ack, err := json.Marshal(map[string]any{
		"order_id":        c.OrderID,
		"client_order_id": c.Coid,
		"remaining_count": c.Count,
		"fill_count":      "0.00",
		"ts_ms":           1,
	})
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: ack}, nil
}

func (f *seamExchange) cancel(req rest.Request) (rest.Response, error) {
	id := strings.TrimPrefix(req.Path, "/portfolio/events/orders/")

	f.mu.Lock()
	f.deletes = append(f.deletes, id)
	kept := f.resting[:0]
	for _, o := range f.resting {
		if o["order_id"] == id {
			continue
		}
		kept = append(kept, o)
	}
	f.resting = kept
	f.mu.Unlock()

	body, err := json.Marshal(map[string]any{
		"order_id": id, "client_order_id": "", "reduced_by": "1.00", "ts_ms": 1,
	})
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: body}, nil
}

// seamRestingOrder renders a create as the resting-order record the exchange
// would list afterwards.
//
// `yes_price_dollars` carries the create's own wire price unchanged, because
// the wire price IS yes-denominated (H-CO-1): a NO bid at 55c goes out as an
// `ask` at "0.4500" and comes back as a `side: no` order at yes 0.4500, which
// `rest.readPrice` turns back into 55c. Rebuilding it any other way here would
// put a second, untested copy of the transform in the fixture.
func seamRestingOrder(c seamCreate) map[string]any {
	side := "yes"
	if c.WireSide == string(rest.Ask) {
		side = "no"
	}
	return map[string]any{
		"order_id":          c.OrderID,
		"client_order_id":   c.Coid,
		"ticker":            c.Ticker,
		"side":              side,
		"yes_price_dollars": c.Price,
		"remaining_count":   c.Count,
		"status":            rest.StatusResting,
	}
}

// The four read pages. Each carries its endpoint's own cursor key, present and
// empty on the terminal page, and every declared item array -- `rest.decodePage`
// refuses an absent cursor or an absent array outright, because reading either
// as "the walk finished and the account is empty" is how `200 {}` overwrites a
// live position with flat.

func (f *seamExchange) positionsPage() (rest.Response, error) {
	f.mu.Lock()
	items := make([]any, 0, len(f.positions))
	for ticker, q := range f.positions {
		items = append(items, map[string]any{"ticker": ticker, "position_fp": q})
	}
	f.mu.Unlock()
	return seamJSON(map[string]any{
		"cursor":           "",
		"market_positions": items,
		"event_positions":  []any{},
	})
}

func (f *seamExchange) ordersPage() (rest.Response, error) {
	f.mu.Lock()
	f.ordersWalks++
	items := make([]any, 0, len(f.resting))
	for _, o := range f.resting {
		items = append(items, o)
	}
	f.mu.Unlock()
	return seamJSON(map[string]any{"cursor": "", "orders": items})
}

func (f *seamExchange) fillsPage() (rest.Response, error) {
	f.mu.Lock()
	items := make([]any, 0, len(f.fills))
	for _, fl := range f.fills {
		items = append(items, fl)
	}
	f.mu.Unlock()
	return seamJSON(map[string]any{"cursor": "", "fills": items})
}

func (f *seamExchange) balancePage() (rest.Response, error) {
	f.mu.Lock()
	cents := f.balanceCents
	f.mu.Unlock()
	return seamJSON(map[string]any{"balance": cents})
}

func seamJSON(v any) (rest.Response, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: b}, nil
}

// --- observation helpers, all mutex-guarded so a test goroutine may call them
// while the dispatcher is writing --------------------------------------------

func (f *seamExchange) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *seamExchange) createAt(i int) (seamCreate, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.creates) {
		return seamCreate{}, false
	}
	return f.creates[i], true
}

func (f *seamExchange) allCreates() []seamCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seamCreate(nil), f.creates...)
}

func (f *seamExchange) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

func (f *seamExchange) ordersWalkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ordersWalks
}

func (f *seamExchange) restingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resting)
}

func (f *seamExchange) setPosition(ticker, positionFP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positions[ticker] = positionFP
}

func (f *seamExchange) addFill(fill map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fills = append(f.fills, fill)
}

// writerIDs is every goroutine that has issued a REST WRITE, with its count.
func (f *seamExchange) writerIDs() map[uint64]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uint64]int, len(f.writersByID))
	for id, n := range f.writersByID {
		out[id] = n
	}
	return out
}

func (f *seamExchange) maxConcurrentWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInWrite
}

// seamGoroutineID reads the calling goroutine's id out of its own stack header.
//
// There is no supported way to ask for it, and that is exactly why D3 is stated
// as "one REST writer" and enforced by there BEING one goroutine rather than by
// an assertion. This is the assertion, and it is confined to a test: the first
// line of `runtime.Stack` is "goroutine N [running]:". A parse failure returns
// 0, and the tests assert the id is non-zero so a broken parse fails loudly
// instead of making every writer look like the same one.
func seamGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// ---------------------------------------------------------------------------
// The fake exchange -- websocket
// ---------------------------------------------------------------------------

// seamSigner is `wsx.Signer` and nothing else. The supervisor is deliberately
// given the handshake-header interface rather than `*feed.Signer`, because
// signing headers is all it may do with a credential; this fake holds none.
type seamSigner struct{}

func (seamSigner) WSHeaders(int64) (map[string]string, error) {
	return map[string]string{"KALSHI-ACCESS-KEY": "seam-test"}, nil
}

// seamDialer hands out one socket that replays scripted frames.
//
// It never fails a dial and never closes a connection, so the gate's generation
// stays at 1 for the whole of every test here. That matters: a disconnect
// retires every snapshot and every reconciliation taken on the dead connection
// (H-FAIL-5), so a fixture that dropped the socket would make every actionable
// market non-actionable for reasons unrelated to what was being tested.
type seamDialer struct {
	mu      sync.Mutex
	frames  chan []byte
	sent    [][]byte
	nextSeq int64
}

func newSeamDialer() *seamDialer {
	return &seamDialer{frames: make(chan []byte, 64), nextSeq: 1}
}

func (d *seamDialer) Dial(_ context.Context, _ string, _ http.Header) (wsx.Socket, error) {
	return &seamSocket{d: d}, nil
}

// frame builds one enveloped frame, stamping the subscription-wide sequence.
//
// The sequence is stamped HERE rather than by the caller because `core.Rig`
// quarantines every book on the subscription when it sees a gap, and a fixture
// that let a test pick its own numbers would make an off-by-one in the test
// indistinguishable from the defect the quarantine exists to catch.
func (d *seamDialer) frame(typ string, msg map[string]any) ([]byte, error) {
	d.mu.Lock()
	seq := d.nextSeq
	d.nextSeq++
	d.mu.Unlock()
	return json.Marshal(map[string]any{
		"type": typ, "sid": 1, "seq": seq, "msg": msg,
	})
}

func (d *seamDialer) subscriptions() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.sent...)
}

type seamSocket struct{ d *seamDialer }

// Read blocks for the next scripted frame, or until the session's context ends.
//
// Blocking on an empty channel is the point: a live socket that has nothing to
// say does not return, and a fake that returned io.EOF would be a disconnect
// the test never asked for.
func (s *seamSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.d.frames:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *seamSocket) Write(_ context.Context, b []byte) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.d.sent = append(s.d.sent, append([]byte(nil), b...))
	return nil
}

func (s *seamSocket) Ping(context.Context) error { return nil }
func (s *seamSocket) Close() error               { return nil }

// ---------------------------------------------------------------------------
// The virtual clock
// ---------------------------------------------------------------------------

// seamClock is `wsx.Clock` plus the two functions `exchange` separates.
//
// One source for both readings, and the separation kept: `Mono` is what every
// age in `wsx` and every elapsed interval in the owner is measured against, and
// `WallMs` is what records carry. F21 (a clock step) and F7 (host sleep) both
// turn on the two being distinguishable, so a fixture that derived one from the
// other would be testing a clock the harness does not have.
//
// Timers are one-shot and fire on `Advance`, exactly as a real timer does when
// its deadline passes. They are never pruned: `wsx.Timer` is a resettable
// handle -- the poller resets the same timer after every cycle -- so a fixture
// that forgot a stopped timer would silently stop polling.
type seamClock struct {
	mu     sync.Mutex
	mono   time.Duration
	wall   int64
	timers []*seamTimer
}

func newSeamClock() *seamClock {
	return &seamClock{wall: time.Now().UnixMilli()}
}

func (c *seamClock) Now() wsx.Stamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsx.Stamp{WallMs: c.wall, Mono: c.mono}
}

func (c *seamClock) NewTimer(d time.Duration) wsx.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &seamTimer{
		c: c, ch: make(chan time.Time, 1), deadline: c.mono + d, live: true,
	}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves both clocks forward and fires every timer whose deadline the
// step crossed.
//
// Callers keep the step under `read_deadline_s` (60 s) and normally under
// `ping_interval_s` (10 s): those are the session's own F1 clocks, and stepping
// past one drops the socket, which is a different test.
func (c *seamClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d
	c.wall += int64(d / time.Millisecond)
	now := time.Unix(0, int64(c.mono))
	for _, t := range c.timers {
		if !t.live || t.deadline > c.mono {
			continue
		}
		t.live = false
		select {
		case t.ch <- now:
		default:
		}
	}
}

func (c *seamClock) monoNow() time.Duration { return c.Now().Mono }
func (c *seamClock) wallMs() int64          { return c.Now().WallMs }

type seamTimer struct {
	c        *seamClock
	ch       chan time.Time
	deadline time.Duration
	live     bool
}

func (t *seamTimer) C() <-chan time.Time { return t.ch }

func (t *seamTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.live
	t.live = false
	return was
}

func (t *seamTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.live
	t.deadline = t.c.mono + d
	t.live = true
	return was
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

// seamOptions is what a test varies about the fixture. Everything absent is the
// pilot default.
type seamOptions struct {
	// Resume is `-resume`: §10.4's operator action, asserted at construction.
	Resume bool
	// Latched pre-writes a durable halt latch, as a previous incarnation would
	// have left it.
	Latched bool
	// ListCreated makes an acknowledged create join the resting book.
	ListCreated bool
	// Positions seeds `GET /portfolio/positions` before the run starts. A
	// composed harness quotes its EXIT and nothing else -- see the file header
	// -- so a test that wants an order on the wire seeds a position here.
	Positions map[string]string
	// SkipRig builds the config and the fakes but not the rig, for the tests
	// whose subject IS `newRig`.
	SkipRig bool
}

// seamHarness is one composed harness process plus everything outside it.
type seamHarness struct {
	t   *testing.T
	ctx context.Context

	cfg config
	ex  *seamExchange
	ws  *seamDialer
	clk *seamClock
	xch exchange

	rig *rig

	serveCancel context.CancelFunc
	serveDone   chan struct{}
	serveErr    error
}

// newSeamConfig builds a pilot config whose every artifact is under t.TempDir().
//
// Nothing here may name a path outside the temp directory. `rig.db` and
// `lip.db` in the repository root have had live collectors writing them since
// 24 July, and a test that opened one writable would switch its journal mode
// underneath two running writers.
func newSeamConfig(t *testing.T) config {
	t.Helper()
	d := t.TempDir()
	store := filepath.Join(d, "store")
	return config{
		Params:         cfg.Default(),
		Ticker:         seamTicker,
		Rung:           rungs["pilot"],
		EarlyCloseLead: defaultEarlyCloseLead,
		Paths: paths{
			DB:         filepath.Join(store, "harness.db"),
			AnomalyLog: filepath.Join(store, "anomaly.jsonl"),
			Latch:      filepath.Join(d, "harness.halt"),
			Lock:       filepath.Join(d, "harness.lock"),
			Key:        filepath.Join(d, "kalshi.pem"),
			Env:        filepath.Join(d, "env"),
		},
	}
}

func newSeamHarness(t *testing.T, opt seamOptions) *seamHarness {
	t.Helper()

	c := newSeamConfig(t)
	// The store is PROVISIONED, never created by the run path. That is not
	// fixture hygiene, it is the rule `requireExistingDB` enforces: an absent
	// path and a mistyped one are the same thing to `hstore.Open`, and the
	// ledger it creates for either recognises no order id at all.
	if err := provision(c, io.Discard); err != nil {
		t.Fatalf("provisioning the seam store: %v", err)
	}

	if opt.Latched {
		latch, err := lifecycle.NewFileLatch(c.Paths.Latch)
		if err != nil {
			t.Fatalf("NewFileLatch: %v", err)
		}
		durable, err := latch.Ensure(lifecycle.LatchRecord{
			Version: 1, Trigger: "taker_fill", Market: seamTicker,
			TsMillis: time.Now().UnixMilli(),
		})
		if err != nil || !durable {
			t.Fatalf("seeding the halt latch: durable=%v err=%v", durable, err)
		}
	}

	h := &seamHarness{
		t:   t,
		ctx: context.Background(),
		cfg: c,
		ex:  newSeamExchange(),
		ws:  newSeamDialer(),
		clk: newSeamClock(),
	}
	h.ex.listCreated = opt.ListCreated
	for ticker, q := range opt.Positions {
		h.ex.positions[ticker] = q
	}

	// Seeded BEFORE the dial, so the session delivers it as soon as it reads.
	// The gate applies `OnConnect` first regardless -- the supervisor emits
	// EventConnected on the same channel before the session starts -- so the
	// snapshot always lands on the current generation.
	frame, err := h.ws.frame("orderbook_snapshot", seamBook())
	if err != nil {
		t.Fatalf("building the opening snapshot: %v", err)
	}
	h.ws.frames <- frame

	h.xch = exchange{
		Doer:   h.ex,
		Dialer: h.ws,
		Signer: seamSigner{},
		Clock:  h.clk,
		Target: seamTarget,
		NowMs:  h.clk.wallMs,
		Mono:   h.clk.monoNow,
	}

	if opt.SkipRig {
		return h
	}

	r, err := newRig(h.ctx, c, opt.Resume, h.xch)
	if err != nil {
		t.Fatalf("newRig: %v", err)
	}
	h.rig = r
	t.Cleanup(h.closeRig)
	return h
}

// seamBook is the opening orderbook snapshot message: a yes bid at 40c and a no
// bid at 55c, twenty contracts each. They sum to 95, so the two sides do not
// cross and H-CO-6 constrains nothing here.
func seamBook() map[string]any {
	return map[string]any{
		"market_ticker":  seamTicker,
		"ts_ms":          1,
		"yes_dollars_fp": [][]string{{"0.4000", "20.00"}},
		"no_dollars_fp":  [][]string{{"0.5500", "20.00"}},
	}
}

// start runs `serve` on its own goroutine and registers its stop.
//
// Cleanup order is LIFO, so this stop runs BEFORE `closeRig`: the store cannot
// be shut down while the loops that submit to it are still running, and
// `hstore.Shutdown` refuses rather than closing over an accepted record.
func (h *seamHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	h.serveCancel = cancel
	h.serveDone = make(chan struct{})
	go func() {
		h.serveErr = h.rig.serve(ctx)
		close(h.serveDone)
	}()
	h.t.Cleanup(h.stopServe)
}

func (h *seamHarness) stopServe() {
	if h.serveCancel == nil {
		return
	}
	h.serveCancel()
	h.serveCancel = nil
	select {
	case <-h.serveDone:
	case <-time.After(seamStopBudget):
		h.t.Errorf("serve did not return within %v of its context being "+
			"cancelled; a run loop that cannot be stopped wedges every test "+
			"after it", seamStopBudget)
	}
}

// closeRig stops the store in `hstore.Shutdown`'s order and reports rather than
// forces.
//
// The retry is not superstition: `runResults` makes one last pass on ctx.Done,
// so a submission can land between `Drain` reporting empty and `Close` running,
// and `Close` refuses with an accepted record queued. A second pass drains it.
// A failure after that is LOGGED and not failed: what this file tests is the
// composition, and a store that would not stop is `shutdown_test.go`'s subject.
func (h *seamHarness) closeRig() {
	ctx, cancel := context.WithTimeout(context.Background(), seamStopBudget)
	defer cancel()
	err := h.rig.close(ctx)
	if err == nil {
		return
	}
	if retry := h.rig.close(ctx); retry != nil {
		h.t.Logf("the seam store would not stop cleanly: %v (retry: %v)",
			err, retry)
	}
}

// await polls a condition until it holds, the budget expires, or `serve` gives
// up first.
//
// The `serve` check is what turns "the startup walk failed" from a fifteen
// second timeout into an immediate, named failure.
func (h *seamHarness) await(why string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(seamBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		if h.serveDone != nil {
			select {
			case <-h.serveDone:
				h.t.Fatalf("serve returned (%v) while waiting for %s",
					h.serveErr, why)
			default:
			}
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("timed out after %v waiting for %s", seamBudget, why)
}

func (h *seamHarness) snapshot() *risk.Snapshot { return h.rig.snap.Load() }

func (h *seamHarness) snapSeq() uint64 {
	s := h.snapshot()
	if s == nil {
		return 0
	}
	return s.Seq
}

// market is the one market's published state, or false before the first tick.
func (h *seamHarness) market() (risk.MarketSnap, bool) {
	s := h.snapshot()
	if s == nil || len(s.Markets) == 0 {
		return risk.MarketSnap{}, false
	}
	return s.Markets[0], true
}

// awaitActionable waits for the state A13 requires before any placement
// decision may be taken: connected, snapshotted on THIS generation, and
// positions, orders and fills each reconciled after it and inside
// `truth_max_age_s`.
func (h *seamHarness) awaitActionable() {
	h.t.Helper()
	h.await("the market to satisfy A13 (connected, snapshotted, all three "+
		"portfolio endpoints reconciled on this connection)", func() bool {
		m, ok := h.market()
		return ok && m.BookActionable
	})
}

// awaitTicks waits for `n` further owner ticks, measured by H-TOP-5's sequence.
func (h *seamHarness) awaitTicks(n uint64) {
	h.t.Helper()
	from := h.snapSeq()
	h.await(fmt.Sprintf("%d owner ticks after snapshot seq %d", n, from),
		func() bool { return h.snapSeq() >= from+n })
}

// ownerFor builds an owner over a rig that is NOT serving.
//
// Everything it touches -- the queue, the portfolio, the book, the capacity --
// carries no mutex by design, so these tests run it on the test's own goroutine
// and never start `serve`. That is not a workaround: it is the same
// single-writer discipline `run.go` states, applied to the test. It is also the
// only way to reach the owner's §9 schedule fields, which are private and are
// filled by a poll `serve` owns.
func (h *seamHarness) ownerFor() *owner {
	h.t.Helper()
	o := newOwner(h.rig, newShutdown(h.rig))
	o.global = quote.Running
	return o
}

// connectGate puts the gate in the state a live connection produces, for the
// owner-level tests that do not run the websocket loop.
//
// Only `Connected` and the generation; portfolio truth is deliberately NOT
// forged. `Gate.noteTruth` is private precisely so that no test can manufacture
// a complete reconciliation -- "an exported version is a SetActionable(true)
// with a longer name" -- so `Actionable` stays false in these tests and no
// PLACEMENT decision is taken. Cancels are unaffected, which is the asymmetry
// every stop path in this system has (I1), and the cancels are what those tests
// are about.
func (h *seamHarness) connectGate() {
	h.t.Helper()
	eff := h.rig.gate.OnConnect(h.clk.Now())
	if !eff.Token.Valid() {
		h.t.Fatalf("the gate issued no reconciliation token on connect")
	}
	if !h.rig.gate.Connected() {
		h.t.Fatalf("the gate is not connected after OnConnect")
	}
}

// installBook feeds one snapshot straight into the decoder, for the owner-level
// tests that do not run the websocket loop.
func (h *seamHarness) installBook(yes, no [][]string) {
	h.t.Helper()
	frame, err := h.ws.frame("orderbook_snapshot", map[string]any{
		"market_ticker":  seamTicker,
		"ts_ms":          1,
		"yes_dollars_fp": yes,
		"no_dollars_fp":  no,
	})
	if err != nil {
		h.t.Fatalf("building a snapshot frame: %v", err)
	}
	if err := h.rig.book.Handle(frame); err != nil {
		h.t.Fatalf("core refused the seam snapshot: %v", err)
	}
}

// installResting puts orders of ours into the position model the way a complete
// resting-orders walk would (H-POS-4).
func (h *seamHarness) installResting(orders ...risk.LiveOrder) {
	h.t.Helper()
	eff := h.rig.pf.ReplaceOrders(orders, nil, true)
	if !eff.Applied {
		h.t.Fatalf("the resting orders were not installed: %+v", eff.Anomalies)
	}
}

// installPosition seeds q the way a complete positions walk would (H-POS-1).
func (h *seamHarness) installPosition(q num.Qty) {
	h.t.Helper()
	eff := h.rig.pf.ReplacePositions(map[string]num.Qty{seamTicker: q}, true,
		h.cfg.Params)
	if !eff.Applied {
		h.t.Fatalf("the position was not installed: %+v", eff.Anomalies)
	}
}

// takeRaised drains the anomaly sink without submitting anything, so a test can
// assert on what a component RAISED rather than on what survived the writer.
// (`shutdown_test.go` has the same helper on its own fixture; this one exists
// because these tests hold a `*rig` rather than that fixture.)
func (h *seamHarness) takeRaised() []risk.Anomaly {
	var out []risk.Anomaly
	for {
		select {
		case a := <-h.rig.anom.ch:
			out = append(out, a)
		default:
			return out
		}
	}
}

// anomalyClasses reads back every DURABLE anomaly class in the store.
func (h *seamHarness) anomalyClasses() []string {
	h.t.Helper()
	rows, err := h.rig.store.Reader().PendingAnomalies()
	if err != nil {
		h.t.Fatalf("PendingAnomalies: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Class)
	}
	return out
}

// seamCancelsOn is every queued intent whose current leg is a cancel on one
// side.
func seamCancelsOn(q *quote.Queue, side quote.Side) []quote.Intent {
	var out []quote.Intent
	for _, in := range q.Pending() {
		if in.Side == side && in.Op() == quote.OpCancel {
			out = append(out, in)
		}
	}
	return out
}

func seamContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// seamNewRig calls `newRig` and turns a panic on its REFUSAL path into an
// ordinary, named test failure.
//
// It exists because of a live defect in `runtime.go`, and it is not a licence
// to tolerate a panic anywhere else. `newRig` uses NAMED results and registers
//
//	defer func() { if err != nil { r.unwind(); r = nil } }()
//
// immediately after assigning `r`. Every refusal after that line is written
// `return nil, err`, which assigns nil to the NAMED result `r` before the
// deferred closure runs -- so the closure calls `unwind` on a nil `*rig` and
// `len(r.cleanup)` dereferences it. That is every one of the constructor's
// refusals, including the two this file is about: H-DEP-5's instance lock and
// H-HALT-4's "-resume was not given". `main.go` wraps them in `refusal` and
// exits 2; what actually happens is a nil-pointer panic and a crash that
// `launchd KeepAlive` restarts forever.
//
// The recover is here so that ONE broken error path does not abort the whole
// test binary and hide every other seam this file covers. The test still fails,
// loudly, with the defect named.
func seamNewRig(t *testing.T, ctx context.Context, c config, resume bool,
	ex exchange) (*rig, error) {

	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("newRig PANICKED instead of returning its refusal: %v\n\n"+
				"`newRig` has named results and registers\n"+
				"    defer func() { if err != nil { r.unwind(); r = nil } }()\n"+
				"right after assigning `r`. Every refusal below that line is "+
				"`return nil, err`, which sets the NAMED result `r` to nil "+
				"before the closure runs -- so `unwind` dereferences a nil "+
				"*rig.\n\n"+
				"This is not a test-only crash. It is the whole refusal "+
				"surface of the composition root: the instance lock (H-DEP-5), "+
				"the set halt latch without -resume (H-HALT-4), a missing "+
				"store, an unopenable store, a run row that would not commit, "+
				"and every collaborator constructor after them. `main.go` "+
				"expects an error it can classify as a `refusal` and exit 2 "+
				"with; it gets a nil-pointer panic and a crash that `launchd "+
				"KeepAlive` restarts forever.\n\n"+
				"Fix belongs in runtime.go, not here: return `r` rather than "+
				"nil on the refusal paths, or guard the closure with "+
				"`r != nil`", p)
		}
	}()
	return newRig(ctx, c, resume, ex)
}

// ---------------------------------------------------------------------------
// 1. The whole tick
// ---------------------------------------------------------------------------

// TestAFullTickPlacesTheExitAtTheExternalTouch is the test that proves the
// parts are wired together at all.
//
// Before `runtime.go` and `run.go` nothing in this repository had ever composed
// them: `quote.Decide` had no production caller, `wsx.Poller` was complete and
// nothing consumed its channel, and `harness/lifecycle` was reachable only from
// its own tests. So this drives one whole tick through every join in order --
// instance lock, store open, run row, latch read, §7.5 adoption, socket
// connect, book snapshot, portfolio poll, §5.2's market state, §6.2's reducer
// sizing, §6.5's price, §6.6's queue and token bucket, H-ORD-6's two-stage
// commit, the wire -- and asserts the one artifact that can only exist if every
// one of them worked: an order on the exchange.
//
// The order is the EXIT, and that is the specified behaviour rather than a
// limitation of the fixture. §9's schedule is read and this harness has not
// read one yet, so `hasClose` is false; §5.2 answers an unenforceable close
// with a market-scoped stop, and a stop sends the market to REDUCING -- adding
// side off, capped reducer resting. I1 in one sentence: every stop path stops
// ADDING risk and none of them stops reducing it.
//
// The assertions are on the WIRE BODY rather than on any Go value this process
// holds, because that is the only place H-CO-1's transform, H-Q-3's `post_only`,
// H-CO-5's self-trade-prevention and H-ORD-1's coid are all simultaneously
// observable, and it is what the exchange would actually have received.
func TestAFullTickPlacesTheExitAtTheExternalTouch(t *testing.T) {
	// q = +8: long YES, so the exit is a NO bid (§6.2's R).
	held := num.QtyFromFloat(8)
	h := newSeamHarness(t, seamOptions{
		Positions: map[string]string{seamTicker: held.Wire()},
	})
	h.start()

	h.awaitActionable()
	h.await("the exit to reach the exchange",
		func() bool { return h.ex.createCount() >= 1 })

	c, ok := h.ex.createAt(0)
	if !ok {
		t.Fatalf("no create was recorded")
	}

	if m, present := h.market(); !present || m.State != quote.Reducing {
		t.Fatalf("the market is %v (present=%v), want REDUCING: §9's schedule "+
			"has not been read, so no close lead can be enforced and §5.2's "+
			"answer is a market-scoped stop -- which keeps the exit alive "+
			"rather than cancelling everything", m.State, present)
	}

	// --- H-ORD-1: the coid identifies the run, the market and the side ------
	if !strings.HasPrefix(c.Coid, rest.CoidPrefix+"-") {
		t.Fatalf("the order carried client_order_id %q, which does not begin "+
			"with %q; §7.5's adoption identifies our orders by that prefix and "+
			"an order without it reads as a third party's on a dedicated "+
			"account (H-ORD-9)", c.Coid, rest.CoidPrefix)
	}
	parsed, ours := rest.ParseCoid(c.Coid)
	if !ours {
		t.Fatalf("client_order_id %q does not parse as one of ours", c.Coid)
	}
	if parsed.RunID != h.rig.runID {
		t.Fatalf("the coid names run %q but this process is run %q; a coid that "+
			"does not name its own run cannot be reconstructed after a restart, "+
			"which is the whole of H-ORD-2b's same-coid recovery",
			parsed.RunID, h.rig.runID)
	}
	if parsed.MarketIdx != pilotMarketIdx {
		t.Fatalf("the coid names market index %d, want %d; ParseCoid reads it "+
			"back as the market the order belongs to and a wrong one sizes a "+
			"reducer against a position we do not hold",
			parsed.MarketIdx, pilotMarketIdx)
	}
	if parsed.Side != quote.SideNo {
		t.Fatalf("the exit was placed on the %s side while q is +%s. §6.2's "+
			"reducing side is NO when q > 0, and an exit on the wrong side is "+
			"not an exit -- it doubles the position", parsed.Side, held.Wire())
	}

	// --- H-CO-1: a NO bid goes out as `ask` at the YES-denominated price ----
	if c.Ticker != seamTicker {
		t.Fatalf("the order names market %q, want %q", c.Ticker, seamTicker)
	}
	if c.WireSide != string(rest.Ask) {
		t.Fatalf("a no bid went out with wire side %q, want %q; a NO bid at "+
			"42c IS a YES ask at 58c, and getting the leg wrong adopts the "+
			"position with the wrong sign (H-CO-1)", c.WireSide, rest.Ask)
	}

	// --- H-Q-1 / H-Q-2: AT the external touch, never inside it --------------
	if want := rest.PriceWire(100 - seamNoTouch); c.Price != want {
		t.Fatalf("the exit priced at %s, want %s. The book publishes a no bid "+
			"at %dc and nothing of ours was resting there, so H-Q-1 places AT "+
			"that price -- inside it is H-Q-2's forbidden improvement, and "+
			"behind it is an exit that does not exit", c.Price, want, seamNoTouch)
	}

	// --- H-Q-5a: the reducer never exceeds |q| ------------------------------
	if want := held.Wire(); c.Count != want {
		t.Fatalf("the exit is for %s contracts, want %s = min(|q|, S_max, "+
			"funded). H-Q-5a caps the aggregate at |q| because a reducer that "+
			"overshoots FLIPS the position -- HR-004 is two individually-capped "+
			"reducers that both filled", c.Count, want)
	}

	// --- H-Q-3, H-CO-5: the two flags that are never a decision -------------
	if !c.PostOnly {
		t.Fatalf("post_only was false on the wire. H-Q-3 forbids taking with " +
			"no exception, no flag and no code path that sets it false, and A1 " +
			"asserts it over every order ever sent")
	}
	if c.STP != "taker_at_cross" {
		t.Fatalf("self_trade_prevention_type is %q, want taker_at_cross; "+
			"`maker` cancels our RESTING order on a self-match and would "+
			"silently destroy the scoring presence we are paid for (H-CO-5)",
			c.STP)
	}
	if c.TIF != "good_till_canceled" {
		t.Fatalf("time_in_force is %q, want good_till_canceled", c.TIF)
	}

	// --- I1: the ADDING side is off, and nothing was placed on it -----------
	for i, made := range h.ex.allCreates() {
		p, _ := rest.ParseCoid(made.Coid)
		if p.Side == quote.SideYes {
			t.Fatalf("create %d was placed on the ADDING side (%s) while the "+
				"market was stopped for an unreadable close. §5.2's REDUCING "+
				"row cancels the adding side and confirms it absent; placing "+
				"there is adding risk into a market whose close no lead is "+
				"protecting", i, made.Coid)
		}
	}

	// --- H-ORD-6: and the ownership row is on disk, not merely in memory ----
	h.await("the owned_order row for the dispatched coid to be readable",
		func() bool {
			row, found, err := h.rig.store.Reader().OwnedOrder(c.Coid)
			return err == nil && found && row.Ticker == seamTicker &&
				row.RunID == h.rig.runID
		})

	// --- the socket half was really wired too -------------------------------
	if subs := h.ws.subscriptions(); len(subs) != 2 {
		t.Fatalf("the session sent %d subscriptions, want 2: a connection "+
			"carrying only the filtered delta stream looks healthy, answers "+
			"pings and produces books, and never delivers a single trade",
			len(subs))
	}
}

// ---------------------------------------------------------------------------
// 2. H-ORD-6 at the composition root
// ---------------------------------------------------------------------------

// TestNoOrderIsDispatchedWhileTheStoreCannotCommitAReservation is H-ORD-6
// asserted from the only side that can prove it: the wire.
//
// `dispatch_test.go` already proves that `executeWrite` refuses without a
// committed permit. What it cannot prove is that the COMPOSED process has no
// other route to the exchange -- that the owner does not build a request and
// hand it somewhere else, that no second path reaches `rest.Client.Create`, and
// that the two-stage commit is not bypassed by anything `serve` starts. So this
// one runs the whole harness, makes every reservation permanently unwritable,
// and watches the exchange.
//
// The store's WRITER is stopped, which is the strongest form of "cannot
// commit": `releaseWriter` latches `writerGone`, and from then on `submit`
// REFUSES rather than queueing for a writer that will never run. Every
// placement therefore fails at stage one, and there is no timing window in
// which one could slip past.
//
// The test is not vacuous, and the two assertions below are what make it so:
// the market really did become actionable and really was REDUCING with the
// external touch visible and inventory to exit, so §6.5 wanted an order and
// everything except the durable permit was in place.
// `TestAFullTickPlacesTheExitAtTheExternalTouch` is the control: same fixture,
// live store, an order on the wire.
//
// Expect stderr lines from `submitAnomaly` while this runs. They are §13.1's
// last-resort channel doing its job -- a store that cannot take the anomaly
// about itself still has to say so somewhere.
func TestNoOrderIsDispatchedWhileTheStoreCannotCommitAReservation(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Positions: map[string]string{seamTicker: "8.00"},
	})

	// Stop the writer BEFORE the run loop starts. Nothing on the §7.5 happy
	// path writes, so the startup walk still completes and the harness still
	// reaches RUNNING -- which is what makes this a test about the dispatch
	// barrier rather than about a harness that never started.
	h.rig.storeCancel()
	<-h.rig.storeDone

	h.start()
	h.awaitActionable()

	m, ok := h.market()
	if !ok {
		t.Fatalf("no snapshot was published")
	}
	if m.State != quote.Reducing {
		t.Fatalf("the market is %s, want REDUCING; the point of this test is "+
			"that §6.2 WANTED to rest an exit and could not", m.State)
	}
	if got := m.Sides[quote.SideNo].TouchCents; got != seamNoTouch {
		t.Fatalf("the published no touch is %dc, want %dc; without an external "+
			"touch there would be nothing to place against and the absence of "+
			"an order would prove nothing", got, seamNoTouch)
	}
	if m.Q == 0 {
		t.Fatalf("the harness reads its position as flat, so there is no exit " +
			"to place and the absence of an order proves nothing")
	}

	// Ten further ticks of the real 250 ms owner loop. Each one re-evaluates
	// §6.5, re-enqueues, and re-pumps: a build that took `ReserveOrder`'s
	// RECEIPT as licence, or that reserved and dispatched concurrently, has ten
	// separate opportunities to put a POST on the wire.
	h.awaitTicks(10)

	if n := h.ex.createCount(); n != 0 {
		c, _ := h.ex.createAt(0)
		t.Fatalf("%d order(s) reached the exchange while no reservation could "+
			"commit (first coid %q).\n\n"+
			"This is H-ORD-6: the ownership record is durable BEFORE the order "+
			"is sent, because an order whose ownership is unrecorded produces a "+
			"fill H-ORD-9 must classify as FOREIGN -- a SEV1 and a durable "+
			"operator-only global stop, declared about our own order", n, c.Coid)
	}
	if h.rig.store.Health().AllowsAdding() {
		t.Fatalf("the store reports adding authority with its writer gone; " +
			"H-STORE-3 revokes adding precisely because nothing submitted from " +
			"now on can become durable")
	}
}

// ---------------------------------------------------------------------------
// 3. The binding, inline
// ---------------------------------------------------------------------------

// TestTheBindingIsSubmittedInlineOnTheAckAndNotByALaterWalk is the
// highest-value assertion in this file.
//
// Between the exchange's ack and the committed binding, the order id is not in
// `Ownership.byOrder`, so every fill on OUR OWN order classifies UNRESOLVED and
// `risk.ApplyFills` DEFERS it. `q_local` lags the account for exactly as long
// as the gap lasts, and past 120 s the deferral escalates to a SEV2
// `FILL_UNCLASSIFIABLE` per trade, repeated. `wsx.bindListedOrders` is the
// safety net and it is one poll late BY CONSTRUCTION -- it can only learn an
// order id from the next complete orders walk, and only while the order is
// still listed, so it does not catch an order that filled and went terminal
// inside one poll interval.
//
// Three things are asserted and none of them is about `BindOrder` itself:
//
//  1. at the instant the FIRST create is sent, its reservation is already in
//     the durable unresolved set -- `Ownership.reserveCommitted` runs only after
//     the transaction commits, so this is commit-before-dispatch observed from
//     the wire;
//
//  2. at the instant the SECOND create is sent, the FIRST order is already
//     BOUND. That is a deterministic ordering rather than a race: `hstore` is a
//     FIFO with one writer, so a binding submitted inline before `placeWrite`
//     returned is necessarily ahead of the next placement's reservation, and
//     the second POST cannot happen until that reservation's permit exists. A
//     binding that were batched, deferred, or "left to the walk" could not be
//     ahead of anything;
//
//  3. no orders walk ever listed the coid, so nothing but the dispatch path
//     could have produced the binding at all. The walks DID happen -- the count
//     is asserted -- they simply had nothing to bind.
func TestTheBindingIsSubmittedInlineOnTheAckAndNotByALaterWalk(t *testing.T) {
	// listCreated stays FALSE. An exchange that listed the new order would give
	// `wsx.bindListedOrders` a way to produce the same binding, and assertion 3
	// would then prove nothing.
	//
	// The account is FLAT, and that is what makes a second dispatch happen for
	// an honest reason. §5.2 puts a flat, selected market in QUOTING, which
	// quotes S on BOTH sides symmetrically -- so the two creates are the two
	// sides of one quote. Seeding a position instead would put |q| over
	// `inv_hard` into REDUCING, where the adding side is cancelled and the only
	// write is the exit.
	//
	// This test previously seeded 8 contracts and got its second dispatch from a
	// defect: an acked order was in no aggregate until the next orders walk, so
	// §6.5 saw an empty side and re-placed the same exit every tick. That is
	// fixed -- see `pendingOrder` -- and a test that depended on it would now be
	// asserting the bug had come back.
	h := newSeamHarness(t, seamOptions{})

	var (
		obs               sync.Mutex
		firstCoid         string
		firstOrderID      string
		reservedAtFirst   bool
		boundAtSecond     bool
		sawSecondDispatch bool
	)
	h.ex.onCreate = func(idx int, c seamCreate) {
		own := h.rig.store.Ownership()
		switch idx {
		case 0:
			_, committed := own.Unresolved()[c.Coid]
			obs.Lock()
			firstCoid, firstOrderID, reservedAtFirst = c.Coid, c.OrderID, committed
			obs.Unlock()
		case 1:
			obs.Lock()
			want := firstOrderID
			obs.Unlock()
			_, bound := own.Bound(want)
			obs.Lock()
			boundAtSecond, sawSecondDispatch = bound, true
			obs.Unlock()
		}
	}

	h.start()
	h.awaitActionable()
	h.await("two orders to reach the exchange", func() bool {
		obs.Lock()
		defer obs.Unlock()
		return sawSecondDispatch
	})

	obs.Lock()
	coid, orderID := firstCoid, firstOrderID
	reserved, bound := reservedAtFirst, boundAtSecond
	obs.Unlock()

	if !reserved {
		t.Fatalf("order %s was sent while its coid %s was NOT in the durable "+
			"unresolved set. A coid enters that set in `reserveCommitted`, "+
			"which runs only after the reservation's transaction commits, so "+
			"the order was dispatched before it was recorded (H-ORD-6)",
			orderID, coid)
	}
	if !bound {
		t.Fatalf("the SECOND order was dispatched while order %s (coid %s) was "+
			"still unbound.\n\n"+
			"The store is a FIFO with one writer, so a binding submitted inline "+
			"on the ack is necessarily ahead of the next placement's "+
			"reservation, and the second dispatch cannot happen before that "+
			"reservation commits. An unbound first order here means the binding "+
			"was NOT submitted on the dispatch path -- and until a walk rebinds "+
			"it, every fill on our own order defers rather than applying, "+
			"escalating to SEV2 FILL_UNCLASSIFIABLE per trade after 120 s",
			orderID, coid)
	}

	h.await("the binding to be readable from the durable ledger", func() bool {
		got, ok := h.rig.store.Ownership().Bound(orderID)
		return ok && got == coid
	})

	if walks := h.ex.ordersWalkCount(); walks == 0 {
		t.Fatalf("no orders walk ran at all, so assertion 3 is vacuous: the " +
			"safety net has to have had the OPPORTUNITY to bind and failed to " +
			"take it")
	}
	if n := h.ex.restingCount(); n != 0 {
		t.Fatalf("the fixture listed %d resting order(s); with the created "+
			"orders listed, `wsx.bindListedOrders` could have produced this "+
			"binding and the test would prove nothing about the dispatch path",
			n)
	}

	row, found, err := h.rig.store.Reader().OwnedOrder(coid)
	if err != nil {
		t.Fatalf("reading owned_order: %v", err)
	}
	if !found || !row.Bound || row.OrderID != orderID {
		t.Fatalf("owned_order for %s reads found=%v bound=%v order_id=%q, want "+
			"a durable binding to %s: H-ORD-9 makes ownership a fact in the "+
			"ledger and never an inference at the fill",
			coid, found, row.Bound, row.OrderID, orderID)
	}
}

// ---------------------------------------------------------------------------
// 4. D3 -- one REST writer
// ---------------------------------------------------------------------------

// TestEveryRESTWriteComesFromTheOneDispatcherGoroutine is D3, H-ORD-6 and "ONE
// REST writer" -- the same requirement stated three times -- asserted as the
// goroutine count it actually is.
//
// `dispatchWorkers` is 1 and the enforcement is that there IS one goroutine, so
// the property is not visible from any value inside this process. It is visible
// from the transport, and only there: this records the goroutine id of every
// POST and DELETE and the maximum number in flight at once.
//
// Both halves matter and neither implies the other. Two writers that happened
// never to overlap would still show two ids; two writers that raced would still
// show a depth of two even if the ids were reused. Run under -race, a genuine
// second writer also trips the detector on `quote.Capacity` and
// `risk.Portfolio`, which carry no mutex by design.
//
// The scenario deliberately produces BOTH kinds of write, and the position flip
// is how. At q = +2 the exit is a NO bid, and it rests. When the exchange then
// reports q = −2, the reducing side becomes YES and that same NO order is now
// on the ADDING side of a stopped market -- so §5.2 takes it off (A8) and the
// DELETE is issued by the dispatcher rather than by any startup path. The step
// is two contracts, inside `pos_drift_hard`, so nothing here is a drift stop.
//
// The one legitimate second writer in this process is the §7.5 startup sweep,
// which issues its cancels from the goroutine that runs `serve` -- and it runs
// strictly BEFORE the dispatcher goroutine is started, so D3's concurrency
// property is untouched. This fixture starts with no resting orders precisely
// so that no startup write exists and the claim below can be exact.
func TestEveryRESTWriteComesFromTheOneDispatcherGoroutine(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		ListCreated: true,
		Positions:   map[string]string{seamTicker: "2.00"},
	})
	h.start()

	h.awaitActionable()
	h.await("the exit to reach the exchange and be listed as resting",
		func() bool { return h.ex.createCount() >= 1 && h.ex.restingCount() >= 1 })

	h.ex.setPosition(seamTicker, "-2.00")
	h.clk.Advance(h.cfg.Params.PositionPoll)

	h.await("the position poll to flip the sign of q", func() bool {
		m, ok := h.market()
		return ok && m.Q < 0
	})
	h.await("the now-adding side to be cancelled on the exchange",
		func() bool { return h.ex.deleteCount() >= 1 })

	ids := h.ex.writerIDs()
	if len(ids) != 1 {
		t.Fatalf("REST writes came from %d goroutines (%v), want exactly 1.\n\n"+
			"D3, H-ORD-6 and \"ONE REST writer\" are one requirement written "+
			"three times, and one goroutine is how it is enforced rather than "+
			"remembered. A second writer mints coids from the same run id "+
			"(H-ORD-1 permits at most one order per coid), spends the same §16 "+
			"token bucket, and touches `quote.Capacity` and `risk.Portfolio`, "+
			"neither of which carries a mutex", len(ids), ids)
	}
	for id, n := range ids {
		if id == 0 {
			t.Fatalf("the writer's goroutine id read back as 0, so the id was " +
				"never parsed and every writer would look like the same one")
		}
		if n < 2 {
			t.Fatalf("only %d write was observed, so \"they all came from one "+
				"goroutine\" is vacuous", n)
		}
	}
	if got := h.ex.maxConcurrentWrites(); got != 1 {
		t.Fatalf("%d REST writes were in flight at once, want at most 1: "+
			"`Capacity.BusyGeneral` counts what the owner believes is in "+
			"flight, and a writer that fanned out would be spending capacity "+
			"nobody is accounting for", got)
	}
	if h.ex.createCount() == 0 || h.ex.deleteCount() == 0 {
		t.Fatalf("the run produced %d creates and %d deletes; the claim is "+
			"about BOTH kinds of write and needs at least one of each",
			h.ex.createCount(), h.ex.deleteCount())
	}
}

// ---------------------------------------------------------------------------
// 5. H-Q-9 is OFF for the pilot
// ---------------------------------------------------------------------------

// TestRequotingAnExistingOrderIsAlwaysCancelConfirmPlace pins
// `AllowPlaceThenCancel = false`.
//
// It is one line in `decideSide`, it is written explicitly where the zero value
// would already do, and it is the single line between the pilot's invariant --
// at most one order of ours per side at any instant -- and two of our orders
// live at the same price band at once (pilot-plan §2.7, §7.1). H-Q-9 is a
// revenue optimisation: it closes a presence gap, and presence gaps are revenue
// (S4). The pilot pays the gap and takes the simpler invariant.
//
// The test runs `decideSide` on the test's own goroutine over a real rig, which
// is the same single-writer discipline `run.go` states: `quote.Queue`,
// `risk.Portfolio` and `core.Rig` carry no mutex, and the owner goroutine is
// their writer.
//
// The second sub-test is the one that actually pins the flag, and the reason is
// worth stating. `decideSide` also passes `Headroom: 0`, and `requoteKind`
// returns cancel-confirm-place whenever `Size > Headroom` -- so at an ordinary
// size the flag could be flipped to true with no observable effect, and a
// mutation of it would read as caught for the wrong reason. The one shape where
// the flag alone decides is a target of exactly zero, which §6.2's taper
// produces at |q| == inv_hard: SKEWED, adding side sized `size_A` = 0, still
// quoting. That is a real configuration, not a contrived one, and it is where a
// flipped flag shows.
func TestRequotingAnExistingOrderIsAlwaysCancelConfirmPlace(t *testing.T) {
	cases := []struct {
		name   string
		target num.Qty
		why    string
	}{
		{
			name:   "ordinary adding size",
			target: cfg.Default().S,
			why: "the everyday upward requote: §5.2's QUOTING row, S per side, " +
				"the touch one tick above our resting order",
		},
		{
			name:   "size_A at the taper floor",
			target: 0,
			why: "|q| == inv_hard, so §6.2's taper has reached zero while the " +
				"market is still SKEWED and still quoting. Headroom is 0 here " +
				"too, so this is the ONE shape in which AllowPlaceThenCancel " +
				"alone decides the leg structure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{})
			o := h.ownerFor()
			o.market = quote.Quoting

			h.installResting(risk.LiveOrder{
				OrderID: "EX-SEAM-1", Ticker: seamTicker, Side: quote.SideYes,
				Price4: 4000, Remaining: num.QtyFromFloat(12),
			})

			// The touch where our order already is. This call records the touch
			// and must decide nothing.
			h.installBook([][]string{{"0.4000", "20.00"}},
				[][]string{{"0.5500", "20.00"}})
			o.decideSide(0, quote.SideYes, quote.RoleAdding, tc.target)
			if n := h.rig.queue.Len(); n != 0 {
				t.Fatalf("%d intent(s) were queued for an order already AT the "+
					"touch; `Decide` returns early on an unchanged price and "+
					"`decideSide` repeats the check, because a move with an "+
					"unchanged price cancels an order to replace it with itself",
					n)
			}

			// The reference moves AGAINST us by four ticks: H-Q-6's trigger,
			// inside `stale_bid_ticks` so H-Q-8's brake is not what fires, and
			// UPWARD, which is H-Q-9's own first condition.
			h.installBook([][]string{{"0.4400", "20.00"}, {"0.4000", "20.00"}},
				[][]string{{"0.5500", "20.00"}})

			// H-Q-7: the new touch has not held for debounce_s yet.
			o.decideSide(0, quote.SideYes, quote.RoleAdding, tc.target)
			if n := h.rig.queue.Len(); n != 0 {
				t.Fatalf("%d intent(s) were queued before the new touch had "+
					"held for debounce_s (%v); H-Q-7 exists so a touch that "+
					"flickers cannot be chased", n, h.cfg.Params.Debounce)
			}

			// Past the debounce. This is the requote.
			o.decideSide(h.cfg.Params.Debounce+time.Millisecond,
				quote.SideYes, quote.RoleAdding, tc.target)

			pending := h.rig.queue.Pending()
			if len(pending) != 1 {
				t.Fatalf("the upward requote queued %d intents, want 1 (%s)",
					len(pending), tc.why)
			}
			in := pending[0]
			if in.Kind == quote.KindPlaceThenCancel {
				t.Fatalf("the requote was queued as %s.\n\n"+
					"H-Q-9 is OFF for the pilot: `decideSide` passes "+
					"AllowPlaceThenCancel = false, and that single line is what "+
					"stops two of our orders being live at once. Place-then-"+
					"cancel rests the replacement BEFORE the old order is "+
					"retired, so the aggregate on this side momentarily carries "+
					"both -- the shape H-Q-5a forbids on a reducing side "+
					"(HR-004) and which the pilot declines to reason about on "+
					"any side. Case: %s", in.Kind, tc.why)
			}
			if in.Kind != quote.KindCancelConfirmPlace {
				t.Fatalf("the requote was queued as %s, want %s (%s)",
					in.Kind, quote.KindCancelConfirmPlace, tc.why)
			}
			if in.Op() != quote.OpCancel {
				t.Fatalf("the first dispatched leg is %s, want %s: "+
					"cancel-confirm-place does not dispatch the replacement "+
					"until the cancel is confirmed absent (H-Q-9a, H-FAIL-3)",
					in.Op(), quote.OpCancel)
			}
			if in.Reason != quote.ReasonRequote {
				t.Fatalf("the intent's §6.6 row is %s, want %s",
					in.Reason, quote.ReasonRequote)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. H-DEP-5 -- one instance
// ---------------------------------------------------------------------------

// TestTheInstanceLockIsHeldForTheWholeRunAndRefusesASecondRig is H-DEP-5.
//
// > one instance only -- a PID lockfile, checked at startup. Two harnesses on
// > one account is an unrecoverable position-model conflict.
//
// Unrecoverable is the operative word: two processes produce two `q_local`
// models that are each individually consistent, each wrong, and each unable to
// tell that the fills moving the position came from the other one. Every safety
// mechanism downstream of `q` -- the reducer size, the aggregate caps,
// H-POS-2's drift detector, `inv_kill` -- is then computing against a number no
// amount of polling will correct.
//
// Three things are asserted, and the TIMING of the first is the point: the
// refusal happens while the first harness is mid-run, so what is being tested
// is a lock HELD for the run and not merely taken and dropped at construction.
// The message is asserted too, because a mutation that removed the lock would
// still make `newRig` fail -- on the second `hstore.Open`, or on something else
// -- and a test that accepted any error would read that as caught.
func TestTheInstanceLockIsHeldForTheWholeRunAndRefusesASecondRig(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	h.start()
	h.awaitActionable()

	second, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
	if err == nil {
		if second != nil {
			if cerr := second.close(context.Background()); cerr != nil {
				t.Logf("closing the second rig: %v", cerr)
			}
		}
		t.Fatalf("a SECOND harness was constructed against the lock at %s "+
			"while the first was running.\n\n"+
			"H-DEP-5: two harnesses on one account is an unrecoverable "+
			"position-model conflict, and the damage begins at the first "+
			"request each of them sends -- so the second must be refused "+
			"before it can send one", h.cfg.Paths.Lock)
	}
	if !strings.Contains(err.Error(), "H-DEP-5") ||
		!strings.Contains(err.Error(), h.cfg.Paths.Lock) {
		t.Fatalf("the second construction failed for a reason that does not "+
			"name the instance lock: %v.\n\n"+
			"A rig that failed for some other reason -- a second open of the "+
			"same database, say -- would read as caught while H-DEP-5 was not "+
			"enforced at all", err)
	}

	// And the lock is RELEASED on an orderly stop, so a restart is not blocked
	// by its own predecessor. `rig.close` releases it only after the store has
	// been dealt with: while any record is in flight this process is still the
	// one incarnation entitled to write it.
	h.stopServe()
	if err := h.rig.close(context.Background()); err != nil {
		t.Fatalf("the first harness would not stop cleanly: %v", err)
	}
	third, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
	if err != nil {
		t.Fatalf("a harness could not be constructed after the previous one "+
			"stopped cleanly: %v.\n\n"+
			"`InstanceLock.Close` releases the flock and deliberately does not "+
			"unlink the file, so the next incarnation reuses it; a stale lock "+
			"that blocked every restart would be the classic PID-file defect "+
			"reintroduced", err)
	}
	if cerr := third.close(context.Background()); cerr != nil {
		t.Errorf("closing the third rig: %v", cerr)
	}
}

// ---------------------------------------------------------------------------
// 7. H-HALT-4 -- the latch survives the process
// ---------------------------------------------------------------------------

// TestASetLatchRefusesWithoutResumeAndWindsDownWithIt is §10.4 made a refusal
// at the one point where it can be one.
//
// H-HALT-4's whole content is that the latch survives the process: the harness
// never self-clears it, so a restart into a latched state is WINDING_DOWN and
// not RUNNING, and clearing it is an OPERATOR action. HR-009 is the sequence
// this closes -- a taker fill latches WINDING_DOWN, an unrelated panic kills
// the process, `launchd KeepAlive` restarts it, and without the latch being
// read FIRST the flat markets resume adding and the halt self-clears with no
// operator action, because the supervision policy the spec mandates erased the
// safety state the spec mandates.
//
// Both halves are asserted, and the second is the one that costs money to get
// wrong: `-resume` does NOT clear anything. It starts a process that will wind
// the account down, and a harness that came up RUNNING after being resumed
// would be HR-009 with the operator's fingerprints on it.
func TestASetLatchRefusesWithoutResumeAndWindsDownWithIt(t *testing.T) {
	t.Run("refused without -resume", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Latched: true, SkipRig: true})

		r, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
		if err == nil {
			if cerr := r.close(context.Background()); cerr != nil {
				t.Logf("closing the unexpectedly-constructed rig: %v", cerr)
			}
			t.Fatalf("a harness started with the durable halt latch at %s SET "+
				"and no -resume.\n\n"+
				"H-HALT-4 makes the latch survive the process on purpose, and "+
				"§10.4 makes clearing it an operator action. A start that "+
				"ignores it is HR-009: the halt self-clears because the "+
				"supervision policy restarted the process", h.cfg.Paths.Latch)
		}
		if !strings.Contains(err.Error(), "-resume") ||
			!strings.Contains(err.Error(), h.cfg.Paths.Latch) {
			t.Fatalf("the refusal does not tell the operator what to read or "+
				"what to pass: %v", err)
		}

		// The refusal UNWOUND: the instance lock it took on the way in was
		// released, so a constructor that refuses does not leave the next start
		// refused by its own predecessor.
		resumed, err := seamNewRig(t, h.ctx, h.cfg, true, h.xch)
		if err != nil {
			t.Fatalf("the refused construction leaked something -- most likely "+
				"the instance lock -- so a -resume start is now impossible: %v",
				err)
		}
		if cerr := resumed.close(context.Background()); cerr != nil {
			t.Errorf("closing the resumed rig: %v", cerr)
		}
	})

	t.Run("with -resume it comes up WINDING_DOWN and quotes nothing", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Latched: true, Resume: true})
		h.start()

		// A13 is satisfied -- connected, snapshotted, all three endpoints
		// reconciled -- so nothing about the evidence is what is stopping this
		// harness. The account is flat, so there is no exit to keep alive
		// either, and IDLE is the whole of what a latched flat market does.
		h.awaitActionable()
		h.awaitTicks(10)

		m, ok := h.market()
		if !ok {
			t.Fatalf("no snapshot was published")
		}
		s := h.snapshot()
		if s.Global != quote.WindingDown {
			t.Fatalf("the global state is %s, want WINDING_DOWN. A14 and "+
				"H-HALT-4: a latch on disk implies WINDING_DOWN or DRAINED, and "+
				"`NextGlobal` checks it before every other rule in every state "+
				"that is not already halted. RUNNING here is the halt having "+
				"self-cleared", s.Global)
		}
		if m.State != quote.Idle {
			t.Fatalf("the market is %s, want IDLE: a global state that has "+
				"stopped adding sends the market out of its quoting states, and "+
				"with q == 0 there is nothing to reduce", m.State)
		}
		if n := h.ex.createCount(); n != 0 {
			c, _ := h.ex.createAt(0)
			t.Fatalf("%d order(s) were placed by a harness resumed onto a SET "+
				"latch (first coid %q). -resume is not a clear: it starts a "+
				"process that winds the account down", n, c.Coid)
		}
	})
}

// ---------------------------------------------------------------------------
// 8. The poll cycle's order
// ---------------------------------------------------------------------------

// TestAFillInsideAPollCycleIsNotDoubleCounted is `wsx.ApplyPortfolio`'s order
// -- orders, then fills, then POSITIONS LAST -- asserted through the composed
// process.
//
// Positions is authoritative and OVERWRITES (H-POS-1: `q_local := q_exch`).
// Applying it before the fills means applying the fills on top of a figure that
// already contains them, which double-counts every fill that landed inside the
// cycle. The consequence is not a cosmetic discrepancy: a reducer is sized from
// `q`, and HR-004's sequence is exactly a reducer sized from a doubled position
// flipping the sign of the real one.
//
// A final `q` alone cannot tell the two orders apart, because the overwrite
// lands last either way. What CAN is the drift H-POS-2 measures at the moment
// positions is applied:
//
//   - correct order: the fill has already moved `q_local` from +8 to +2, the
//     exchange says +2, drift is 0;
//   - positions first: `q_local` is still +8 when the exchange says +2, drift is
//     6, which is past `pos_drift_hard` (5) on a SINGLE poll -- a SEV1
//     `POSITION_DRIFT` and a durable global stop.
//
// So the assertions are: `q` is the exchange's figure, the fill is recorded
// exactly once, no `POSITION_DRIFT` was raised, and the harness is still
// RUNNING.
func TestAFillInsideAPollCycleIsNotDoubleCounted(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Positions: map[string]string{seamTicker: "8.00"},
	})
	h.start()

	h.awaitActionable()
	h.await("the exit to reach the exchange",
		func() bool { return h.ex.createCount() >= 1 })
	first, ok := h.ex.createAt(0)
	if !ok {
		t.Fatalf("no create was recorded")
	}
	// The fill can only be classified as OURS once the binding is durable, and
	// this fixture never lists the order, so the inline binding of `placeWrite`
	// is the only thing that can produce it.
	h.await("the dispatch-path binding to commit", func() bool {
		coid, bound := h.rig.store.Ownership().Bound(first.OrderID)
		return bound && coid == first.Coid
	})

	const tradeID = "SEAM-TRADE-1"
	h.ex.addFill(map[string]any{
		"fill_id":           "SEAM-FILL-1",
		"trade_id":          tradeID,
		"order_id":          first.OrderID,
		"ticker":            seamTicker,
		"side":              "no",
		"yes_price_dollars": "0.4500",
		"no_price_dollars":  "0.5500",
		"count":             "6.00",
		"is_taker":          false,
		// S2: maker fees are $0.00, and a fee we cannot read is one of
		// H-ORD-8's two independent witnesses going silent.
		"fee_cost": "0.0000",
		"ts":       fmt.Sprintf("%d", h.clk.wallMs()),
	})
	// The exchange's own figure ALREADY contains the fill: six contracts of a
	// NO bid against a +8 YES position leaves +2. That is the whole scenario --
	// the walk that reports the fill and the walk that reports the position
	// describe the same event.
	h.ex.setPosition(seamTicker, "2.00")

	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("the poll carrying the fill to be applied", func() bool {
		m, ok := h.market()
		return ok && m.Q == num.QtyFromFloat(2)
	})

	m, _ := h.market()
	if want := num.QtyFromFloat(2); m.Q != want {
		t.Fatalf("q is %s after a poll whose fills walk and positions walk "+
			"described the same 6 contracts, want %s.\n\n"+
			"H-POS-1 makes the positions walk authoritative and it is applied "+
			"LAST for that reason: applied first, the cycle's own fills are "+
			"subtracted from a figure that already accounts for them",
			m.Q.Wire(), want.Wire())
	}

	h.await("the fill to be recorded in the ledger", func() bool {
		_, found, err := h.rig.store.Reader().Fill(tradeID)
		return err == nil && found
	})
	row, _, err := h.rig.store.Reader().Fill(tradeID)
	if err != nil {
		t.Fatalf("reading our_fill: %v", err)
	}
	if want := num.QtyFromFloat(6); row.Count != want {
		t.Fatalf("our_fill records %s contracts for %s, want %s",
			row.Count.Wire(), tradeID, want.Wire())
	}
	if row.OrderID != first.OrderID {
		t.Fatalf("our_fill attributes %s to order %q, want %q; H-ORD-6 makes "+
			"trade_id the join against rig.db and the order id is what ties it "+
			"to our own ledger", tradeID, row.OrderID, first.OrderID)
	}

	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the global state is %s, want RUNNING.\n\n"+
			"That is the signature of positions having been applied BEFORE the "+
			"fills: q_local is still +8 when the exchange says +2, the "+
			"single-poll disagreement is 6, pos_drift_hard is %s, and the "+
			"harness declares a durable global stop about a discrepancy it "+
			"created itself", s.Global, h.cfg.Params.PosDriftHard.Wire())
	}
	for _, class := range h.anomalyClasses() {
		if class == "POSITION_DRIFT" {
			t.Fatalf("a POSITION_DRIFT anomaly was raised on a cycle in which " +
				"our model and the exchange agreed exactly; see above for why " +
				"that is the positions-first ordering")
		}
	}
}

// ---------------------------------------------------------------------------
// 9. H-TOP-5 -- the snapshot advances on every tick
// ---------------------------------------------------------------------------

// TestTheSnapshotSequenceAdvancesOnEveryTickIncludingHalted is H-TOP-5 and I3.
//
// I2 makes the monitor unstoppable but does NOT make it truthful:
//
//	The owner publishes q = 0, then deadlocks. One second later a resting
//	order fills. The monitor re-reads the same pointer forever, stamps a fresh
//	snap row every second, and pushes hourly heartbeats reporting q = 0 --
//	while real inventory grows unobserved. A5 passes at every tick. F18 never
//	fires, because the process and its heartbeat are both alive.
//
// That is probebot.py's observable -- confident silence about live risk --
// reached by a different route than probebot.py's `break`. `Seq` is the fix,
// and it only works if the owner publishes UNCONDITIONALLY.
//
// The halted state is the case that matters and it is the one an optimisation
// would break first: a WINDING_DOWN harness with a flat book decides nothing on
// almost every tick, so a `publish` guarded by "something changed" would look
// perfectly reasonable and would starve the stall detector exactly where the
// operator most needs it. This runs a harness resumed onto a set latch -- so it
// quotes nothing, ever -- and asserts the sequence still advances, strictly and
// without repeats, across many ticks.
func TestTheSnapshotSequenceAdvancesOnEveryTickIncludingHalted(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Latched: true, Resume: true})
	h.start()

	h.awaitActionable()
	if s := h.snapshot(); s.Global != quote.WindingDown {
		t.Fatalf("the fixture is not halted (global %s), so this proves "+
			"nothing about a harness that is deciding nothing", s.Global)
	}

	const samples = 6
	seen := make([]uint64, 0, samples)
	last := h.snapSeq()
	for i := 0; i < samples; i++ {
		want := last + 1
		h.await(fmt.Sprintf("snapshot seq to reach %d", want),
			func() bool { return h.snapSeq() >= want })

		s := h.snapshot()
		if s.Seq <= last {
			t.Fatalf("snapshot seq went %d -> %d; H-TOP-5 requires it to "+
				"advance by at least one on every publication, and the "+
				"monitor's whole stall detector is that number moving",
				last, s.Seq)
		}
		if s.Global != quote.WindingDown {
			t.Fatalf("the global state left WINDING_DOWN (now %s); H-HALT-4 "+
				"offers no path back without an operator", s.Global)
		}
		if len(s.Markets) != 1 || s.Markets[0].Ticker != seamTicker {
			t.Fatalf("the snapshot describes %d market(s); A5 requires a "+
				"sample for every selected market at every tick", len(s.Markets))
		}
		if s.PubWallMs == 0 {
			t.Fatalf("the snapshot carries no wall stamp")
		}
		seen = append(seen, s.Seq)
		last = s.Seq
	}

	if seen[len(seen)-1] <= seen[0] {
		t.Fatalf("the sequence did not advance across %d observations: %v; a "+
			"publisher made conditional on the owner having decided something "+
			"stops exactly here, and the monitor then re-reports a snapshot "+
			"that was true once", samples, seen)
	}
}

// ---------------------------------------------------------------------------
// 10. §9 -- an unread schedule stops ADDING and nothing else
// ---------------------------------------------------------------------------

// TestAnUnknownCloseStopsAddingAndLeavesTheExitAlive is H-CLOSE-0's escalation
// and I1's asymmetry, in one pair.
//
// `quote.MarketInput.HasClose` states the rule this is about: "a market whose
// close_time we do not know is one we cannot enforce a lead on, and H-CLOSE-0
// makes that the caller's problem to escalate rather than this function's to
// assume away." The caller is `owner.closeUnknown`, and escalating means
// stopping.
//
// The direction is the easy thing to get backwards, and getting it backwards is
// expensive in both directions. Quoting on rests an adding order into a market
// that may close before the next poll, with no lead enforced and H-CLOSE-3's
// final cancel never run. Cancelling EVERYTHING takes the exit off a position
// we still hold -- which is the inversion §5.2 states outright: "there is no
// HALTED per-market state that cancels everything. A market-level halt trigger
// sends the market to REDUCING, which keeps the exit alive."
//
// So: adding side off, exit untouched, and a known close is the control that
// makes both of those attributable to the schedule and to nothing else.
//
// `Actionable` is false in both halves -- `Gate.noteTruth` is private precisely
// so no test can forge a reconciliation -- so no PLACEMENT decision is taken
// here. That is the point of the pairing: what differs between the two halves
// is the state and the CANCEL, and cancels are the half of I1 that is not
// gated.
func TestAnUnknownCloseStopsAddingAndLeavesTheExitAlive(t *testing.T) {
	// q = +1, comfortably under inv_soft, so the inventory ladder alone would
	// leave this market QUOTING. Anything that moves it is the schedule.
	const heldYes = 1

	t.Run("no schedule read", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{})
		o := h.ownerFor()
		h.connectGate()
		h.installBook([][]string{{"0.4000", "20.00"}},
			[][]string{{"0.5500", "20.00"}})
		h.installPosition(num.QtyFromFloat(heldYes))
		h.installResting(
			risk.LiveOrder{OrderID: "EX-ADD", Ticker: seamTicker,
				Side: quote.SideYes, Price4: 4000,
				Remaining: num.QtyFromFloat(12)},
			risk.LiveOrder{OrderID: "EX-EXIT", Ticker: seamTicker,
				Side: quote.SideNo, Price4: 5500,
				Remaining: num.QtyFromFloat(1)},
		)

		if o.hasClose {
			t.Fatalf("the owner starts with a close it was never told; every " +
				"§9 field is filled by a schedule poll and none of them has a " +
				"safe default")
		}
		o.evaluate(0)

		if o.market != quote.Reducing {
			t.Fatalf("the market is %s with no close_time read, want REDUCING. "+
				"H-CLOSE-0 hands an unenforceable lead to the caller to "+
				"escalate, and quoting on would rest an adding order into a "+
				"market that may close before the next poll with no lead "+
				"enforced and no final cancel run", o.market)
		}

		// The ADDING side comes off (§5.2's REDUCING row, A8).
		adds := seamCancelsOn(h.rig.queue, quote.SideYes)
		if len(adds) != 1 {
			t.Fatalf("%d cancel intents were queued for the ADDING side, want "+
				"1; a stopped market's adding side is cancelled and confirmed "+
				"absent, not merely left unrefreshed", len(adds))
		}
		if adds[0].Role != quote.RoleAdding {
			t.Fatalf("the adding-side cancel is classified %s", adds[0].Role)
		}

		// The EXIT is untouched, in the queue and in the risk model.
		if exits := seamCancelsOn(h.rig.queue, quote.SideNo); len(exits) != 0 {
			t.Fatalf("%d cancel intent(s) were queued for the REDUCING side.\n\n"+
				"I1: every stop path in this system stops ADDING risk and none "+
				"of them stops reducing it. Cancelling the exit because the "+
				"schedule could not be read is a stop condition that strands "+
				"the position it was protecting", len(exits))
		}
		if got := o.atRisk(quote.SideNo); got != num.QtyFromFloat(heldYes) {
			t.Fatalf("the exit's aggregate is %s, want %s: nothing here may "+
				"retire it from the risk model", got.Wire(),
				num.QtyFromFloat(heldYes).Wire())
		}
	})

	t.Run("a schedule read far from the close", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{})
		o := h.ownerFor()
		h.connectGate()
		h.installBook([][]string{{"0.4000", "20.00"}},
			[][]string{{"0.5500", "20.00"}})
		h.installPosition(num.QtyFromFloat(heldYes))
		h.installResting(
			risk.LiveOrder{OrderID: "EX-ADD", Ticker: seamTicker,
				Side: quote.SideYes, Price4: 4000,
				Remaining: num.QtyFromFloat(12)},
			risk.LiveOrder{OrderID: "EX-EXIT", Ticker: seamTicker,
				Side: quote.SideNo, Price4: 5500,
				Remaining: num.QtyFromFloat(1)},
		)

		// What a complete schedule read would leave behind. Far outside
		// `close_lead` (1h) and not an early-closing market, so §9's ladder
		// contributes nothing and the only difference from the case above is
		// that the close is KNOWN.
		o.closeAt = time.Now().Add(24 * time.Hour)
		o.hasClose = true
		o.scheduleEver = true

		o.evaluate(0)

		if o.market != quote.Quoting {
			t.Fatalf("the market is %s with a close 24h away and q = %d, want "+
				"QUOTING; if this is not QUOTING then the previous sub-test's "+
				"REDUCING was not attributable to the missing schedule",
				o.market, heldYes)
		}
		if n := h.rig.queue.Len(); n != 0 {
			t.Fatalf("%d intent(s) were queued for a market that is quoting "+
				"normally: nothing of ours is being taken off, on either side",
				n)
		}
	})

	// The third case is the DANGEROUS one, and it is the reason the reading
	// expires rather than merely being recorded.
	//
	// An absent schedule is conspicuous: `hasClose` is false and every rule that
	// consults it says so. A schedule read once and never refreshed is not --
	// it carries a real timestamp, every lead computed from it reads as
	// enforced, and nothing in the arithmetic distinguishes it from a current
	// one. §9 states outright that "neither is assumed static", and HR-017 is
	// that difference costing a close: at `schedule_poll_s` = 300 with
	// `final_lead` = 60s, a `close_time` that moved from 17:00 to 12:03 at
	// 12:00:01 was next observed at 12:05 -- after the close. Neither the close
	// lead nor the final cancel ever ran.
	//
	// The expiry is H-CLOSE-0's own rule turned on the reading: "a lead cannot
	// be enforced unless the schedule is read at least twice within it", so a
	// sample older than `final_lead` cannot enforce `final_lead`, whatever value
	// it carries.
	t.Run("a schedule read that has gone stale", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{})
		o := h.ownerFor()
		h.connectGate()
		h.installBook([][]string{{"0.4000", "20.00"}},
			[][]string{{"0.5500", "20.00"}})
		h.installPosition(num.QtyFromFloat(heldYes))
		h.installResting(
			risk.LiveOrder{OrderID: "EX-ADD", Ticker: seamTicker,
				Side: quote.SideYes, Price4: 4000,
				Remaining: num.QtyFromFloat(12)},
			risk.LiveOrder{OrderID: "EX-EXIT", Ticker: seamTicker,
				Side: quote.SideNo, Price4: 5500,
				Remaining: num.QtyFromFloat(1)},
		)

		// The SAME complete read as the sub-test above, so the only difference
		// between quoting normally and stopping is its age.
		o.closeAt = time.Now().Add(24 * time.Hour)
		o.hasClose = true
		o.scheduleEver = true
		o.scheduleRead = h.clk.monoNow()

		// Past `final_lead`, with no further read. In production this is two
		// consecutive failed schedule polls; one missed poll leaves the reading
		// 30-60s old and still usable.
		h.clk.Advance(h.cfg.Params.FinalLead + time.Second)
		o.evaluate(h.clk.monoNow())

		if _, has := o.untilClose(); has {
			t.Fatalf("a schedule read %v ago still reports a usable close "+
				"against final_lead %v. H-CLOSE-0 requires the schedule be "+
				"sampled at least twice within the lead it enforces, and "+
				"`cfg.Validate` already refuses a CONFIGURATION that cannot -- "+
				"a READING older than the lead fails the same rule",
				h.cfg.Params.FinalLead+time.Second, h.cfg.Params.FinalLead)
		}
		if o.market != quote.Reducing {
			t.Fatalf("the market is %s on a stale schedule, want REDUCING. A "+
				"stale close_time is more dangerous than an absent one because "+
				"it looks exactly like a good one: the lead reads as enforced "+
				"and the market may already have moved its close (HR-017)",
				o.market)
		}
		// I1 again: the adding side comes off, the exit does not.
		if adds := seamCancelsOn(h.rig.queue, quote.SideYes); len(adds) != 1 {
			t.Fatalf("%d cancel intents were queued for the ADDING side, want 1",
				len(adds))
		}
		if exits := seamCancelsOn(h.rig.queue, quote.SideNo); len(exits) != 0 {
			t.Fatalf("%d cancel intent(s) were queued for the REDUCING side; a "+
				"schedule this process could not refresh is not a reason to "+
				"strand the position it was protecting", len(exits))
		}
	})
}

// ---------------------------------------------------------------------------
// 11. H-CLOSE-4 -- the operator's early backoff
// ---------------------------------------------------------------------------

// TestTheEarlyCloseBackoffStopsAddingOnlyInsideItsOwnWindow is `earlyCloseDue`,
// and the two things it must NOT do are as important as the one it must.
//
// §16's `close_lead` is 1h, cut from 4h by HR-011 to bound how long a resting
// reducer faces a stale book. That trade is made against a close we can see
// coming. `can_close_early` markets are the ones we cannot: they settle on
// external information at a moment no schedule predicts, and 192 of the 200
// active LIP programmes carry the flag -- so this is the ordinary case, not the
// exception. The operator's answer is to stop ADDING four hours out in those
// markets rather than one.
//
// Three cases, and the last two are the guards:
//
//   - `can_close_early` and inside the window: stop. Adding side off.
//   - `can_close_early` and outside it: no stop. A backoff that fired at any
//     distance from the close would take every early-closing market -- which is
//     almost all of them -- out of QUOTING permanently.
//   - not `can_close_early`, at the same distance: no stop. The flag is what
//     the rule is conditioned on; ignoring it would apply an unpredictable
//     market's backoff to a predictable one and forfeit three hours of reward
//     per market per day for nothing.
//
// The exit survives in all three. §5.2's answer to a market-scoped stop is
// REDUCING, never a state that cancels everything, and this rule is expressed
// as a stop precisely so that it inherits that.
func TestTheEarlyCloseBackoffStopsAddingOnlyInsideItsOwnWindow(t *testing.T) {
	const heldYes = 1

	cases := []struct {
		name          string
		canCloseEarly bool
		untilClose    time.Duration
		wantDue       bool
		wantState     quote.MarketState
		why           string
	}{
		{
			name: "early-closing, inside the backoff", canCloseEarly: true,
			untilClose: 3 * time.Hour, wantDue: true,
			wantState: quote.Reducing,
			why: "the market can settle on external information at any moment " +
				"and we are inside the four hours the operator chose to stop " +
				"adding in",
		},
		{
			name: "early-closing, outside the backoff", canCloseEarly: true,
			untilClose: 8 * time.Hour, wantDue: false,
			wantState: quote.Quoting,
			why: "a backoff that fired at any distance would take 96% of the " +
				"universe out of QUOTING for its whole life",
		},
		{
			name: "not early-closing, same distance", canCloseEarly: false,
			untilClose: 3 * time.Hour, wantDue: false,
			wantState: quote.Quoting,
			why: "the flag is what the rule is conditioned on; a predictable " +
				"close is what §16's close_lead already covers",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{})
			o := h.ownerFor()
			h.connectGate()
			h.installBook([][]string{{"0.4000", "20.00"}},
				[][]string{{"0.5500", "20.00"}})
			h.installPosition(num.QtyFromFloat(heldYes))
			h.installResting(
				risk.LiveOrder{OrderID: "EX-ADD", Ticker: seamTicker,
					Side: quote.SideYes, Price4: 4000,
					Remaining: num.QtyFromFloat(12)},
				risk.LiveOrder{OrderID: "EX-EXIT", Ticker: seamTicker,
					Side: quote.SideNo, Price4: 5500,
					Remaining: num.QtyFromFloat(1)},
			)

			o.closeAt = time.Now().Add(tc.untilClose)
			o.hasClose = true
			o.canCloseEarly = tc.canCloseEarly
			o.scheduleEver = true

			if h.cfg.EarlyCloseLead != defaultEarlyCloseLead {
				t.Fatalf("the fixture's early backoff is %v, want the %v this "+
					"case is written against",
					h.cfg.EarlyCloseLead, defaultEarlyCloseLead)
			}
			if got := o.earlyCloseDue(tc.untilClose, true); got != tc.wantDue {
				t.Fatalf("earlyCloseDue(%v, can_close_early=%v) = %v, want %v "+
					"against a %v backoff: %s", tc.untilClose, tc.canCloseEarly,
					got, tc.wantDue, h.cfg.EarlyCloseLead, tc.why)
			}
			// And it never fires on a close we do not have: an unread schedule
			// is already escalated by `closeUnknown`, and manufacturing a stop
			// from a number we do not hold would silence that with an answer.
			if o.earlyCloseDue(tc.untilClose, false) {
				t.Fatalf("earlyCloseDue fired with hasClose = false; the " +
					"backoff is arithmetic on a close_time, and there is none")
			}

			o.evaluate(0)

			if o.market != tc.wantState {
				t.Fatalf("the market is %s, want %s (%s)",
					o.market, tc.wantState, tc.why)
			}
			adds := seamCancelsOn(h.rig.queue, quote.SideYes)
			if tc.wantDue && len(adds) != 1 {
				t.Fatalf("%d cancel intents for the ADDING side inside the "+
					"backoff, want 1: the whole content of the rule is that the "+
					"adding side comes off early", len(adds))
			}
			if !tc.wantDue && len(adds) != 0 {
				t.Fatalf("%d cancel intents for the ADDING side outside the "+
					"backoff, want 0 (%s)", len(adds), tc.why)
			}

			// The exit survives in EVERY case.
			if exits := seamCancelsOn(h.rig.queue, quote.SideNo); len(exits) != 0 {
				t.Fatalf("%d cancel intent(s) for the REDUCING side. §5.2: a "+
					"market-level trigger sends the market to REDUCING, "+
					"\"never to a state that cancels everything, which is the "+
					"inversion the whole design turns on\"", len(exits))
			}
			if got := o.atRisk(quote.SideNo); got != num.QtyFromFloat(heldYes) {
				t.Fatalf("the exit's aggregate is %s, want %s",
					got.Wire(), num.QtyFromFloat(heldYes).Wire())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 12. The stage-first-sent wedge
// ---------------------------------------------------------------------------

// TestAnIncompleteWriteReleasesItsIntentsRatherThanWedgingTheSide is the least
// obvious rule in `run.go`, and the failure it prevents is silent and permanent.
//
// `Queue.commit` advances a DEPENDENT intent's first leg to `StageFirstSent` at
// DEQUEUE -- "that one line is H-Q-9a: dispatch is not confirmation" -- and
// leaves the intent in the queue. `Intent.Dispatchable()` is false at that
// stage, and `Conditions.stillWanted` deliberately never drops such an entry
// ("a write cannot be un-sent"). So an intent whose write did not reach a
// terminal answer is not merely delayed: it occupies its (market, side)
// FOREVER, it can never be selected again, and `owner.enqueue`'s duplicate
// check then refuses every replacement for that side. The market stops quoting
// for the life of the process and nothing reports it.
//
// Three paths release the intents, and each is asserted separately:
//
//   - `applyWriteResult` when the write did not complete at all (`res.Err`);
//   - `applyWriteResult` on a cancel that did NOT come back verified absent --
//     only `ConfirmAbsent` moves a cancel-confirm-place off `StageFirstSent`,
//     and that is exactly what did not happen;
//   - `pump`'s default branch, where the dispatcher did not take the request.
//
// Every case asserts RECOVERY and not merely the drop. A fix that dropped the
// intent while leaving the duplicate check blocking would pass a `Len() == 0`
// assertion and leave the side just as dead, so each case re-enqueues the same
// decision §6.5 would take on the next tick and requires it to be admitted.
//
// And the deliberate ASYMMETRY is pinned: the INTENT is released, the ORDER
// stays in the risk model. H-FAIL-3 -- a cancel we requested and did not see
// confirmed is a live, fillable order, and it keeps its quantity in every
// aggregate cap until the exchange says otherwise.
func TestAnIncompleteWriteReleasesItsIntentsRatherThanWedgingTheSide(t *testing.T) {
	// seamWedge sets up one owner with an order of ours resting on the yes
	// side, a cancel-confirm-place intent enqueued, and that intent dispatched
	// -- which is to say parked at StageFirstSent with nothing but a
	// confirmation able to move it.
	seamWedge := func(t *testing.T) (*seamHarness, *owner, quote.Intent) {
		t.Helper()
		h := newSeamHarness(t, seamOptions{})
		o := h.ownerFor()
		o.market = quote.Quoting

		h.installResting(risk.LiveOrder{
			OrderID: "EX-SEAM-WEDGE", Ticker: seamTicker, Side: quote.SideYes,
			Price4: 4000, Remaining: num.QtyFromFloat(12),
		})

		in := quote.Intent{
			Market: seamTicker, Side: quote.SideYes, Role: quote.RoleAdding,
			Kind: quote.KindCancelConfirmPlace, Reason: quote.ReasonRequote,
		}
		o.enqueue(0, in)
		if n := h.rig.queue.Len(); n != 1 {
			t.Fatalf("the intent was not admitted (queue length %d)", n)
		}
		return h, o, in
	}

	t.Run("a write that did not complete", func(t *testing.T) {
		h, o, in := seamWedge(t)

		d, ok := h.rig.queue.Dequeue(o.conditions(0), o.capacity)
		if !ok {
			t.Fatalf("the cancel-confirm-place's first leg was not dispatchable")
		}
		if d.Op != quote.OpCancel {
			t.Fatalf("the first dispatched leg is %s, want %s",
				d.Op, quote.OpCancel)
		}
		if n := h.rig.queue.Len(); n != 1 {
			t.Fatalf("a dependent intent left the queue on dispatch (%d "+
				"remaining); H-Q-9a keeps it for its second leg", n)
		}

		o.applyWriteResult(writeResult{
			Req: writeRequest{
				IDs: d.IDs, Market: seamTicker, Side: quote.SideYes,
				Role: quote.RoleAdding, Op: quote.OpCancel, Grant: d.Grant,
			},
			Err: errors.New("the coid reservation never became durable"),
		})

		seamAssertReleased(t, h, o, in, "a write that reported an error")

		var classes []string
		for _, a := range h.takeRaised() {
			classes = append(classes, a.Class)
		}
		if !seamContains(classes, "WRITE_FAILED") {
			t.Fatalf("a write that did not complete raised %v; the release is "+
				"silent otherwise, and a side that stopped quoting because a "+
				"write failed once is exactly what an operator has to be able "+
				"to find", classes)
		}
	})

	t.Run("a cancel that did not come back verified absent", func(t *testing.T) {
		h, o, in := seamWedge(t)

		d, ok := h.rig.queue.Dequeue(o.conditions(0), o.capacity)
		if !ok {
			t.Fatalf("the cancel-confirm-place's first leg was not dispatchable")
		}
		atRiskBefore := o.atRisk(quote.SideYes)
		if atRiskBefore <= 0 {
			t.Fatalf("nothing of ours is at risk on the yes side, so H-FAIL-3 " +
				"has nothing to be asserted about")
		}

		o.applyWriteResult(writeResult{
			Req: writeRequest{
				IDs: d.IDs, Market: seamTicker, Side: quote.SideYes,
				Role: quote.RoleAdding, Op: quote.OpCancel, Grant: d.Grant,
				Orders: []rest.Order{{
					OrderID: "EX-SEAM-WEDGE", Ticker: seamTicker,
					Side: quote.SideYes, Remaining: num.QtyFromFloat(12),
				}},
			},
			// A COMPLETE verifying read that still found the order resting.
			// `Absent` is the only thing that may reach `ConfirmAbsent`, and a
			// 2xx is a cancel REQUEST: a cancel-requested order is still live
			// and still fillable (H-ORD-4, H-FAIL-3).
			Sweep: rest.SweepResult{
				Walk:  rest.Walk{Outcome: rest.WalkComplete},
				Clean: false,
			},
			Absent: false,
			Sent:   true,
		})

		seamAssertReleased(t, h, o, in, "a cancel that was not verified absent")

		if got := o.atRisk(quote.SideYes); got != atRiskBefore {
			t.Fatalf("the aggregate at risk on the yes side went %s -> %s.\n\n"+
				"Releasing the INTENT and keeping the ORDER are the two halves "+
				"of the same reading: H-FAIL-3 makes a cancel we requested and "+
				"did not see confirmed a LIVE, fillable order, and it keeps its "+
				"quantity in every aggregate cap (H-Q-5b, H-CAP-7) until the "+
				"exchange says otherwise",
				atRiskBefore.Wire(), got.Wire())
		}

		var classes []string
		for _, a := range h.takeRaised() {
			classes = append(classes, a.Class)
		}
		if !seamContains(classes, "CANCEL_UNVERIFIED") {
			t.Fatalf("an unverified cancel raised %v, want CANCEL_UNVERIFIED",
				classes)
		}
	})

	t.Run("the dispatcher did not take the request", func(t *testing.T) {
		h, o, in := seamWedge(t)

		before := o.capacity
		// Unbuffered and unread: `pump`'s non-blocking send takes the default
		// branch, which is the third path. Nothing left the process.
		o.pump(0, make(chan writeRequest))

		seamAssertReleased(t, h, o, in, "a request the dispatcher did not take")

		if o.capacity != before {
			t.Fatalf("capacity went %+v -> %+v after a write that never left "+
				"the process; the worker slot and the §16 token both come back, "+
				"and charging the bucket for a write that did not happen lets a "+
				"busy dispatcher spend the reducer's share on nothing at all",
				before, o.capacity)
		}
		if o.inflight != nil {
			t.Fatalf("the owner believes a write is in flight after the " +
				"dispatcher declined it; nothing would ever clear it and the " +
				"pump would never run again")
		}
	})
}

// seamAssertReleased is the common half of the three cases above: the wedge is
// gone AND the side can quote again.
func seamAssertReleased(t *testing.T, h *seamHarness, o *owner,
	in quote.Intent, what string) {

	t.Helper()
	for _, pending := range h.rig.queue.Pending() {
		if pending.Stage() == quote.StageFirstSent {
			t.Fatalf("after %s, an intent is still parked at %s on %s/%s.\n\n"+
				"Nothing but a confirmation moves it from there, and the "+
				"confirmation is exactly what did not arrive. It is not "+
				"dispatchable, `stillWanted` will never drop it, and "+
				"`enqueue`'s duplicate check refuses every replacement for that "+
				"side -- so this market silently stops quoting for the life of "+
				"the process", what, pending.Stage(), pending.Market,
				pending.Side)
		}
	}
	if n := h.rig.queue.Len(); n != 0 {
		t.Fatalf("after %s the queue still holds %d intent(s)", what, n)
	}

	// The recovery, which is the assertion that matters. §6.5 re-decides on the
	// next tick and re-enqueues if the decision still holds; a release that
	// dropped the intent while leaving the side blocked would pass every check
	// above and be just as dead.
	o.enqueue(time.Second, in)
	if n := h.rig.queue.Len(); n != 1 {
		t.Fatalf("after %s, §6.5's next decision for %s/%s was REFUSED "+
			"(queue length %d).\n\n"+
			"Dropping the wedged intent is only half the repair: `enqueue` "+
			"refuses a second intent for a market/side/op that already has one "+
			"pending, so a release that left the entry in place -- or that "+
			"dropped it and left something else holding the key -- leaves the "+
			"side unable to quote for the life of the process",
			what, in.Market, in.Side, n)
	}
}

// TestAnAckedOrderOccupiesTheAggregateBeforeAnyOrdersWalk is H-Q-5b applied to
// the window this composition creates and nothing else covers.
//
// `risk.Portfolio.ReplaceOrders` is wholesale from a COMPLETE orders walk
// (H-POS-4), so an order this process just placed is absent from
// `Portfolio.LiveOrders` until the next poll -- up to `position_poll_s`. It is
// also not `inflight` any more, because the dispatcher has returned. If nothing
// else carried it, `restingOn` would report the side EMPTY.
//
// An empty side is not a quiet side. §6.5's answer to "nothing of ours here" is
// presence restoration, deliberately WITHOUT the debounce -- `Decide` says so:
// "a presence gap is revenue (S4), and waiting 250ms to open one costs more than
// it saves." So the owner would re-place the same order on every tick, bounded
// only by `write_burst`, until a walk finally listed one of them.
//
// Measured on this harness before the fix: one +8 position produced ten
// identical 8-contract exits in about three seconds, none cancelled and none
// filled -- 80 contracts of reducer against 8 contracts of inventory. H-Q-5a
// caps a reducing order's aggregate at |q| and A12 forbids any fill sequence
// that changes the sign of q via a reducer; that sequence does both.
//
// `ListCreated` is FALSE, so no orders walk ever lists the new order. That is
// the whole point: it holds the window open indefinitely, so the assertion is
// about the aggregate carrying the order rather than about a poll arriving in
// time to rescue it.
func TestAnAckedOrderOccupiesTheAggregateBeforeAnyOrdersWalk(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Positions: map[string]string{seamTicker: "8.00"},
	})

	h.start()
	h.awaitActionable()
	h.await("the exit to reach the exchange", func() bool {
		return h.ex.createCount() >= 1
	})

	// Many ticks, each of which re-runs §6.5 over the same side. The defect
	// produced one order per tick until the bucket emptied; the fix produces
	// none, because the side is not empty.
	h.awaitTicks(12)

	got := h.ex.allCreates()
	if len(got) != 1 {
		var coids []string
		for _, c := range got {
			coids = append(coids, c.Coid)
		}
		t.Fatalf("%d orders reached the exchange over twelve owner ticks, want "+
			"exactly 1: %v\n\n"+
			"An acked order that no aggregate can see makes its side read empty, "+
			"and §6.5 restores presence on an empty side every tick without a "+
			"debounce. Against |q| = 8 that is a reducing aggregate of %d "+
			"contracts' worth of orders, which H-Q-5a caps at |q| and A12 says "+
			"no fill sequence may use to flip the sign of q",
			len(got), coids, len(got)*8)
	}

	// And the quantity is genuinely IN the aggregate, not merely un-re-placed
	// for some unrelated reason.
	// And the quantity is genuinely IN the aggregate, rather than the side
	// having gone quiet for some unrelated reason.
	//
	// Read from the published snapshot and NOT from a fresh `ownerFor()`: the
	// owner that placed the order is the one inside `serve`, and a second owner
	// built over the same rig has its own empty `pending` map. The snapshot is
	// the only honest window onto the serving owner's state -- which is I2's
	// whole design, the monitor reaching the owner through one atomic pointer
	// and nothing else.
	parsed, ok := rest.ParseCoid(got[0].Coid)
	if !ok {
		t.Fatalf("the dispatched coid %q does not parse as ours", got[0].Coid)
	}
	m, live := h.market()
	if !live {
		t.Fatal("no market snapshot was published")
	}
	if at := m.Sides[parsed.Side].AtRisk; at <= 0 {
		t.Fatalf("the %s side publishes %s at risk after an acked order; "+
			"H-Q-5b evaluates every size cap against RESTING + SENDING + "+
			"UNKNOWN + unconfirmed-cancel, and an acked order is in that set "+
			"from the ack until a walk lists it", parsed.Side, at.Wire())
	}
}

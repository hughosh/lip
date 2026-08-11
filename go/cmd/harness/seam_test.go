package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"lip/feed"
	"lip/harness/cfg"
	"lip/harness/lifecycle"
	"lip/harness/netx"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
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

	// ackFill is the `fill_count` a create acknowledgement reports, as a wire
	// decimal. Empty means "0.00", which is the ordinary case.
	//
	// It is the only way to reach §8.2's ack path from a composed test: the
	// exchange answers a marketable-but-post-only create by filling part of it
	// inside the acknowledgement, and that quantity moves `q` before any fills
	// walk has run.
	ackFill string

	// positionsBroken makes GET /portfolio/positions answer 500, which is how a
	// test produces an INCOMPLETE positions walk without a second fake. The
	// result does not `Replaces()`, so H-PAGE-1 makes the previous reading
	// stale rather than empty and nothing at all is applied.
	positionsBroken bool

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
	filled := f.ackFill
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
	remaining := c.Count
	if filled == "" {
		filled = "0.00"
	} else {
		// The two counts are kept consistent, because they are on the wire
		// together and an ack claiming a full remainder alongside a fill is a
		// response no exchange sends.
		req, errQ := num.ParseQty(c.Count)
		got, errF := num.ParseQty(filled)
		if errQ != nil || errF != nil {
			return rest.Response{}, fmt.Errorf("seam ack counts %q/%q: %v/%v",
				c.Count, filled, errQ, errF)
		}
		if got > req {
			got = req
		}
		remaining = (req - got).Wire()
	}
	ack, err := json.Marshal(map[string]any{
		"order_id":        c.OrderID,
		"client_order_id": c.Coid,
		"remaining_count": remaining,
		"fill_count":      filled,
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
	if f.positionsBroken {
		f.mu.Unlock()
		return rest.Response{Status: 500, Body: []byte(`{"error":"seam"}`)}, nil
	}
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

func (f *seamExchange) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
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

func (f *seamExchange) setAckFill(count string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackFill = count
}

func (f *seamExchange) breakPositions(broken bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positionsBroken = broken
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
	// ReadOnly disarms BOTH of H-VER-1's keys: no `-live`, and no `live_ok`
	// sentinel on disk. The harness then reaches every decision it normally
	// reaches and cannot transmit a single non-GET.
	ReadOnly bool

	// StopPath overrides `paths.stop`, and it exists for exactly one case: a
	// test that needs `Lstat` on that path to fail with something OTHER than
	// ENOENT. There is no injection point for a failing stat -- the owner calls
	// `os.Lstat` directly, deliberately, because a filesystem seam here would be
	// a second thing that can lie about whether the operator asked us to stop --
	// so the test makes the REAL filesystem fail by putting the sentinel under a
	// parent that is a regular file (ENOTDIR).
	//
	// The rig copies the config BY VALUE, so mutating `h.cfg` after construction
	// does not reach it; the override has to happen here.
	StopPath string

	// Rung selects the capital ladder step. Empty is `pilot`, which is what
	// every test written before the canary policy existed assumes.
	//
	// Choosing one also sizes the fixture to it. `loadConfig` refuses a config
	// whose `S` exceeds the rung's maximum, so a canary harness running at the
	// pilot's S=12 is one no operator could have deployed, and a policy test
	// against a config that cannot exist proves nothing about the policy.
	Rung string

	// LatchDirMissing puts the halt latch inside a subdirectory that does not
	// exist, and it is how a seam test makes a latch WRITE fail without a fake.
	//
	// There is no injection point for one: `newRig` builds a
	// `*lifecycle.FileLatch` from `c.Paths.Latch` and hands it straight to
	// `NewGlobalController`, and `rig.latch` is the concrete type rather than
	// the `LatchStore` interface. That is not an oversight to route around --
	// the whole argument for the controller reading the latch in its
	// constructor is that no seam exists between deciding to halt and recording
	// it -- so the test makes the REAL disk fail instead.
	//
	// The two calls answer differently and both answers are wanted. `Load` gets
	// ENOENT on the file and reports NOT LATCHED with no error, which is the
	// one clear reading, so the harness boots clean. `Ensure`'s `O_EXCL` create
	// gets ENOENT on the DIRECTORY, which is not `os.IsExist`, so it returns
	// not-durable with an error -- exactly the transient-disk condition
	// `RetryLatch` exists for. `os.Mkdir` on `filepath.Dir(c.Paths.Latch)` is
	// the recovery, and the next write succeeds.
	LatchDirMissing bool

	// LatchCorrupt writes bytes at the latch path that `decodeLatch` refuses,
	// which is how a test makes the latch READ fail. `Load` reports
	// present-but-uninterpretable, so `NewGlobalController` bootstraps LATCHED
	// with `BlockAdding` and `RetryLatch` set -- the `rig.boot` case. It needs
	// `Resume`, because `newRig` refuses to start latched without it.
	LatchCorrupt bool
}

// seamHarness is one composed harness process plus everything outside it.
type seamHarness struct {
	t   *testing.T
	ctx context.Context

	cfg  config
	ex   *seamExchange
	ws   *seamDialer
	clk  *seamClock
	xch  exchange
	anom *anomalySink

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
func newSeamConfig(t *testing.T, stopPath string) config {
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
			LiveOK:     filepath.Join(d, "live_ok"),
			Stop:       seamStopPath(d, stopPath),
		},
		// ARMED BY DEFAULT, and only here (H-VER-1).
		//
		// Every seam test written before this bead is about what the harness
		// DOES when it trades -- the exit lands at the touch, the binding is
		// submitted inline, the canary latches on its first fill -- and a
		// read-only rig would make all of them assert nothing while still
		// passing, which is precisely the false green this repository exists
		// to refuse. So the seam arms both keys and the read-only tests turn
		// them off explicitly.
		//
		// The sentinel file itself is created in `newSeamHarness`, because a
		// config is a set of paths and this one has to exist on disk.
		Live: true,
	}
}

func newSeamHarness(t *testing.T, opt seamOptions) *seamHarness {
	t.Helper()

	c := newSeamConfig(t, opt.StopPath)
	if opt.ReadOnly {
		// The disarmed rehearsal. Both keys off: no `-live`, and the sentinel
		// is never created. `TestReadOnlyRunRecordsWouldWriteWithoutSendingNonGET`
		// is the test this exists for.
		c.Live = false
	} else if err := os.WriteFile(c.Paths.LiveOK, []byte("armed\n"), 0o600); err != nil {
		t.Fatalf("writing the live_ok sentinel: %v", err)
	}
	if opt.Rung != "" {
		r, ok := rungs[opt.Rung]
		if !ok {
			t.Fatalf("no such rung %q", opt.Rung)
		}
		c.Rung = r
		if c.Params.S > r.maxS {
			c.Params.S = r.maxS
		}
	}
	if opt.LatchDirMissing {
		// `provision` creates the parents of the DB and the anomaly log and
		// nothing else, so this directory stays absent until a test creates it.
		c.Paths.Latch = filepath.Join(
			filepath.Dir(c.Paths.Latch), "latch", "harness.halt")
	}
	// The store is PROVISIONED, never created by the run path. That is not
	// fixture hygiene, it is the rule `requireExistingDB` enforces: an absent
	// path and a mistyped one are the same thing to `hstore.Open`, and the
	// ledger it creates for either recognises no order id at all.
	if err := provision(c, io.Discard); err != nil {
		t.Fatalf("provisioning the seam store: %v", err)
	}

	if opt.LatchCorrupt {
		// A truncated write from a power cut, JSON that does not parse, the
		// wrong version: `decodeLatch` refuses all of them and every one reports
		// LATCHED with an error. This is the shortest of them.
		if err := os.WriteFile(c.Paths.Latch, []byte("{"), 0o600); err != nil {
			t.Fatalf("writing a corrupt halt latch: %v", err)
		}
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

	// The sink the composition root now owns. In production it is created
	// before `productionExchange` so F6's DNS fallback can reach it before the
	// rig exists; here there is no dialer, so it exists only to be passed.
	h.anom = newAnomalySink()
	r, err := newRig(h.ctx, c, opt.Resume, h.xch, h.anom)
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

// installOwnedOrder makes an exchange order id OURS in the durable ownership
// ledger, the way H-ORD-6's two commits would have.
//
// A test needs it before it can hand the fixture a fill on an order this
// process did not place. H-ORD-9 classifies by the LEDGER and by nothing else,
// so a fill on an order no row mentions is FOREIGN -- SEV1 and a durable global
// stop -- and a test meaning to exercise an owned fill would silently be
// exercising the foreign path instead.
//
// Both stages are made durable and the SECOND is awaited. A reservation without
// its binding is exactly the unresolved set §7.5 refuses to conclude over, so
// returning between the two would leave the next startup retrying forever.
func (h *seamHarness) installOwnedOrder(orderID string, seq uint64) {
	h.t.Helper()
	coid, err := rest.Coid("SEAMPRIOR", 0, quote.SideYes, seq)
	if err != nil {
		h.t.Fatalf("building a coid: %v", err)
	}
	o, err := rest.NewCreateOrder(seamTicker, quote.SideYes, 40,
		num.QtyFromFloat(1), num.QtyFromFloat(1), coid)
	if err != nil {
		h.t.Fatalf("building the reserved order: %v", err)
	}
	if _, err := h.rig.store.ReserveOrder(h.rig.run, o, quote.RoleAdding,
		h.clk.wallMs()); err != nil {
		h.t.Fatalf("reserving %s: %v", coid, err)
	}
	if _, err := h.rig.store.BindOrder(coid, orderID,
		h.clk.wallMs()); err != nil {
		h.t.Fatalf("binding %s -> %s: %v", coid, orderID, err)
	}
	h.await("the seeded ownership binding to commit", func() bool {
		got, ok := h.rig.store.Ownership().Bound(orderID)
		return ok && got == coid
	})
}

// latchTrigger is the durable §12 cause on disk, or "" if the harness has not
// stopped. It reads the LATCH rather than any in-memory field, because the
// latch is what a restart and a §10.4 operator both see.
//
// It RETRIES a latch that is present and not yet readable, and that is not
// leniency about a corrupt file. `FileLatch.Ensure` creates the path with
// `O_EXCL` and then writes it, and `Load` refuses the zero-length file that
// exists in between -- correctly, because "a zero-length file is what a crash
// between create and write leaves behind". A test polling from its own
// goroutine lands in that window often enough to matter, and a fixture that
// failed there would be reporting the writer's crash-safety as a defect. A file
// that never becomes readable still fails, after the same budget as every other
// wait in this file.
func (h *seamHarness) latchTrigger() string {
	h.t.Helper()
	latch, err := lifecycle.NewFileLatch(h.cfg.Paths.Latch)
	if err != nil {
		h.t.Fatalf("NewFileLatch(%s): %v", h.cfg.Paths.Latch, err)
	}
	deadline := time.Now().Add(seamBudget)
	for {
		rec, present, err := latch.Load()
		switch {
		case err == nil && !present:
			return ""
		case err == nil:
			return rec.Trigger
		case !time.Now().Before(deadline):
			h.t.Fatalf("the halt latch at %s never became readable within %v: "+
				"%v", h.cfg.Paths.Latch, seamBudget, err)
		}
		time.Sleep(time.Millisecond)
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

// anomalySevs is every DURABLE anomaly of one class with the severity it was
// recorded at.
//
// A class-only assertion cannot see a severity downgrade, and §11 assigns the
// severity deliberately: SEV1 wakes somebody and SEV2 does not. A control that
// failed and reported it at SEV3 is a control that failed silently as far as the
// operator's night is concerned.
func (h *seamHarness) anomalySevs(class string) []risk.Severity {
	h.t.Helper()
	rows, err := h.rig.store.Reader().PendingAnomalies()
	if err != nil {
		h.t.Fatalf("PendingAnomalies: %v", err)
	}
	var out []risk.Severity
	for _, r := range rows {
		if r.Class == class {
			out = append(out, r.Sev)
		}
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

// seamPlaces is every pending intent whose CURRENT leg is a placement, on either
// side. It is the queue-level reading of "an adding write is on its way out":
// `pump` dispatches what `Dequeue` hands it, so an empty answer here is a tick
// that cannot have placed anything.
func seamPlaces(q *quote.Queue) []quote.Intent {
	var out []quote.Intent
	for _, in := range q.Pending() {
		if in.Op() == quote.OpPlace {
			out = append(out, in)
		}
	}
	return out
}

// seamLatchTrigger reads the trigger off the durable halt latch, or "" if there
// is no latch at that path.
func seamLatchTrigger(t *testing.T, path string) string {
	t.Helper()
	latch, err := lifecycle.NewFileLatch(path)
	if err != nil {
		t.Fatalf("NewFileLatch(%s): %v", path, err)
	}
	rec, present, err := latch.Load()
	if err != nil {
		t.Fatalf("loading the halt latch at %s: %v", path, err)
	}
	if !present {
		return ""
	}
	return rec.Trigger
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
	// A FRESH sink per call, because every caller of this helper is testing a
	// SECOND process against a rig that is already running, and the refusal it
	// expects happens before anything drains either queue.
	anom := newAnomalySink()
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
	return newRig(ctx, c, resume, ex, anom)
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
		// A14 and H-HALT-4 admit EITHER halted state -- "a latch on disk
		// implies WINDING_DOWN or DRAINED" -- and this account is flat with
		// nothing resting, so since lip-xdq drove §5.1's own drain edge from
		// the tick it settles in DRAINED. RUNNING is the failure this asserts
		// against: it is the halt having self-cleared.
		if s.Global.AddsRisk() {
			t.Fatalf("the global state is %s, want a halted one. A14 and "+
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
	// Halted, not one named state. This fixture resumes onto a set latch with a
	// flat account and nothing resting, so once lip-xdq made §5.1's own
	// WINDING_DOWN -> DRAINED edge reachable from the tick it settles in
	// DRAINED rather than WINDING_DOWN. Both are halted, A14 admits both, and
	// what this test needs is only that the harness quotes nothing -- which is
	// exactly `AddsRisk`.
	if s := h.snapshot(); s.Global.AddsRisk() {
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
		// The rule is that the halt does not SELF-CLEAR, and the only clearing
		// is back to RUNNING. WINDING_DOWN -> DRAINED is §5.1 continuing
		// forwards on a flat account, not a path back, and H-HALT-4 names both
		// as halted.
		if s.Global.AddsRisk() {
			t.Fatalf("the global state left the halt (now %s); H-HALT-4 "+
				"offers no path back to RUNNING without an operator", s.Global)
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

// TestASustainedOutageReachesTheGateAndTheDisconnectTokenReachesThePoller is
// `lip-0qj`: the two effects `applyEvent` must RELAY, both of which fail
// SILENTLY when they are forgotten.
//
// Neither is re-derived anywhere downstream, which is what makes them worth a
// seam test rather than a unit test of either end:
//
//   - F4's threshold is detected in exactly one place, `wsx.Supervisor`, which
//     emits `EventDisconnectReduce`. `Gate.Tick` deliberately does NOT re-detect
//     it -- two clocks on one rule is how a market reduces twice or neither --
//     so if `applyEvent` drops the event, the socket can be down for an hour and
//     no market ever reduces. Nothing raises, nothing logs, and the harness goes
//     on quoting into a book it cannot see.
//   - `ApplyDisconnect` returns a token for the DISCONNECTED generation. If it
//     is not offered, every portfolio read taken during the outage is discarded
//     as stale, so position monitoring is blind for exactly the window in which
//     the socket is not watching either -- while REST keeps issuing the requests.
func TestASustainedOutageReachesTheGateAndTheDisconnectTokenReachesThePoller(
	t *testing.T) {
	// q = +1, far under inv_soft, a book on both sides and a schedule read a day
	// from the close: every ground for stopping EXCEPT the one under test is
	// removed, so a market that stops has stopped because of the outage.
	const heldYes = 1

	setup := func(t *testing.T) (*seamHarness, *owner) {
		t.Helper()
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
		o.closeAt = time.Now().Add(24 * time.Hour)
		o.hasClose = true
		o.scheduleEver = true
		return h, o
	}

	t.Run("the sustained outage sends the market to REDUCING", func(t *testing.T) {
		h, o := setup(t)

		// The control. If this is not QUOTING then nothing below is
		// attributable to the relay.
		o.evaluate(0)
		if o.market != quote.Quoting {
			t.Fatalf("the market is %s before any outage, want QUOTING; the "+
				"rest of this test cannot attribute a stop to the disconnect "+
				"unless the market was running first", o.market)
		}
		if h.rig.gate.Reducing(seamTicker) {
			t.Fatal("the gate reports the market REDUCING before any outage " +
				"was relayed to it")
		}
		h.takeRaised()

		// The gate is left CONNECTED deliberately. The supervisor only emits
		// this event while the socket is down, so the realistic sequence would
		// be a disconnect first -- but a disconnect ALSO makes the book
		// non-actionable, and then a market that stopped adding would be
		// evidence about H-FAIL-5's quarantine rather than about F4's
		// threshold. Holding the connection is what isolates the relay.
		tokens := make(chan wsx.ReconcileToken, 1)
		o.applyEvent(wsx.Event{
			Kind: wsx.EventDisconnectReduce,
			At:   h.clk.Now(),
			Down: h.cfg.Params.DisconnectReduce,
		}, tokens)

		// The gate's `reducing` is STICKY and this is the only thing in the
		// process that sets it here, so it is the relay's own footprint: it is
		// what keeps the market reducing after the socket comes back, because
		// "a reconnect is evidence about the socket and not about the risk
		// taken while it was down".
		if !h.rig.gate.Reducing(seamTicker) {
			t.Fatal("the gate does not report the market REDUCING after " +
				"EventDisconnectReduce.\n\n" +
				"F4 is detected in exactly one place -- the supervisor -- and " +
				"Gate.Tick deliberately does not re-detect it. If applyEvent " +
				"does not call Gate.NoteDisconnectSustained, the socket can " +
				"be down for an hour and no market ever reduces.")
		}
		if !o.reduceNoted {
			t.Fatal("the owner did not note the reduce for this evaluation; " +
				"the gate's sticky flag would still stop the market on a " +
				"LATER tick, but this tick would quote on")
		}

		// A SEV1 is the operator's only notice that the harness has stopped
		// adding across the board.
		var sustained bool
		for _, a := range h.takeRaised() {
			if a.Class == "WS_DISCONNECT_SUSTAINED" {
				sustained = true
				if a.Sev != risk.SEV1 {
					t.Fatalf("WS_DISCONNECT_SUSTAINED is %v, want SEV1",
						a.Sev)
				}
			}
		}
		if !sustained {
			t.Fatal("no WS_DISCONNECT_SUSTAINED anomaly was raised; the " +
				"gate returns it from NoteDisconnectSustained and applyEvent " +
				"is what puts it in front of the operator")
		}

		o.evaluate(0)
		if o.market != quote.Reducing {
			t.Fatalf("the market is %s after a sustained outage, want "+
				"REDUCING", o.market)
		}

		// §5.2's REDUCING row, A8: the adding side comes off and is confirmed
		// absent.
		adds := seamCancelsOn(h.rig.queue, quote.SideYes)
		if len(adds) != 1 {
			t.Fatalf("%d cancel intents were queued for the ADDING side, "+
				"want 1", len(adds))
		}
		if adds[0].Role != quote.RoleAdding {
			t.Fatalf("the adding-side cancel is classified %s", adds[0].Role)
		}

		// I1: every stop path stops ADDING risk and none of them stops
		// reducing it. An outage is not a reason to strand the position.
		if exits := seamCancelsOn(h.rig.queue, quote.SideNo); len(exits) != 0 {
			t.Fatalf("%d cancel intent(s) were queued for the REDUCING side; "+
				"cancelling the exit because the socket went down strands the "+
				"position it was protecting", len(exits))
		}
		if got := o.atRisk(quote.SideNo); got != num.QtyFromFloat(heldYes) {
			t.Fatalf("the exit's aggregate is %s, want %s", got.Wire(),
				num.QtyFromFloat(heldYes).Wire())
		}
	})

	t.Run("the disconnect token is offered to the poller", func(t *testing.T) {
		h, o := setup(t)

		tokens := make(chan wsx.ReconcileToken, 1)
		o.applyEvent(wsx.Event{
			Kind:  wsx.EventDisconnected,
			At:    h.clk.Now(),
			Clean: false,
		}, tokens)

		select {
		case tok := <-tokens:
			if !tok.Valid() {
				t.Fatal("an INVALID token was offered on disconnect")
			}
		default:
			t.Fatal("no reconciliation token was offered on disconnect.\n\n" +
				"ApplyDisconnect returns a token for the disconnected " +
				"generation and the poller's own contract says why it wants " +
				"it: on disconnect it is the first reading of an account " +
				"nobody is watching over the socket any more. Without the " +
				"relay every read taken during the outage is discarded as " +
				"stale and position monitoring is blind for the whole of it, " +
				"while REST keeps issuing the requests.")
		}

		// A second disconnect on an already-disconnected gate returns zero
		// effects, and `offerToken` must drop the invalid token rather than
		// hand the poller a generation that does not exist.
		o.applyEvent(wsx.Event{
			Kind:  wsx.EventDisconnected,
			At:    h.clk.Now(),
			Clean: false,
		}, tokens)
		select {
		case tok := <-tokens:
			t.Fatalf("a token (valid=%v) was offered for a disconnect of an "+
				"already-disconnected gate; ApplyDisconnect returns zero "+
				"effects there and offerToken's validity guard is what stops "+
				"the zero value being read as a generation", tok.Valid())
		default:
		}
	})
}

// TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs is
// `lip-vxo`: the two answers `lifecycle` computes on every uncertain stop path
// and that `cmd/harness` used to read NEITHER of.
//
// `StopCommit.BlockAdding` is "I1's response to every uncertainty in this
// file", and `StopCommit.RetryLatch` "asks the caller to commit again". Before
// this test `requestStop` read neither: on `!Durable` it returned, and whether
// the stop was ever tried again depended on whether the ORIGINAL condition
// happened to recur. All three of its call sites are event-driven -- a
// portfolio read, a gate tick and an ack that carried a fill -- and a taker
// fill does not recur. So one transient EIO lost a global stop for the life of
// the process, silently, with the harness still adding into the condition that
// had already decided to stop it.
//
// This is a seam test and not a `lifecycle` unit test because `lifecycle` was
// never wrong. Every mutation of the PRODUCER stays caught while the CONSUMER
// throws the result away, which is the same shape as `lip-0qj`, and the only
// place the two ends meet is here.
func TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs(
	t *testing.T) {
	// q = +1, far under inv_soft, a book on both sides and a schedule read a day
	// from the close: every ground for stopping EXCEPT the one under test is
	// removed, so a market that stops has stopped because of the failed write.
	const heldYes = 1

	setup := func(t *testing.T) (*seamHarness, *owner) {
		t.Helper()
		h := newSeamHarness(t, seamOptions{LatchDirMissing: true})
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
		o.closeAt = time.Now().Add(24 * time.Hour)
		o.hasClose = true
		o.scheduleEver = true

		// The control. If this is not QUOTING then nothing below is
		// attributable to the stop.
		o.evaluate(0)
		if o.market != quote.Quoting {
			t.Fatalf("the market is %s before any stop was requested, want "+
				"QUOTING; the rest of this test cannot attribute a stop to a "+
				"failed latch write unless the market was running first",
				o.market)
		}
		if o.global != quote.Running {
			t.Fatalf("the global state is %s, want RUNNING", o.global)
		}
		h.takeRaised()
		return h, o
	}

	t.Run("the gap blocks adding and leaves everything else running",
		func(t *testing.T) {
			h, o := setup(t)

			o.requestStop("taker_fill", seamTicker)

			// The write really did fail. Without this the test would pass on a
			// harness that never had anything to recover from.
			if got := seamLatchTrigger(t, h.cfg.Paths.Latch); got != "" {
				t.Fatalf("the halt latch reads trigger %q, so the write "+
					"SUCCEEDED and this test is asserting nothing", got)
			}
			if !o.stopHeld {
				t.Fatal("the owner is not holding the cause after a failed " +
					"latch write.\n\n" +
					"StopCommit.BlockAdding and StopCommit.RetryLatch are the " +
					"two answers lifecycle computes for exactly this case, " +
					"and neither is a diagnostic. Dropping them here is a " +
					"global stop that survives only if the condition that " +
					"produced it happens to fire again -- and requestStop's " +
					"three callers are all event-driven. A taker fill does " +
					"not recur.")
			}
			if o.stopCause.Trigger != "taker_fill" {
				t.Fatalf("the held cause is %q, want taker_fill",
					o.stopCause.Trigger)
			}

			// H-HALT-4's order, from the other side: the latch reaches disk
			// BEFORE the in-memory state changes, so a latch that never reached
			// disk changes no state at all. Publishing here is the HR-009
			// sequence with the harness having been TOLD the write failed --
			// WINDING_DOWN in memory, nothing on disk, a panic and `launchd
			// KeepAlive` erasing it completely.
			if o.global != quote.Running {
				t.Fatalf("the global state is %s after a stop that is NOT on "+
					"disk, want RUNNING. A halt published without its latch "+
					"is one a restart cannot see", o.global)
			}

			var failed bool
			for _, a := range h.takeRaised() {
				if a.Class == "LATCH_WRITE_FAILED" {
					failed = true
					if a.Sev != risk.SEV1 {
						t.Fatalf("LATCH_WRITE_FAILED is %v, want SEV1", a.Sev)
					}
				}
			}
			if !failed {
				t.Fatal("no LATCH_WRITE_FAILED anomaly reached the operator; " +
					"it is their only notice that the harness has decided to " +
					"stop and cannot record it")
			}

			// I1, which is the whole of BlockAdding's meaning: this stops
			// ADDING and stops nothing else.
			o.evaluate(0)

			if o.market != quote.Reducing {
				t.Fatalf("the market is %s while a §12 cause is held "+
					"undurable, want REDUCING. BlockAdding is honoured in "+
					"exactly one place -- §5.2's own stop term -- and a "+
					"market still QUOTING is one adding into the condition "+
					"that already decided to stop it", o.market)
			}
			if places := seamPlaces(h.rig.queue); len(places) != 0 {
				t.Fatalf("%d placement intent(s) are queued during the gap; "+
					"no adding write may leave this process while the cause "+
					"that stopped it is not on disk", len(places))
			}
			adds := seamCancelsOn(h.rig.queue, quote.SideYes)
			if len(adds) != 1 {
				t.Fatalf("%d cancel intents were queued for the ADDING side, "+
					"want 1; a stopped market's adding side is cancelled and "+
					"confirmed absent, not merely left unrefreshed", len(adds))
			}
			if adds[0].Role != quote.RoleAdding {
				t.Fatalf("the adding-side cancel is classified %s",
					adds[0].Role)
			}
			if exits := seamCancelsOn(h.rig.queue, quote.SideNo); len(exits) != 0 {
				t.Fatalf("%d cancel intent(s) were queued for the REDUCING "+
					"side.\n\nI1: BlockAdding is deliberately the ONLY "+
					"prohibition -- cancels, reducing quotes, position "+
					"polling, reconciliation and monitoring all continue "+
					"while it is set. A harness that stopped managing its "+
					"inventory because it could not write a file has "+
					"converted a storage failure into an unobserved position",
					len(exits))
			}
			if got := o.atRisk(quote.SideNo); got != num.QtyFromFloat(heldYes) {
				t.Fatalf("the exit's aggregate is %s, want %s: nothing here "+
					"may retire it from the risk model", got.Wire(),
					num.QtyFromFloat(heldYes).Wire())
			}
		})

	t.Run("the tick retries it and publishes only once it is durable",
		func(t *testing.T) {
			h, o := setup(t)

			o.requestStop("taker_fill", seamTicker)
			o.evaluate(0)
			if !o.stopHeld || o.global != quote.Running {
				t.Fatalf("held=%v global=%s before the disk recovered",
					o.stopHeld, o.global)
			}
			h.takeRaised()

			// Several ticks with the disk still failing. The cause is held
			// across every one of them, and adding stays off across every one
			// of them -- the retry is the loop's obligation and is not paced
			// by the trigger, which has not fired again and will not.
			for i := 0; i < 3; i++ {
				o.evaluate(0)
				if !o.stopHeld {
					t.Fatalf("the cause was dropped on retry %d", i+1)
				}
				if o.market != quote.Reducing {
					t.Fatalf("the market is %s on retry %d, want REDUCING",
						o.market, i+1)
				}
			}
			if o.stopRetries < 3 {
				t.Fatalf("the owner recorded %d retries across 4 ticks; "+
					"RetryLatch asks for the write to be made again, and a "+
					"count that does not advance is a hold nothing is "+
					"driving", o.stopRetries)
			}

			// The disk recovers. Nothing else changes: no new trigger fires,
			// no event arrives, and the only thing that happens is a tick.
			if err := os.Mkdir(filepath.Dir(h.cfg.Paths.Latch), 0o755); err != nil {
				t.Fatalf("recovering the latch directory: %v", err)
			}
			o.evaluate(0)

			if got := seamLatchTrigger(t, h.cfg.Paths.Latch); got != "taker_fill" {
				t.Fatalf("the halt latch reads trigger %q after the disk "+
					"recovered, want taker_fill. Nothing re-triggered the "+
					"stop, so the write can only have come from the retry",
					got)
			}
			if o.stopHeld {
				t.Fatal("the owner still holds a cause that is now durable")
			}
			if o.global != quote.WindingDown {
				t.Fatalf("the global state is %s once the stop is durable, "+
					"want WINDING_DOWN", o.global)
			}
			var recovered bool
			for _, a := range h.takeRaised() {
				if a.Class == "LATCH_WRITE_RECOVERED" {
					recovered = true
				}
			}
			if !recovered {
				t.Fatal("no LATCH_WRITE_RECOVERED anomaly was raised; an " +
					"operator told the harness could not record its halt " +
					"needs telling that it since did, because only the " +
					"second fact licenses the state now published")
			}

			// I1 held on both sides of the recovery.
			if o.market != quote.Reducing {
				t.Fatalf("the market is %s after the stop went durable, want "+
					"REDUCING", o.market)
			}
			if got := o.atRisk(quote.SideNo); got != num.QtyFromFloat(heldYes) {
				t.Fatalf("the exit's aggregate is %s, want %s", got.Wire(),
					num.QtyFromFloat(heldYes).Wire())
			}
		})

	t.Run("a second trigger does not displace the first cause",
		func(t *testing.T) {
			h, o := setup(t)

			o.requestStop("taker_fill", seamTicker)
			o.requestStop("insufficient_balance", seamTicker)
			if o.stopCause.Trigger != "taker_fill" {
				t.Fatalf("the held cause is %q after a second trigger, want "+
					"taker_fill.\n\nFileLatch.Ensure is first-writer-wins for "+
					"the reason it states: the first durable cause is the one "+
					"the operator investigates, and a later, more mundane "+
					"trigger must not overwrite the reason the harness "+
					"stopped. A hold that takes the last cause instead makes "+
					"the retry write the wrong one", o.stopCause.Trigger)
			}

			if err := os.Mkdir(filepath.Dir(h.cfg.Paths.Latch), 0o755); err != nil {
				t.Fatalf("recovering the latch directory: %v", err)
			}
			o.evaluate(0)
			if got := seamLatchTrigger(t, h.cfg.Paths.Latch); got != "taker_fill" {
				t.Fatalf("the halt latch records trigger %q, want taker_fill",
					got)
			}
		})

	t.Run("a signal whose latch write failed is retried too",
		func(t *testing.T) {
			h, o := setup(t)

			o.applySignal(syscall.SIGTERM)

			if !o.stopHeld {
				t.Fatal("the owner is not holding a cause after a SIGTERM " +
					"whose latch write failed.\n\n" +
					"This is the one trigger with no condition left to recur: " +
					"a signal is delivered ONCE. SignalController.Handle's " +
					"own contract says that on a failed write \"the harness " +
					"still stops adding and still drains\", and dropping the " +
					"decision here is what made that sentence false.")
			}
			if o.stopCause.Trigger != "sigterm" {
				t.Fatalf("the held cause is %q, want sigterm; it comes from "+
					"SignalEffects.Cause so that the caller does not derive a "+
					"second os.Signal-to-trigger mapping of its own",
					o.stopCause.Trigger)
			}
			if o.global != quote.Running {
				t.Fatalf("the global state is %s after a SIGTERM that is not "+
					"on disk, want RUNNING", o.global)
			}

			o.evaluate(0)
			if o.market != quote.Reducing {
				t.Fatalf("the market is %s after a SIGTERM whose latch write "+
					"failed, want REDUCING (H-HALT-3 stops adding and keeps "+
					"the exit alive)", o.market)
			}

			// The control for the assertion below. The drain has begun, but
			// UNPLANNED: `Handle` issues a permit only on `Committed`, and this
			// stop is not on disk. An unplanned drain "can never authorise an
			// exit", which is the correct answer while the stop is unrecorded.
			drained := lifecycle.DrainObservation{TruthKnown: true}
			if h.rig.drain.Observe(drained, 0).ExitAuthorised {
				t.Fatal("the drain authorised an exit while the SIGTERM's own " +
					"stop was not on disk; exiting there hands launchd a clean " +
					"directory to resume quoting from")
			}

			if err := os.Mkdir(filepath.Dir(h.cfg.Paths.Latch), 0o755); err != nil {
				t.Fatalf("recovering the latch directory: %v", err)
			}
			o.evaluate(0)
			if got := seamLatchTrigger(t, h.cfg.Paths.Latch); got != "sigterm" {
				t.Fatalf("the halt latch reads trigger %q after the disk "+
					"recovered, want sigterm; the signal will not be sent "+
					"again and the retry is the only thing that can record it",
					got)
			}
			if o.global != quote.WindingDown {
				t.Fatalf("the global state is %s once the SIGTERM is durable, "+
					"want WINDING_DOWN", o.global)
			}

			// The half a generic retry cannot deliver. H-HALT-3's two
			// authorities are granted by different things: the signal stops
			// adding, but only a DrainPermit may end the process, and `Handle`
			// issued none because its write failed. Committing the same cause
			// later makes the stop durable and leaves the drain UNPLANNED
			// forever unless the permit is re-issued -- so the operator's
			// SIGTERM would stop the harness, wind it down, reach flat, and
			// then idle rather than finish.
			if !h.rig.drain.Observe(drained, 0).ExitAuthorised {
				t.Fatal("the drain still refuses to authorise an exit after " +
					"the SIGTERM's stop became durable.\n\n" +
					"BeginUnplanned can never authorise one and DrainTracker " +
					"upgrades an unplanned drain only when handed a valid " +
					"permit. If nothing mints one on recovery, H-HALT-3's " +
					"\"exits only when every market is flat or closed\" is " +
					"unreachable for exactly the signals whose latch write " +
					"failed once.")
			}
		})
}

// TestAnUnreadableLatchAtBootIsHonouredOnTheFirstTick is `rig.boot`'s half of
// `lip-vxo`.
//
// The field's own comment says it: "`BlockAdding` and `RetryLatch` are answers
// the run loop must honour on its first tick, not diagnostics." Only `.Latched`
// and `.Anomalies` were ever read, so the two answers reached the run loop and
// stopped there.
//
// The condition is a latch that is PRESENT and cannot be interpreted -- a
// truncated write from a power cut, a permission error, JSON that does not
// parse. `NewGlobalController` bootstraps LATCHED and asks for the write to be
// retried, and there is no cause to retry it with because nothing in this
// process decided to stop. The owner synthesises one, and `Ensure`'s
// first-writer-wins is what makes that safe: an existing record is not
// overwritten, and the retry instead completes the parent-directory sync that
// the failed read could never confirm had happened.
func TestAnUnreadableLatchAtBootIsHonouredOnTheFirstTick(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Resume: true, LatchCorrupt: true})

	// The producer really did set them. Without this the test would pass on a
	// build where the bootstrap had quietly stopped answering.
	if !h.rig.boot.Latched {
		t.Fatal("the bootstrap did not report LATCHED for a latch it could " +
			"not interpret; exactly one condition is clear and it is the file " +
			"not existing")
	}
	if !h.rig.boot.BlockAdding || !h.rig.boot.RetryLatch {
		t.Fatalf("the bootstrap reported BlockAdding=%v RetryLatch=%v for an "+
			"uninterpretable latch; this test asserts what the run loop does "+
			"with them and there is nothing to do",
			h.rig.boot.BlockAdding, h.rig.boot.RetryLatch)
	}

	o := h.ownerFor()

	// Honoured at CONSTRUCTION, which is what "on its first tick" requires: the
	// owner is built before `startup` runs and long before the first
	// `evaluate`, so a hold taken any later is a hold taken after the window it
	// exists for.
	if !o.stopHeld {
		t.Fatal("the owner does not hold a cause with rig.boot.BlockAdding " +
			"and rig.boot.RetryLatch both set.\n\n" +
			"The field's own comment calls these answers the run loop must " +
			"honour on its first tick and not diagnostics, and reading only " +
			".Latched and .Anomalies honours neither.")
	}
	if o.stopCause.Trigger != "latch_unreadable" {
		t.Fatalf("the held cause is %q, want latch_unreadable",
			o.stopCause.Trigger)
	}

	// The first tick makes it durable. `Ensure` sees the existing file, so the
	// corrupt record is left exactly as it was -- the operator of §10.4 reads
	// what the previous incarnation left, not what this one guessed -- and what
	// the retry adds is the directory sync.
	o.evaluate(0)

	if o.stopHeld {
		t.Fatal("the owner still holds the bootstrap's cause after a tick " +
			"that could write it")
	}
	// Halted on the first tick. It settles in DRAINED rather than WINDING_DOWN
	// because this account is flat, has nothing resting, and its startup walks
	// left truth fresh -- which is §5.1's drain rule exactly, now that lip-xdq
	// drives that edge from the tick. A14 admits both; RUNNING is the failure.
	if o.global.AddsRisk() {
		t.Fatalf("the global state is %s on the first tick after booting on "+
			"an unreadable latch, want a halted one", o.global)
	}
}

// seamGlobalEvents is every DURABLE A9 row for a §5.1 transition, in order.
//
// It reads what SURVIVED the writer rather than what was submitted, because the
// operator of §10.4 reconstructing why the harness stopped reads the table and
// not the call.
func (h *seamHarness) seamGlobalEvents() []string {
	h.t.Helper()
	rows, err := h.rig.store.Reader().StateEvents()
	if err != nil {
		h.t.Fatalf("StateEvents: %v", err)
	}
	var out []string
	for _, r := range rows {
		if r.Scope == "global" {
			out = append(out, fmt.Sprintf("%s->%s/%s", r.From, r.To, r.Trigger))
		}
	}
	return out
}

// awaitGlobalEvent waits for a durable A9 global row matching `want`.
func (h *seamHarness) awaitGlobalEvent(want string) {
	h.t.Helper()
	h.await("the durable A9 row "+want, func() bool {
		return seamContains(h.seamGlobalEvents(), want)
	})
}

// TestEveryGlobalTransitionRecordsItsOwnCause is lip-xdq: `advance` had exactly
// ONE caller -- the stop funnel -- and passed no state, so §5.1 was half wired.
//
// Two consequences, and the operator of §10.4 reading `state_event` at 3am is
// the one who paid for both:
//
//   - `GTStop` was unreachable. `CommitStop` sets the controller's cached latch
//     and `Advance` injects it, so A14 -- "checked before every other rule" --
//     fired ahead of the `RUNNING && Stop` rule on every commit-then-advance.
//     A taker fill, `insufficient_balance` and a SIGTERM all wrote `halt_latch`,
//     which is the trigger that means "this process inherited a halt from a
//     previous incarnation". The two situations call for opposite responses and
//     the column could not tell them apart.
//   - The ordinary edges were never driven at all. §5.1 defines
//     WINDING_DOWN -> DRAINED on all-flat and DRAINED -> WINDING_DOWN when
//     inventory reappears, and nothing ever asked for either, so a harness that
//     wound down and reduced to flat reported WINDING_DOWN forever and
//     `GTInventory` was dead code in production.
func TestEveryGlobalTransitionRecordsItsOwnCause(t *testing.T) {
	t.Run("a live stop is global_stop, not halt_latch", func(t *testing.T) {
		// Inventory is held deliberately: it keeps the harness in WINDING_DOWN
		// so this sub-test observes the stop edge and nothing after it.
		h := newSeamHarness(t, seamOptions{})
		o := h.ownerFor()
		h.connectGate()
		h.installBook([][]string{{"0.4000", "20.00"}},
			[][]string{{"0.5500", "20.00"}})
		h.installPosition(num.QtyFromFloat(1))
		o.closeAt = time.Now().Add(24 * time.Hour)
		o.hasClose = true
		o.scheduleEver = true

		o.evaluate(0)
		if o.global != quote.Running {
			t.Fatalf("the fixture is %s before any stop, want RUNNING",
				o.global)
		}

		o.requestStop("taker_fill", seamTicker)

		if o.global != quote.WindingDown {
			t.Fatalf("the global state is %s after a durable taker fill, "+
				"want WINDING_DOWN", o.global)
		}
		h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")

		for _, ev := range h.seamGlobalEvents() {
			if ev == "RUNNING->WINDING_DOWN/halt_latch" {
				t.Fatal("the live taker fill was recorded as `halt_latch`.\n\n" +
					"That is the trigger for a halt this process INHERITED. " +
					"An operator reading it cannot tell a stop this " +
					"incarnation took from a restart into a previous one's " +
					"latch, and the two call for opposite responses.")
			}
		}
	})

	t.Run("a restart onto a latch is halt_latch, and then drains",
		func(t *testing.T) {
			// Flat, nothing resting: the account has nothing to wind down, so
			// §5.1's own drain rule applies the moment it is asked.
			h := newSeamHarness(t, seamOptions{Latched: true, Resume: true})
			h.start()
			h.awaitActionable()

			h.awaitGlobalEvent("STARTING->WINDING_DOWN/halt_latch")
			h.awaitGlobalEvent("WINDING_DOWN->DRAINED/drained")

			if s := h.snapshot(); s.Global != quote.Drained {
				t.Fatalf("the published global state is %s on a flat resumed "+
					"harness, want DRAINED.\n\n"+
					"§5.1 draws WINDING_DOWN -> DRAINED on all-flat and the "+
					"machine implements it, but before lip-xdq nothing asked: "+
					"`advance` was reachable only from the stop funnel, so a "+
					"harness with nothing left to unwind reported WINDING_DOWN "+
					"for the rest of its life and every §14 report repeated it",
					s.Global)
			}
		})

	t.Run("inventory reappearing under DRAINED returns to WINDING_DOWN",
		func(t *testing.T) {
			// Flat and unrested, so the stop drains on the very next tick.
			h := newSeamHarness(t, seamOptions{})
			o := h.ownerFor()
			h.connectGate()
			h.installBook([][]string{{"0.4000", "20.00"}},
				[][]string{{"0.5500", "20.00"}})
			o.closeAt = time.Now().Add(24 * time.Hour)
			o.hasClose = true
			o.scheduleEver = true

			o.requestStop("taker_fill", seamTicker)
			o.evaluate(0)

			if o.global != quote.Drained {
				t.Fatalf("the global state is %s after a stop on a flat, "+
					"unrested account, want DRAINED", o.global)
			}
			h.awaitGlobalEvent("WINDING_DOWN->DRAINED/drained")

			// A fill reported late, a position poll disagreeing, an adoption at
			// restart. DRAINED rests no reducer, so a position discovered there
			// is unmanaged until the state that keeps an exit alive is
			// re-entered.
			h.installPosition(num.QtyFromFloat(1))
			o.evaluate(0)

			if o.global != quote.WindingDown {
				t.Fatalf("the global state is %s after inventory reappeared "+
					"under DRAINED, want WINDING_DOWN.\n\n"+
					"DRAINED rests no reducer. A position that turns up there "+
					"and does not move the state is one nothing is quoting an "+
					"exit for, and the harness reports itself drained while "+
					"holding it", o.global)
			}
			h.awaitGlobalEvent("DRAINED->WINDING_DOWN/inventory_reappeared")

			if o.market != quote.Reducing {
				t.Fatalf("the market is %s with inventory under a halted "+
					"global state, want REDUCING: the exit is what "+
					"WINDING_DOWN exists to keep alive", o.market)
			}
		})
}

// ---------------------------------------------------------------------------
// 13. lip-2t6 -- the canary first-fill latch
// ---------------------------------------------------------------------------

// TestTheCanaryLatchesOnAnyPositionItDidNotStartWith is the ENTRY FALLBACK, and
// the vector table is the arithmetic that makes this rule necessary at all.
//
// pilot-plan §7.9 bounds the canary by "the FIRST directional fill latches
// WINDING_DOWN", and no assignment of §16's numbers delivers that. Fills are
// fractional to the 0.01-contract quantum, F17 compares with a strict `>`, and
// `inv_kill` must sit strictly above `inv_hard`, which must sit strictly above
// `inv_soft`, which must be positive. So a fill of 0.01 breaches nothing, a
// fill between `inv_hard` and `inv_kill` breaches only the market-scoped brake
// -- which self-clears at flat and lets the harness resume adding -- and a fill
// of exactly `inv_kill` does not breach F17 either, because the comparison is
// strict.
//
// The position endpoint carries no trade identity, so this source is not proof
// of ownership and is not treated as any: `q_local` exactly flat against a
// nonzero exchange figure is the transition from no position to a position, and
// that is the whole of what it claims.
func TestTheCanaryLatchesOnAnyPositionItDidNotStartWith(t *testing.T) {
	cases := []struct {
		name  string
		posFP string
		cause string
		why   string
	}{
		{
			name: "one quantum", posFP: "0.01", cause: "canary_position_nonzero",
			why: "0.01 is the smallest quantity that can exist and it breaches " +
				"no §16 threshold at all; it is the case the whole rule is for",
		},
		{
			name: "negative inventory", posFP: "-0.01",
			cause: "canary_position_nonzero",
			why: "q is signed and YES-positive, so a NO fill is a directional " +
				"entry that arrives with the other sign. A rule written on the " +
				"signed value rather than the magnitude ignores half the book",
		},
		{
			name: "exactly inv_kill", posFP: "18.00", cause: "portfolio_read",
			why: "F17 compares with a strict `>`, so exactly inv_kill would not " +
				"breach it. This position DOES exceed pos_drift_hard, which is a " +
				"stronger cause and takes the latch by first-writer-wins -- the " +
				"canary offered its own and correctly lost",
		},
		{
			name: "one quantum above inv_kill", posFP: "18.01",
			cause: "portfolio_read",
			why:   "as above, and the point is that the harness stops either way",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{Rung: "canary"})
			h.start()
			h.awaitActionable()

			// The control. Everything below is attributable to the position
			// only if the harness was running and unlatched before it.
			h.awaitTicks(2)
			if s := h.snapshot(); s.Global != quote.Running {
				t.Fatalf("the global state is %s on a flat canary before any "+
					"position appeared, want RUNNING", s.Global)
			}
			if got := h.latchTrigger(); got != "" {
				t.Fatalf("the halt latch already reads %q before the test did "+
					"anything", got)
			}

			h.ex.setPosition(seamTicker, tc.posFP)
			h.clk.Advance(h.cfg.Params.PositionPoll)

			h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
			h.await("the durable §12 cause to reach the latch", func() bool {
				return h.latchTrigger() != ""
			})
			if got := h.latchTrigger(); got != tc.cause {
				t.Fatalf("a canary that went from flat to %s contracts latched "+
					"with cause %q, want %q.\n\n%s",
					tc.posFP, got, tc.cause, tc.why)
			}
		})
	}
}

// TestThePilotRungIgnoresThePositionTheCanaryStopsFor is the other half of the
// same measurement, and it is what makes the test above mean something.
//
// The rule is a property of the RUNG. If the pilot stopped here too, the canary
// assertions would be satisfied by any harness that stops on any position, and
// nothing would be measuring the policy. 0.01 contracts is under `inv_soft`,
// under `inv_hard`, under `inv_kill` and inside `pos_drift_tol`, so a pilot has
// no ground to stop and must not.
func TestThePilotRungIgnoresThePositionTheCanaryStopsFor(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Rung: "pilot"})
	h.start()
	h.awaitActionable()

	h.ex.setPosition(seamTicker, "0.01")
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("a PILOT harness latched with cause %q on 0.01 contracts.\n\n"+
			"The first-fill bound belongs to the canary rung and to no other. "+
			"A pilot that stops at the quantum has had its exposure ladder "+
			"collapsed into one step, and the rung table exists to stop "+
			"exactly that", got)
	}
	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the pilot's global state is %s after a 0.01 position, want "+
			"RUNNING", s.Global)
	}
}

// TestAnIncompletePositionWalkNeverLatchesTheCanary is H-PAGE-1 applied to the
// entry fallback.
//
// "Stale, never empty" cuts both ways. An incomplete walk may not be read as
// flat, and it may not be read as a position either: nothing about it is a
// current-generation statement of what the account holds. The fallback fires on
// `Applied`, which is `wsx`'s report that the walk REPLACED, and never on the
// mere presence of a record.
func TestAnIncompletePositionWalkNeverLatchesTheCanary(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Rung: "canary"})
	h.start()
	h.awaitActionable()

	// The exchange holds a position AND cannot answer for it. Both at once is
	// the case that matters: the fact is true and the read that would establish
	// it did not complete.
	h.ex.breakPositions(true)
	h.ex.setPosition(seamTicker, "0.01")
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("the canary latched with cause %q from a positions walk that "+
			"did not complete.\n\n"+
			"An incomplete walk applies nothing -- not q, not the drift streak, "+
			"not a record -- so there is no `q_local` transition to observe and "+
			"any latch here was taken from a reading that does not exist", got)
	}

	// And it is not that the fallback is dead: the same position, read
	// completely, is the thing it exists to catch.
	h.ex.breakPositions(false)
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "canary_position_nonzero" {
		t.Fatalf("the completed walk latched with cause %q, want "+
			"canary_position_nonzero; the previous assertion is only worth "+
			"anything if this one fires", got)
	}
}

// TestTheCanaryLatchesOnAnAcknowledgementThatCarriedAFill is §8.2's source.
//
// The acknowledgement's own fill count moves `q` before any fills walk runs, so
// a rule that watched only the walk would be up to one `position_poll_s` late
// on the one event it exists to react to.
//
// There is no baseline test on this source and none would mean anything: this
// is an acknowledgement of a create THIS process made, and `serve` starts the
// dispatcher only after `install` has returned. Every ack is live by
// construction.
func TestTheCanaryLatchesOnAnAcknowledgementThatCarriedAFill(t *testing.T) {
	// Seeded with inventory so the harness has an exit to place; the ack that
	// comes back carries a partial fill of a single quantum.
	h := newSeamHarness(t, seamOptions{
		Rung:      "canary",
		Positions: map[string]string{seamTicker: "1.00"},
	})
	h.ex.setAckFill("0.01")
	h.start()

	h.await("the first order to reach the exchange",
		func() bool { return h.ex.createCount() >= 1 })
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})

	if got := h.latchTrigger(); got != "canary_ack_fill" {
		t.Fatalf("an acknowledgement carrying 0.01 contracts latched with "+
			"cause %q, want canary_ack_fill.\n\n"+
			"§8.2 applies the ack's own count immediately. A canary that waits "+
			"for the fills walk to corroborate it is one that may place "+
			"another order first", got)
	}
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
}

// TestTheCanaryLatchesOnANewlyClassifiedOwnedFill is the fills-walk source, and
// the duplicate-report vector rides on the same fixture.
//
// The fills endpoint re-offers the whole walk on every poll. That is what
// `seenTrade` is for, and it is also why a rule written on "a fill is present"
// rather than "a fill is NEW" would re-decide the same stop every five seconds
// -- which is invisible while the first cause holds, and becomes a cause that
// overwrites the reason the harness stopped the moment anything else changes.
func TestTheCanaryLatchesOnANewlyClassifiedOwnedFill(t *testing.T) {
	const tradeID = "SEAM-CANARY-1"

	h := newSeamHarness(t, seamOptions{
		Rung:      "canary",
		Positions: map[string]string{seamTicker: "1.00"},
	})
	h.start()

	h.await("the exit to reach the exchange",
		func() bool { return h.ex.createCount() >= 1 })
	first, ok := h.ex.createAt(0)
	if !ok {
		t.Fatalf("no create was recorded")
	}
	// The fill can only classify as OURS once the binding is durable, and this
	// fixture never lists the order, so the inline binding of the dispatch path
	// is the only thing that can produce it.
	h.await("the dispatch-path binding to commit", func() bool {
		coid, bound := h.rig.store.Ownership().Bound(first.OrderID)
		return bound && coid == first.Coid
	})

	h.ex.addFill(map[string]any{
		"fill_id":           "SEAM-CANARY-FILL-1",
		"trade_id":          tradeID,
		"order_id":          first.OrderID,
		"ticker":            seamTicker,
		"side":              "no",
		"yes_price_dollars": "0.4500",
		"no_price_dollars":  "0.5500",
		"count":             "0.01",
		"is_taker":          false,
		"fee_cost":          "0.0000",
		"ts":                fmt.Sprintf("%d", h.clk.wallMs()),
	})
	// The exchange's position ALREADY contains the fill: 0.01 contracts of a NO
	// bid against a +1.00 YES position leaves +0.99. Keeping the two walks
	// consistent is what stops this becoming a drift test.
	h.ex.setPosition(seamTicker, "0.99")

	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})

	if got := h.latchTrigger(); got != "canary_owned_fill" {
		t.Fatalf("a newly classified owned fill of 0.01 contracts latched with "+
			"cause %q, want canary_owned_fill", got)
	}

	// The duplicate vector, and what it does and does not establish.
	//
	// The fills endpoint re-offers the whole walk on every poll, so this same
	// fill arrives three more times below. What is asserted here is what the
	// COMPOSED seam can observe: the cause on disk does not change and the A9
	// table does not grow. It is deliberately NOT a proof that the owner stops
	// re-deciding -- `commitStop` is first-writer-wins and `advance` is a no-op
	// once WINDING_DOWN, so a canary block that fired every poll would satisfy
	// everything below. The rule that makes the re-offer a non-event is
	// `seenTrade`, and it is pinned where it lives, in `risk`'s own dedup test.
	before := len(h.seamGlobalEvents())
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}
	if got := h.latchTrigger(); got != "canary_owned_fill" {
		t.Fatalf("the latch cause became %q after the same fill was re-offered "+
			"three more times, want canary_owned_fill; first-writer-wins is "+
			"what keeps the reason the operator investigates", got)
	}
	stops := 0
	for _, ev := range h.seamGlobalEvents() {
		if ev == "RUNNING->WINDING_DOWN/global_stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("%d RUNNING->WINDING_DOWN rows are on disk after one fill was "+
			"reported four times, want 1. §15's A9 table is how §10.4 "+
			"reconstructs what happened, and a transition recorded once per "+
			"poll is a table that describes a flapping harness that never "+
			"flapped", stops)
	}
	if after := len(h.seamGlobalEvents()); after != before {
		t.Fatalf("the A9 table grew from %d rows to %d while nothing but the "+
			"same duplicate fill arrived", before, after)
	}
}

// TestStartupHistoryIsNeverALiveCanaryFill is the rule that decides whether
// this harness can be restarted at all.
//
// The trap is specific and it is not hypothetical. §7.5 reads only `backfill_h`
// of history; the live poll reads ALL of it with a zero `since`. The filter is
// client-side, applied after a complete walk, so a fill older than the window
// is dropped from what startup classifies and seeds -- which means `seenTrade`
// has never heard of it -- and the very first live poll then presents it as a
// brand new owned fill. A canary that reacted to that would latch on every
// single restart, for a trade that happened last week.
//
// The boundary is therefore IDENTITY AND PHASE: the union of trade ids from
// every complete fills walk of §7.5, taken BEFORE the filter, frozen into the
// adoption. A timestamp cannot do this job -- `parseTsMillis` returns 0 for a
// stamp it cannot read and a 0 deliberately bypasses the filter, so an
// adoption-time cut-off is not even a total order over the records.
func TestStartupHistoryIsNeverALiveCanaryFill(t *testing.T) {
	const tradeID = "SEAM-OLD-1"
	const adoptedID = "SEAM-ADOPTED-1"
	const orderID = "EX-SEAM-OLD"

	h := newSeamHarness(t, seamOptions{Rung: "canary"})
	h.installOwnedOrder(orderID, 1)

	// Older than `backfill_h` (24h), so §7.5's filtered walk never sees it and
	// the live walk always does. That gap is the whole scenario.
	old := h.clk.wallMs() - int64(25*time.Hour/time.Millisecond)
	h.ex.addFill(map[string]any{
		"fill_id":           "SEAM-OLD-FILL-1",
		"trade_id":          tradeID,
		"order_id":          orderID,
		"ticker":            seamTicker,
		"side":              "yes",
		"yes_price_dollars": "0.4000",
		"no_price_dollars":  "0.6000",
		"count":             "0.01",
		"is_taker":          false,
		"fee_cost":          "0.0000",
		"ts":                fmt.Sprintf("%d", old),
	})
	// And one INSIDE the window, which §7.5 does classify and seed. It is the
	// same history by a different route, and it is the one `Adoption.OwnedFills`
	// carries.
	h.ex.addFill(map[string]any{
		"fill_id":           "SEAM-ADOPTED-FILL-1",
		"trade_id":          adoptedID,
		"order_id":          orderID,
		"ticker":            seamTicker,
		"side":              "yes",
		"yes_price_dollars": "0.4000",
		"no_price_dollars":  "0.6000",
		"count":             "0.01",
		"is_taker":          false,
		"fee_cost":          "0.0000",
		"ts":                fmt.Sprintf("%d", h.clk.wallMs()-1000),
	})

	h.start()
	h.awaitActionable()

	// The fill really did reach the owned-fill path. Without this the test
	// would pass on a harness that classified it foreign, deferred it, or never
	// read it at all -- three ways of not latching that prove nothing.
	h.await("the historical fill to be classified as ours and recorded",
		func() bool {
			_, found, err := h.rig.store.Reader().Fill(tradeID)
			return err == nil && found
		})

	// `lip-da6`. It reached `our_fill` by the LIVE route, because no other route
	// exists for a fill outside `backfill_h`: §7.5 never asked for it, so
	// `recordBackfilled` never wrote it, and this poll is genuinely its first
	// observation. H-ORD-6's first-observer-wins therefore makes the label
	// written here the label the row keeps forever, and the label is a claim
	// about provenance -- history we INFERRED from a walk over the past, not
	// trading we watched happen.
	//
	// Suppressing the canary was never enough on its own. `liveOwnedFill` runs
	// in `applyRead` AFTER `wsx.ApplyPortfolio` has applied the fill and after
	// `RecordFill` has been submitted, so before this the row was written
	// `backfilled = false` and an analysis joining `our_fill` against `rig.db`
	// read every restart as a burst of trading.
	oldRow, _, err := h.rig.store.Reader().Fill(tradeID)
	if err != nil {
		t.Fatalf("reading our_fill for the out-of-window fill: %v", err)
	}
	if !oldRow.Backfilled {
		t.Fatalf("the out-of-window fill %s is recorded with "+
			"backfilled = false.\n\n"+
			"It was already on the account when this process started -- §7.5's "+
			"complete walk carried its trade id and only the backfill_h filter "+
			"kept it out of what was seeded. The live walk asks with a zero "+
			"`since`, so this is the first and only time it is ever written, "+
			"and H-ORD-6 makes that first write permanent", tradeID)
	}

	// The adoption's own owned history reaches `our_fill`, flagged as what it
	// is. Nothing else will ever write these rows: `risk.Seed` marks their
	// trade ids seen, so the first live walk deduplicates them away and the
	// only other caller of `RecordFill` never sees them. Before this the table
	// began at the first fill the incarnation happened to watch land -- and
	// H-ORD-6 makes it the join against `rig.db`.
	h.await("the adopted fill to reach our_fill", func() bool {
		_, found, err := h.rig.store.Reader().Fill(adoptedID)
		return err == nil && found
	})
	row, _, err := h.rig.store.Reader().Fill(adoptedID)
	if err != nil {
		t.Fatalf("reading our_fill for the adopted fill: %v", err)
	}
	if !row.Backfilled {
		t.Fatalf("the adopted fill %s is recorded with backfilled = false.\n\n"+
			"It was INFERRED from a walk over the past, not observed happening. "+
			"An analysis that cannot tell the two apart reads every restart as "+
			"a burst of trading", adoptedID)
	}

	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("the canary latched with cause %q on a fill that was already "+
			"on the account when it started.\n\n"+
			"§7.5 saw this trade id in its complete fills walk and only the "+
			"`backfill_h` filter kept it out of what was seeded. Reacting to "+
			"it makes every restart of a canary that has ever traded latch "+
			"immediately, and the operator has no way to tell that from a real "+
			"first fill", got)
	}
	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the global state is %s after adopting an account with old "+
			"history on it, want RUNNING", s.Global)
	}
}

// TestTheCanarysFirstFractionalFillWindsItDownAndItStaysDown is the whole of
// pilot-plan §7.9 in one run: the chain from a 0.01-contract fill to a process
// that will not add again, and will not add again after a restart either.
//
// It is written end to end because every link in it was independently true
// before and the composition still was not. `lip-vxo` is the standing example:
// `BlockAdding` and `RetryLatch` were produced correctly, tested at the
// producer, and read by nobody -- so a stop that could not be written was a stop
// that never happened. The links here are the durable cause, §5.1's transition,
// §5.2's response to it, the cancel that must be CONFIRMED rather than merely
// requested, the exit that must survive all of it, and H-HALT-4's latch
// outliving the process.
func TestTheCanarysFirstFractionalFillWindsItDownAndItStaysDown(t *testing.T) {
	const tradeID = "SEAM-E2E-1"

	// ListCreated so an acknowledged order joins the resting book, which is what
	// makes "cancelled and confirmed absent" an observable fact about the
	// exchange rather than an assertion about our own intent queue.
	h := newSeamHarness(t, seamOptions{Rung: "canary", ListCreated: true})
	h.start()
	h.awaitActionable()

	// A flat canary quotes both sides at S=1. The YES bid is the one that fills.
	h.await("both quotes to reach the exchange",
		func() bool { return h.ex.createCount() >= 2 })
	adding, ok := h.ex.createAt(0)
	if !ok {
		t.Fatalf("no create was recorded")
	}
	if adding.WireSide != string(rest.Bid) {
		t.Fatalf("the first create is a %s, want a yes bid; this test needs to "+
			"know which order it is filling", adding.WireSide)
	}
	h.await("the dispatch-path binding to commit", func() bool {
		coid, bound := h.rig.store.Ownership().Bound(adding.OrderID)
		return bound && coid == adding.Coid
	})
	quotesBefore := h.ex.createCount()

	// ONE QUANTUM. Below `inv_soft`, below `inv_hard`, below `inv_kill`, and
	// inside `pos_drift_tol`: nothing in §16 has anything to say about it.
	h.ex.addFill(map[string]any{
		"fill_id":           "SEAM-E2E-FILL-1",
		"trade_id":          tradeID,
		"order_id":          adding.OrderID,
		"ticker":            seamTicker,
		"side":              "yes",
		"yes_price_dollars": "0.4000",
		"no_price_dollars":  "0.6000",
		"count":             "0.01",
		"is_taker":          false,
		"fee_cost":          "0.0000",
		"ts":                fmt.Sprintf("%d", h.clk.wallMs()),
	})
	h.ex.setPosition(seamTicker, "0.01")
	h.clk.Advance(h.cfg.Params.PositionPoll)

	// 1. The cause is DURABLE, and 2. §5.1 moved -- in that order, which is
	// H-HALT-4: the latch reaches disk before the in-memory state changes.
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "canary_owned_fill" {
		t.Fatalf("the latch reads %q, want canary_owned_fill", got)
	}

	// 3. §5.2's response: the adding side is cancelled and CONFIRMED absent,
	// and the exit is not.
	h.await("the adding side to be cancelled", func() bool {
		return seamContains(h.ex.deletedIDs(), adding.OrderID)
	})
	h.await("the adding side to be confirmed absent from the book",
		func() bool { return h.ex.restingCount() == 1 })

	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(3)
	}

	// 4. The exit REMAINS. I1 in one sentence: every stop path stops adding
	// risk and none of them stops reducing it. A harness that cancelled its own
	// exit on the way down would be holding the position it stopped to shed.
	if n := h.ex.restingCount(); n != 1 {
		t.Fatalf("%d order(s) rest while the canary holds 0.01 contracts under "+
			"WINDING_DOWN, want exactly 1 -- the capped reducer.\n\n"+
			"§5.2's stop sends the market to REDUCING: adding side cancelled "+
			"and confirmed absent, capped reducer resting. It never cancels "+
			"everything, which is the inversion the whole design turns on", n)
	}
	if m, _ := h.market(); m.State != quote.Reducing {
		t.Fatalf("the market is %s while holding inventory under WINDING_DOWN, "+
			"want REDUCING", m.State)
	}
	if got := h.ex.createCount(); got != quotesBefore {
		t.Fatalf("%d order(s) have been placed, up from %d before the fill; a "+
			"latched harness may cancel and may keep an exit alive, and may "+
			"place nothing new", got, quotesBefore)
	}

	// 5. Flat. The reducer's work is done and there is nothing left to manage.
	h.ex.setPosition(seamTicker, "0.00")
	for i := 0; i < 4; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(4)
	}
	h.await("the account to go quiet", func() bool {
		return h.ex.restingCount() == 0
	})

	// 6. And NO new adding order, which is the assertion the whole bead exists
	// for. §5.2's stop is not a pause: reaching flat does not license the
	// harness to start again, because the latch is still on disk.
	if got := h.ex.createCount(); got != quotesBefore {
		c, _ := h.ex.createAt(quotesBefore)
		t.Fatalf("%d order(s) have been placed, up from %d: the canary reached "+
			"flat and quoted again (first new coid %q).\n\n"+
			"This is the market-scoped brake's failure mode and the reason "+
			"§7.9 needs a GLOBAL latch: `inv_hard` self-clears at flat, so a "+
			"harness bounded only by it reduces to zero and resumes adding, "+
			"forever, one quantum at a time", got, quotesBefore, c.Coid)
	}
	if m, _ := h.market(); m.State != quote.Idle {
		t.Fatalf("the market is %s at flat under a halted global state, want "+
			"IDLE", m.State)
	}
	if s := h.snapshot(); s.Global.AddsRisk() {
		t.Fatalf("the global state is %s, want a halted one", s.Global)
	}

	// 7. It survives the process. H-HALT-4 makes the latch outlive the
	// incarnation that wrote it and §10.4 makes clearing it an operator action,
	// so the next start is a refusal and not a fresh RUNNING harness.
	h.stopServe()
	if err := h.rig.close(context.Background()); err != nil {
		t.Fatalf("the canary would not stop cleanly: %v", err)
	}
	if got := seamLatchTrigger(t, h.cfg.Paths.Latch); got != "canary_owned_fill" {
		t.Fatalf("the halt latch reads %q after the process ended, want "+
			"canary_owned_fill", got)
	}
	again, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
	if err == nil {
		if cerr := again.close(context.Background()); cerr != nil {
			t.Logf("closing the unexpectedly-constructed rig: %v", cerr)
		}
		t.Fatalf("a harness restarted onto the canary's own latch without " +
			"-resume.\n\nThat is HR-009 with the canary's name on it: the " +
			"first fill halts the harness, an unrelated crash kills the " +
			"process, `launchd KeepAlive` restarts it, and the bound the " +
			"whole rung is built on has cleared itself")
	}
	if !strings.Contains(err.Error(), "-resume") {
		t.Fatalf("the refusal does not tell the operator what to pass: %v", err)
	}
}

// TestStartupSeededInventoryNeverInvokesTheCanary is the other half of the
// history rule, on the source that has no identity to check.
//
// A canary restarted while holding a position is the ordinary case after its
// first fill: WINDING_DOWN keeps an exit alive, the process is bounced, and the
// next incarnation adopts inventory it did not create. The entry fallback fires
// on a TRANSITION -- `q_local` exactly flat against a nonzero exchange figure --
// and adoption seeds `q_local` from the exchange, so on every poll after it the
// two agree and there is no transition to see. A rule written on "the exchange
// reports a position" instead would latch every restart of a canary that holds
// anything, which is precisely the restart the operator needs to work.
func TestStartupSeededInventoryNeverInvokesTheCanary(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "canary",
		Positions: map[string]string{seamTicker: "1.00"},
	})
	h.start()
	h.awaitActionable()

	for i := 0; i < 4; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(3)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("the canary latched with cause %q on inventory it adopted at "+
			"startup.\n\n"+
			"§7.5 seeds q from the exchange, so this position was never a "+
			"transition from flat -- it was the state the process came up in. "+
			"A canary that stops for it cannot be restarted while holding "+
			"anything, which is every restart after its first fill", got)
	}
	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the global state is %s after adopting a position, want "+
			"RUNNING", s.Global)
	}
}

// TestATakerFillOnTheCanaryKeepsTheStrongerCause is the first-writer-wins
// ordering, measured rather than asserted about.
//
// A taker fill on the canary is truthfully described by both causes, and the
// operator of §10.4 reading the latch at 3am needs the one that says H-Q-3 has
// been violated -- a post_only that did not take effect, a marketable price, an
// API change -- and not the one that says the canary did what canaries do.
// `commitStop` keeps the FIRST cause, so this is decided entirely by the order
// of two blocks in `applyRead`.
func TestATakerFillOnTheCanaryKeepsTheStrongerCause(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "canary",
		Positions: map[string]string{seamTicker: "1.00"},
	})
	h.start()

	h.await("the exit to reach the exchange",
		func() bool { return h.ex.createCount() >= 1 })
	first, ok := h.ex.createAt(0)
	if !ok {
		t.Fatalf("no create was recorded")
	}
	h.await("the dispatch-path binding to commit", func() bool {
		coid, bound := h.rig.store.Ownership().Bound(first.OrderID)
		return bound && coid == first.Coid
	})

	h.ex.addFill(map[string]any{
		"fill_id":           "SEAM-TAKER-FILL-1",
		"trade_id":          "SEAM-TAKER-1",
		"order_id":          first.OrderID,
		"ticker":            seamTicker,
		"side":              "no",
		"yes_price_dollars": "0.4500",
		"no_price_dollars":  "0.5500",
		"count":             "0.01",
		"is_taker":          true,
		"fee_cost":          "0.0100",
		"ts":                fmt.Sprintf("%d", h.clk.wallMs()),
	})
	h.ex.setPosition(seamTicker, "0.99")

	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})

	if got := h.latchTrigger(); got != "portfolio_read" {
		t.Fatalf("a TAKER fill on the canary latched with cause %q, want "+
			"portfolio_read.\n\n"+
			"H-ORD-8 is the cheapest detector for the most expensive bug in "+
			"the system. Both causes are true of this event and only one of "+
			"them is worth being woken for, so the stronger one must reach "+
			"`requestStop` first -- first-writer-wins does the rest", got)
	}
}

// ---------------------------------------------------------------------------
// 14. lip-lqw -- F17, `inv_kill` reaches the durable global latch
// ---------------------------------------------------------------------------
//
// Every test in this section runs at the PILOT rung, and that is the subject
// rather than the setting. `stopOnFirstOwnedFill` is a canary property, so a
// pilot has no first-fill bound at all and the inventory ladder IS its backstop:
// `inv_hard` market-scoped, `inv_kill` global. Until this bead the second of
// those was parsed, ordered against its neighbours by `Validate`, and read by
// nothing -- so the rung the burn-in actually runs at had no global inventory
// backstop whatsoever. A canary fixture would hide that, because the canary
// stops on the first fill and never reaches these figures.

// TestInventoryBeyondInvKillLatchesTheGlobalHaltByName is F17's whole claim:
// §6.4.5 clause 5 and the §12 row both say a position past `inv_kill` is a
// GLOBAL halt, and §11 gives it SEV1.
//
// The position is stepped from 15.00 rather than seeded past the threshold, and
// the step is what makes the test measure the right value. Seeded inventory is
// adopted, so `q_local` and `q_exch` are equal on every poll and a detector
// reading either one would pass -- while the rule is specifically about the
// exchange's figure AFTER H-POS-1's overwrite. Stepping leaves them disagreeing
// at the moment of the breach, and by 3.01 contracts, which is inside
// `pos_drift_hard` (5) so nothing else stops the harness and the latch records
// this rule's cause rather than a drift's.
func TestInventoryBeyondInvKillLatchesTheGlobalHaltByName(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "15.00"},
	})
	h.start()
	h.awaitActionable()

	// The control. 15.00 is over `inv_hard` and under `inv_kill`, so the market
	// is already REDUCING and the harness is still RUNNING -- everything below
	// is attributable to the step alone.
	h.awaitTicks(2)
	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the global state is %s while holding 15.00 contracts, want "+
			"RUNNING: §16 puts that between inv_hard and inv_kill, which is the "+
			"market-scoped brake's band and not the global one's", s.Global)
	}
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("the halt latch already reads %q before the test did anything",
			got)
	}

	h.ex.setPosition(seamTicker, "18.01")
	h.clk.Advance(h.cfg.Params.PositionPoll)

	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "inv_kill" {
		t.Fatalf("18.01 contracts against inv_kill 18.00 latched with cause "+
			"%q, want inv_kill.\n\n"+
			"The latch is the one artefact that survives the process, and §10.4 "+
			"has the operator read its cause to learn WHICH row of the halt "+
			"table fired. `portfolio_read` is the label five different causes "+
			"already share; a named row of §12 arriving under it is a halt "+
			"whose reason cannot be recovered at 3am", got)
	}
	h.await("the SEV1 INVENTORY_KILL anomaly to reach the store", func() bool {
		return seamContains(h.anomalyClasses(), "INVENTORY_KILL")
	})
}

// TestInventoryExactlyAtInvKillIsNotABreach is the strict `>`.
//
// `Validate` orders inv_soft < inv_hard < inv_kill strictly, so §12 assigns the
// whole band up to and including `inv_kill` to the MARKET-scoped row and only
// what is past it to the global one. A `>=` here takes the global halt at a
// figure the spec gives to the other row, and it does it one quantum early --
// which on the ladder §16 actually ships is the difference between a market
// that keeps reducing and a process that stops adding everywhere.
func TestInventoryExactlyAtInvKillIsNotABreach(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "15.00"},
	})
	h.start()
	h.awaitActionable()

	h.ex.setPosition(seamTicker, "18.00")
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("exactly 18.00 contracts against inv_kill 18.00 latched with "+
			"cause %q, and the comparison is specified strict", got)
	}
	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the global state is %s at exactly inv_kill, want RUNNING",
			s.Global)
	}
	m, ok := h.market()
	if !ok || m.State != quote.Reducing {
		t.Fatalf("the market is %v (present=%v) at exactly inv_kill, want "+
			"REDUCING: the position is far past inv_hard and the market-scoped "+
			"brake is what owns this band", m.State, ok)
	}
}

// TestInventoryBetweenInvHardAndInvKillStopsOneMarketAndNotTheProcess is the
// bead's central claim, and the one assertion that separates the two rows.
//
// harness-spec.md:1599-1600 distinguishes them by SCOPE and by nothing else:
// `inv_hard` is market-scoped, `inv_kill` is global and durable. A detector
// pointed at the wrong threshold still stops the harness on every position this
// suite drives past `inv_kill`, so no amount of evidence that the breach fires
// can tell the two apart. Only evidence that it does NOT fire can.
func TestInventoryBetweenInvHardAndInvKillStopsOneMarketAndNotTheProcess(
	t *testing.T) {

	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "10.00"},
	})
	h.start()
	h.awaitActionable()

	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	m, ok := h.market()
	if !ok || m.State != quote.Reducing {
		t.Fatalf("the market is %v (present=%v) holding 10.00 contracts "+
			"against inv_hard 7.00, want REDUCING", m.State, ok)
	}
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("10.00 contracts latched the GLOBAL halt with cause %q.\n\n"+
			"§16 puts inv_hard at 7.00 and inv_kill at 18.00, and §12 gives the "+
			"band between them to the market-scoped row: adds off in THIS "+
			"market, reducer live, every other market untouched. A global halt "+
			"here is the harness treating its ordinary taper as its kill "+
			"switch, and the two thresholds stop existing separately", got)
	}
	if s := h.snapshot(); s.Global != quote.Running {
		t.Fatalf("the global state is %s holding 10.00 contracts, want RUNNING",
			s.Global)
	}
}

// TestAnIncompletePositionWalkNeverLatchesInvKill is H-PAGE-1 applied to F17.
//
// "Stale, never empty" is the whole of it, and the trap it names is specific: a
// truncated positions walk that reads as flat everywhere. `applyPositions`
// refuses a walk that did not replace, so the breach is never evaluated against
// a partial reading -- neither created from one nor cleared by one.
//
// The second half is not decoration. An assertion that nothing fired is
// satisfied by a detector that cannot fire at all, and this bead exists because
// exactly that went unnoticed for the whole of `inv_kill`'s life.
func TestAnIncompletePositionWalkNeverLatchesInvKill(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "15.00"},
	})
	h.start()
	h.awaitActionable()

	// The breach is TRUE on the account and the read that would establish it
	// does not complete. Both at once is the case that matters.
	h.ex.breakPositions(true)
	h.ex.setPosition(seamTicker, "18.01")
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("F17 latched with cause %q from a positions walk that did not "+
			"complete.\n\n"+
			"An incomplete walk applies nothing -- not q, not the drift streak, "+
			"not a record -- so there is no authoritative |q| to compare "+
			"against inv_kill, and any halt taken here was decided from a "+
			"reading that does not exist", got)
	}

	h.ex.breakPositions(false)
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "inv_kill" {
		t.Fatalf("the completed walk latched with cause %q, want inv_kill; the "+
			"assertion above is only worth anything if this one fires", got)
	}
}

// TestAnInvKillBreachThatReducesToFlatDoesNotResumeAdding is the failure the
// bead was filed for, and it is the only one of these that is about what
// happens AFTER the halt.
//
// `quote/machine.go` takes REDUCING -> IDLE at exactly flat, and IDLE -> QUOTING
// is an ordinary edge. So a breach that reduced itself away would leave a market
// that had every reason to resume adding, and the harness would work its way
// back to a position, breach again, and repeat -- with no operator ever told,
// because nothing about that loop is an error.
//
// The durable latch is what forbids it: `NextGlobal` reads `Latched` before
// every other rule (A14) and §10.4 provides no edge back to RUNNING without an
// operator `-resume`. This test is that claim measured end to end rather than
// argued from the state table.
func TestAnInvKillBreachThatReducesToFlatDoesNotResumeAdding(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "15.00"},
	})
	h.start()
	h.awaitActionable()

	h.ex.setPosition(seamTicker, "18.01")
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "inv_kill" {
		t.Fatalf("the breach latched with cause %q, want inv_kill", got)
	}

	// The exit fills, in the sense the positions endpoint reports: the market
	// reduces all the way to exactly zero, which is the figure §5.2 lets a
	// REDUCING market leave on.
	h.ex.setPosition(seamTicker, "0.00")
	for i := 0; i < 4; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(3)
	}

	// The create baseline is taken HERE and not at the latch, because a create
	// between the two is the reducing quote and §12 requires it: the row for
	// this trigger reads "adds off everywhere, reducer LIVE". What must not
	// exist is a create issued once the market is flat -- there is nothing left
	// to reduce, so any order placed from here is an adding one.
	placedWhileWindingDown := h.ex.createCount()
	for i := 0; i < 4; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(3)
	}

	if s := h.snapshot(); s.Global.AddsRisk() {
		t.Fatalf("the global state is %s after the breached inventory reduced "+
			"to flat, and AddsRisk() is true.\n\n"+
			"That is the harness quoting again after a §12 global halt, with no "+
			"operator action and nothing on disk cleared. The latch outranks "+
			"every other rule in NextGlobal precisely so this cannot happen",
			s.Global)
	}
	if m, ok := h.market(); ok && (m.State == quote.Quoting ||
		m.State == quote.Skewed) {
		t.Fatalf("the market is %v after the breach reduced to flat, and both "+
			"QUOTING and SKEWED rest an ADDING side", m.State)
	}
	if got := h.ex.createCount(); got != placedWhileWindingDown {
		t.Fatalf("%d further order(s) reached the exchange after the halted "+
			"harness reached flat (%d before, %d after).\n\n"+
			"A flat market has nothing to reduce, so every one of them is an "+
			"adding order placed under a §12 global halt",
			got-placedWhileWindingDown, placedWhileWindingDown, got)
	}
	if got := h.latchTrigger(); got != "inv_kill" {
		t.Fatalf("the latch reads %q after the position went flat, want "+
			"inv_kill still: a halt that clears itself when the condition "+
			"passes is not durable", got)
	}
}

// TestReadOnlyRunRecordsWouldWriteWithoutSendingNonGET is H-VER-1 as a property
// of the whole composed process, and it is the test the bead exists for.
//
// A rehearsal is only worth running if it exercises everything except the write.
// So the assertions are in two directions at once, and both are load-bearing:
//
//   - The harness REACHES the decision. It adopts the position, publishes a
//     REDUCING market at an actionable touch, wants to rest an exit, takes a
//     durable reservation for it, and abandons that reservation when the write
//     is refused. That is the five-table evidence of a would-write, and it is
//     recorded with no sixth table -- an `owned_order` row that is neither bound
//     nor outstanding is exactly what "we would have placed this" looks like.
//   - NOTHING non-GET reaches the transport. Not one POST, not one DELETE, and
//     the read walks keep going, so this is a complete observer that cannot act.
//
// A test that asserted only the second half would pass on a harness that
// crashed at startup.
func TestReadOnlyRunRecordsWouldWriteWithoutSendingNonGET(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		ReadOnly:  true,
		Positions: map[string]string{seamTicker: "8.00"},
	})
	h.start()
	h.awaitActionable()

	m, ok := h.market()
	if !ok {
		t.Fatal("no snapshot was published; a read-only harness must still " +
			"reach every truth a live one does")
	}
	if m.State != quote.Reducing {
		t.Fatalf("the market is %s, want REDUCING. The point of this test is "+
			"that §6.2 WANTED to rest an exit and structurally could not",
			m.State)
	}

	// Several ticks, so this is not a claim about one instant.
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	h.ex.mu.Lock()
	creates, deletes, walks := len(h.ex.creates), len(h.ex.deletes), h.ex.ordersWalks
	h.ex.mu.Unlock()

	if creates != 0 || deletes != 0 {
		t.Fatalf("a read-only run transmitted %d create(s) and %d delete(s). "+
			"Neither key was present, so this process was structurally "+
			"incapable of it -- and every rehearsal, qualification run and "+
			"dry run this repository plans is worth exactly as much as that "+
			"guarantee", creates, deletes)
	}
	if walks == 0 {
		t.Fatal("no orders walk was served, so the harness was not reading " +
			"either; a process that neither reads nor writes rehearses nothing")
	}

	// The would-write, on disk. `Abandoned` is the terminal a refused create
	// leaves behind: the reservation was taken durably BEFORE the write was
	// attempted (H-ORD-6's barrier) and then closed out when it was refused.
	rows, err := h.rig.store.Reader().OwnedOrders()
	if err != nil {
		t.Fatalf("reading owned_order: %v", err)
	}
	abandoned := 0
	for _, r := range rows {
		if r.Abandoned {
			abandoned++
		}
		if r.Bound {
			t.Fatalf("coid %s is BOUND in a read-only run: an order id can "+
				"only come from an exchange that answered a create", r.Coid)
		}
	}
	if abandoned == 0 {
		t.Fatalf("no abandoned reservation is recorded across %d owned_order "+
			"row(s). The refusal has to leave the same durable trail as any "+
			"other unplaced order, or a rehearsal proves the harness declined "+
			"to trade and not that it decided to and was stopped", len(rows))
	}
}

// TestLiveComposedRunReachesRawDoerWithBothWriteKeys is the control.
//
// Every assertion in the read-only test above is about an ABSENCE, and an
// absence is the easiest thing in the world to produce by accident -- a harness
// that failed to start, a fixture that seeded no position, a book that never
// qualified. This is the same composition with both keys present, and it must
// reach the transport. Without it, the pair above could be passing because
// nothing works.
func TestLiveComposedRunReachesRawDoerWithBothWriteKeys(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Positions: map[string]string{seamTicker: "8.00"},
	})
	if !h.cfg.Live {
		t.Fatal("the seam harness is not armed, so this control asserts nothing")
	}
	if _, err := os.Stat(h.cfg.Paths.LiveOK); err != nil {
		t.Fatalf("the live_ok sentinel is absent: %v", err)
	}

	h.start()
	h.awaitActionable()
	h.await("a write to reach the raw transport", func() bool {
		h.ex.mu.Lock()
		defer h.ex.mu.Unlock()
		return len(h.ex.creates) > 0 || len(h.ex.deletes) > 0
	})
}

// ---------------------------------------------------------------------------
// F6 -- the composed network layer, end to end
// ---------------------------------------------------------------------------

// F6 -- the composed network layer, end to end.
//
// Every other test in this package substitutes at `exchange`, so nothing in the
// tree has ever exercised the production wiring underneath it: the two
// transports, the shared resolver cache, and the fallback's route into the
// process anomaly sink. These four tests are the only ones that do, and they do
// it WITHOUT a live endpoint -- a scripted resolver, an injected dial, and two
// local TLS servers standing in for the exchange's two hostnames.
//
// The scripted resolver answers with TEST-NET-1 addresses (RFC 5737, "for use
// in documentation, and never routed"). They are never dialled: the injected
// dial maps each one to a loopback listener. That is what makes it possible to
// give the two hostnames DIFFERENT servers while both keep port 443, and it
// makes every assertion about which cached address was used exact.

const (
	// The two hostnames the harness actually resolves, and the TEST-NET-1
	// address each one is scripted to answer with.
	f6RESTHost = "api.elections.kalshi.com"
	f6WSHost   = "external-api-ws.kalshi.com"
	f6RESTAddr = "192.0.2.1"
	f6WSAddr   = "192.0.2.2"

	// f6Ticker is on the SECOND page of the programs walk, deliberately.
	// `feed.Universe` read one page of 200 and never sent a cursor, so a market
	// past the first page was invisible to it -- and invisible reads as "not in
	// the active programme", which is a refusal to start on a market that pays.
	f6Ticker = "KXF6-26AUG10-T2"
	f6Target = 250.5
)

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

// f6Fixture is a scripted resolver, an injected dial, and the two local servers
// they lead to.
type f6Fixture struct {
	t *testing.T

	mu      sync.Mutex
	wedged  bool
	lookups map[string]int
	dialed  []string
	route   map[string]string

	restHosts, restSNI []string
	wsHosts, wsSNI     []string
	programPages       int
	balances           int
	handshakes         int

	now time.Time

	pool *x509.CertPool
}

// LookupHost is the `netx.Resolver` seam. Wedging it is the whole point: it is
// what `getaddrinfo` does on this machine roughly every 2.5 hours.
func (f *f6Fixture) LookupHost(_ context.Context, host string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups[host]++
	if f.wedged {
		return nil, fmt.Errorf("scripted resolver is wedged for %s", host)
	}
	switch host {
	case f6RESTHost:
		return []string{f6RESTAddr}, nil
	case f6WSHost:
		return []string{f6WSAddr}, nil
	}
	return nil, fmt.Errorf("no scripted answer for %s", host)
}

// dial is the `netx.DialFunc` seam. It records the address the cache CHOSE --
// which is the thing under test -- and then routes it to a loopback listener.
func (f *f6Fixture) dial(ctx context.Context, network, address string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, address)
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	real, ok := f.route[host]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("nothing is listening for %s", address)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, real)
}

func (f *f6Fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *f6Fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *f6Fixture) wedge() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wedged = true
}

// f6Snapshot is everything the fixture observed, taken under the lock so a test
// asserts against one consistent reading. It is a separate type because the
// fixture holds a mutex and a `*testing.T`, neither of which may be copied.
type f6Snapshot struct {
	lookups            map[string]int
	dialed             []string
	restHosts, restSNI []string
	wsHosts, wsSNI     []string
	programPages       int
	balances           int
	handshakes         int
}

func (f *f6Fixture) snapshot() f6Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f6Snapshot{
		lookups: map[string]int{
			f6RESTHost: f.lookups[f6RESTHost],
			f6WSHost:   f.lookups[f6WSHost],
		},
		dialed:       append([]string(nil), f.dialed...),
		restHosts:    append([]string(nil), f.restHosts...),
		restSNI:      append([]string(nil), f.restSNI...),
		wsHosts:      append([]string(nil), f.wsHosts...),
		wsSNI:        append([]string(nil), f.wsSNI...),
		programPages: f.programPages,
		balances:     f.balances,
		handshakes:   f.handshakes,
	}
}

// newF6Fixture generates a CA and one leaf covering BOTH exchange hostnames,
// starts a TLS REST server and a TLS websocket server, and points the scripted
// resolver at them.
//
// A real CA and a real leaf, because certificate verification stays ON. That is
// what makes `TestF6FallbackPreservesHostAndSNIOnRESTAndWebSocket` an assertion
// rather than a formality: if the numeric fallback address reached SNI, the
// handshake would fail against a certificate that names hostnames.
func newF6Fixture(t *testing.T) *f6Fixture {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "f6 test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: f6RESTHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// NAMES ONLY. No IPAddresses entry, so a handshake that carried the
		// numeric fallback address as its server name cannot verify -- which is
		// exactly what `M-7ZT-IPHOST` produces.
		DNSNames: []string{f6RESTHost, f6WSHost},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf := tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	f := &f6Fixture{
		t:       t,
		lookups: map[string]int{},
		route:   map[string]string{},
		// A fixed instant, because the one-hour floor is stepped over
		// explicitly and a wall clock cannot be.
		now:  time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		pool: pool,
	}

	restSrv := httptest.NewUnstartedServer(http.HandlerFunc(f.serveREST))
	restSrv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	restSrv.StartTLS()
	t.Cleanup(restSrv.Close)

	wsSrv := httptest.NewUnstartedServer(http.HandlerFunc(f.serveWS))
	wsSrv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	wsSrv.StartTLS()
	t.Cleanup(wsSrv.Close)

	f.route[f6RESTAddr] = restSrv.Listener.Addr().String()
	f.route[f6WSAddr] = wsSrv.Listener.Addr().String()
	return f
}

// serveREST answers the two endpoints `exchangeOver` and the poll loop need.
func (f *f6Fixture) serveREST(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.restHosts = append(f.restHosts, r.Host)
	if r.TLS != nil {
		f.restSNI = append(f.restSNI, r.TLS.ServerName)
	}
	f.mu.Unlock()

	switch r.URL.Path {
	case rest.APIPrefix + rest.EpPrograms.Path:
		f.servePrograms(w, r)
	case rest.APIPrefix + "/portfolio/balance":
		f.mu.Lock()
		f.balances++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"balance": 123456})
	default:
		http.Error(w, "unscripted path "+r.URL.Path, http.StatusNotFound)
	}
}

// servePrograms is a TWO-page walk with the ticker under test on page two.
func (f *f6Fixture) servePrograms(w http.ResponseWriter, r *http.Request) {
	if got := r.URL.Query().Get("status"); got != "active" {
		http.Error(w, "status filter was "+got, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.programPages++
	f.mu.Unlock()

	switch r.URL.Query().Get("cursor") {
	case "":
		writeJSON(w, map[string]any{
			"next_cursor": "F6PAGE2",
			"incentive_programs": []any{
				map[string]any{"market_ticker": "KXF6-26AUG10-T1",
					"target_size_fp": "1000.00"},
			},
		})
	case "F6PAGE2":
		writeJSON(w, map[string]any{
			"next_cursor": "",
			"incentive_programs": []any{
				map[string]any{"market_ticker": f6Ticker,
					"target_size_fp": "250.50"},
			},
		})
	default:
		http.Error(w, "unknown cursor", http.StatusBadRequest)
	}
}

func (f *f6Fixture) serveWS(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.handshakes++
	f.wsHosts = append(f.wsHosts, r.Host)
	if r.TLS != nil {
		f.wsSNI = append(f.wsSNI, r.TLS.ServerName)
	}
	f.mu.Unlock()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	// A read loop, and not a wait on the request context. `coder/websocket`
	// answers the peer's close frame from inside its READ path, so a server
	// that never reads leaves every graceful `Close` to time out.
	for {
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, body map[string]any) {
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(raw); err != nil {
		return
	}
}

// f6Compose builds the production network layer over the fixture, exactly as
// `productionExchange` builds it over the system's.
//
// The two transports are the ones production made; the test only installs the
// CA on them. `InsecureSkipVerify` is never set, so every handshake below is a
// real verification against the leaf's DNS names.
func f6Compose(t *testing.T, f *f6Fixture) (*anomalySink, *f6Net) {
	t.Helper()
	anom := newAnomalySink()
	nt, err := newF6Net(anom, f, f.dial, f.clock)
	if err != nil {
		t.Fatalf("newF6Net: %v", err)
	}
	for _, tr := range []*http.Transport{nt.rest, nt.ws} {
		tr.TLSClientConfig = &tls.Config{RootCAs: f.pool}
		// The fixture's dial is the only route to the servers; a proxy read out
		// of the developer's environment would bypass it.
		tr.Proxy = nil
	}
	return anom, nt
}

// f6NoDefaultTransport makes `net/http`'s default transport refuse.
//
// `M-7ZT-RESTBYPASS` and `M-7ZT-WSBYPASS` both build production on that default
// transport, which resolves through the real system stack and dials the real
// exchange. Refusing it here means those mutations fail against a local error
// rather than against Kalshi: the gate must never send a request to the account
// this harness trades on, mutated or not.
func f6NoDefaultTransport(t *testing.T) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = f6RefusingTransport{}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

type f6RefusingTransport struct{}

func (f6RefusingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("F6: %s was issued on net/http's DEFAULT transport, "+
		"which is not the one carrying the resolver cache. Production must "+
		"build both the REST Doer and the websocket Dialer on a transport over "+
		"`netx.CachedDialer`", r.URL.Host)
}

// f6Signer is a real `feed.Signer` over a freshly generated key. Real, because
// the handshake headers are what the local websocket server receives, and a
// fake signer would prove the plumbing and nothing about the request.
func f6Signer(t *testing.T) *feed.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "kalshi.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath,
		[]byte("KALSHI_API_KEY_ID=f6-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := feed.NewSignerFrom(keyPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// f6WSHeaders is the signed handshake header set the production dialer sends.
func f6WSHeaders(t *testing.T, s *feed.Signer) http.Header {
	t.Helper()
	m, err := s.WSHeaders(time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func f6Config(t *testing.T) config {
	c := newSeamConfig(t, "")
	c.Ticker = f6Ticker
	return c
}

// dialsTo counts how many dials went to one sentinel address, on port 443.
func (f f6Snapshot) dialsTo(addr string) int {
	want := net.JoinHostPort(addr, "443")
	n := 0
	for _, d := range f.dialed {
		if d == want {
			n++
		}
	}
	return n
}

// f6Drain empties the sink without submitting, the way `takeRaised` does for a
// rig. This one takes the sink directly because F6 raises before a rig exists.
func f6Drain(s *anomalySink) []risk.Anomaly {
	var out []risk.Anomaly
	for {
		select {
		case a := <-s.ch:
			out = append(out, a)
		default:
			return out
		}
	}
}

// The REST half of the production wiring: the active-programme walk that
// replaced `feed.Universe`, and an ordinary portfolio read, both over the
// transport carrying the resolver cache.
//
// The Target Size assertion is the load-bearing one. It comes off the SECOND
// page, so it is simultaneously the proof that the walk is complete and the
// proof that it happened over this transport -- `M-7ZT-RESTBYPASS` cannot
// satisfy it, because the default transport has no route to the fixture at all.
func TestProductionRESTUsesF6DialerForActiveProgramsAndPortfolio(t *testing.T) {
	f6NoDefaultTransport(t)
	f := newF6Fixture(t)
	anom, nt := f6Compose(t, f)
	ctx := context.Background()

	ex, err := exchangeOver(ctx, f6Config(t), f6Signer(t), nt)
	if err != nil {
		t.Fatalf("exchangeOver could not build the production exchange over the "+
			"F6 transports: %v", err)
	}
	if ex.Target != f6Target {
		t.Fatalf("Target Size = %v, want %v. It is read from page TWO of the "+
			"programs walk, so a wrong or missing value means either the walk "+
			"stopped at page one -- `feed.Universe`'s defect -- or it did not "+
			"traverse the cached transport", ex.Target, f6Target)
	}

	// The portfolio half. Same Doer, same transport, same cache.
	if _, err := rest.NewClient(ex.Doer).Balance(ctx); err != nil {
		t.Fatalf("a portfolio read over the F6 transport failed: %v", err)
	}

	got := f.snapshot()
	if got.programPages != 2 {
		t.Fatalf("the programs endpoint served %d pages, want 2: the walk must "+
			"be COMPLETE, and a single-page read is what makes a market past "+
			"the first page look absent from the incentive programme",
			got.programPages)
	}
	if got.balances != 1 {
		t.Fatalf("the local exchange served %d balance requests, want 1",
			got.balances)
	}
	if n := got.dialsTo(f6RESTAddr); n == 0 || n != len(got.dialed) {
		t.Fatalf("dialled %v; every REST dial must go to the cached address %s "+
			"and nothing else may be dialled at all", got.dialed, f6RESTAddr)
	}
	if got.lookups[f6RESTHost] != 1 {
		t.Fatalf("the resolver was consulted %d times for %s, want 1. Inside "+
			"the one-hour floor the cached answer is used WITHOUT asking, which "+
			"is the cheapest possible way of surviving a resolver that wedges "+
			"mid-run", got.lookups[f6RESTHost], f6RESTHost)
	}
	if raised := f6Drain(anom); len(raised) != 0 {
		t.Fatalf("a healthy resolver raised %d anomalies: %+v. A fallback alert "+
			"on the happy path is an operator who learns to ignore it", len(raised), raised)
	}
}

// The websocket half. It is a SEPARATE transport over the SAME cache, and this
// test is the only thing in the tree that says so.
func TestProductionWebSocketUsesF6Dialer(t *testing.T) {
	f6NoDefaultTransport(t)
	f := newF6Fixture(t)
	_, nt := f6Compose(t, f)
	signer := f6Signer(t)
	ctx := context.Background()

	ex, err := exchangeOver(ctx, f6Config(t), signer, nt)
	if err != nil {
		t.Fatalf("exchangeOver: %v", err)
	}

	// Two dials, because F6's websocket case is a RECONNECT: the wedge takes
	// the socket down, and the reconnect is the thing that has to survive it.
	for i := 0; i < 2; i++ {
		sock, err := ex.Dialer.Dial(ctx, wsx.WSURL, f6WSHeaders(t, signer))
		if err != nil {
			t.Fatalf("websocket dial %d over the F6 transport failed: %v.\n\n"+
				"The production Dialer must be built with "+
				"`wsx.NewLiveDialerWithTransport` over the transport carrying "+
				"`netx.CachedDialer`; on net/http's default transport the "+
				"handshake resolves through the system stack that wedges",
				i+1, err)
		}
		if err := sock.Close(); err != nil {
			t.Fatalf("closing socket %d: %v", i+1, err)
		}
	}

	got := f.snapshot()
	if got.handshakes != 2 {
		t.Fatalf("the local websocket server completed %d handshakes, want 2",
			got.handshakes)
	}
	if n := got.dialsTo(f6WSAddr); n != 2 {
		t.Fatalf("%d of the dials %v went to the cached websocket address %s, "+
			"want 2", n, got.dialed, f6WSAddr)
	}
	if got.lookups[f6WSHost] != 1 {
		t.Fatalf("the resolver was consulted %d times for %s, want 1: the "+
			"reconnect is inside the one-hour floor and must not ask again",
			got.lookups[f6WSHost], f6WSHost)
	}
	// ONE cache, holding both hostnames. The REST host was resolved by the
	// programs walk above and the websocket dials did not disturb it.
	if got.lookups[f6RESTHost] != 1 {
		t.Fatalf("%s was resolved %d times, want 1; the two transports must "+
			"share one `netx.CachedDialer`, not hold one each",
			f6RESTHost, got.lookups[f6RESTHost])
	}
	// H-FAIL-2: distinct transports. REST is how a cancel still reaches the
	// exchange when the feed is gone, and a connection-pool fault shared with
	// the feed takes out the escape route along with it.
	if nt.rest == nt.ws {
		t.Fatal("the REST and websocket transports are the same *http.Transport")
	}
}

// The SNI clause, which is the whole subtlety of §F6 and the reason this is a
// dialer rather than a URL rewrite.
//
// The fallback substitutes the TCP destination and NOTHING else, so `Host` and
// the TLS server name are still derived from the URL. The leaf certificate here
// carries DNS names and no IP address, and verification is left ON -- so a
// numeric address reaching either one does not merely look wrong, it fails to
// connect. That is `M-7ZT-IPHOST`.
func TestF6FallbackPreservesHostAndSNIOnRESTAndWebSocket(t *testing.T) {
	f6NoDefaultTransport(t)
	f := newF6Fixture(t)
	anom, nt := f6Compose(t, f)
	signer := f6Signer(t)
	ctx := context.Background()

	// (1) SEED both hostnames. §F6 falls back only after a prior SUCCESSFUL
	// resolution: with nothing cached there is nothing to fall back to, and
	// inventing an address would be a connection to somewhere never verified.
	ex, err := exchangeOver(ctx, f6Config(t), signer, nt)
	if err != nil {
		t.Fatalf("seeding the cache through the programs walk: %v", err)
	}
	seed, err := ex.Dialer.Dial(ctx, wsx.WSURL, f6WSHeaders(t, signer))
	if err != nil {
		t.Fatalf("seeding the cache through the websocket handshake: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seeding socket: %v", err)
	}
	// A pooled connection would carry the next request without dialling at all.
	// The wedge this models arrives with the socket already gone.
	nt.rest.CloseIdleConnections()
	base := f.snapshot()

	// (2) THE WEDGE, past the floor so the resolver is consulted and fails.
	f.wedge()
	f.advance(netx.FloorTTL + time.Second)

	if _, err := rest.NewClient(ex.Doer).Balance(ctx); err != nil {
		t.Fatalf("the REST fallback did not connect: %v.\n\n"+
			"If this is a certificate error naming an IP address, the numeric "+
			"fallback address has reached the TLS server name. §F6 requires "+
			"SNI PRESERVED: substitute the TCP destination and nothing else",
			err)
	}
	sock, err := ex.Dialer.Dial(ctx, wsx.WSURL, f6WSHeaders(t, signer))
	if err != nil {
		t.Fatalf("the websocket fallback did not connect: %v", err)
	}
	if err := sock.Close(); err != nil {
		t.Fatalf("closing the fallback socket: %v", err)
	}

	got := f.snapshot()

	// (3) EXACTLY ONE of each, so nothing below is asserted about a retry.
	if n := got.balances - base.balances; n != 1 {
		t.Fatalf("the fallback produced %d REST requests, want exactly 1", n)
	}
	if n := got.handshakes - base.handshakes; n != 1 {
		t.Fatalf("the fallback produced %d handshakes, want exactly 1", n)
	}

	// (4) The hostname survived, in the header AND in SNI, on both.
	for _, c := range []struct {
		what      string
		host, sni []string
		want      string
	}{
		{"REST", got.restHosts, got.restSNI, f6RESTHost},
		{"the websocket", got.wsHosts, got.wsSNI, f6WSHost},
	} {
		gotHost := c.host[len(c.host)-1]
		gotSNI := c.sni[len(c.sni)-1]
		if gotHost != c.want {
			t.Fatalf("%s sent Host %q on the fallback, want %q", c.what, gotHost, c.want)
		}
		if gotSNI != c.want {
			t.Fatalf("%s sent SNI %q on the fallback, want %q. The certificate "+
				"is issued for hostnames and for no IP address, exactly as the "+
				"exchange's is", c.what, gotSNI, c.want)
		}
	}

	// (5) And it was a real verification, not a waived one. A fallback that
	// worked by disabling certificate checking would satisfy every assertion
	// above and would be a signed session against whatever answered.
	for _, tr := range []*http.Transport{nt.rest, nt.ws} {
		if tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("certificate verification is disabled on a production " +
				"transport")
		}
	}

	// (6) It really was the FALLBACK and not a lucky cache hit.
	raised := f6Drain(anom)
	if len(raised) != 2 {
		t.Fatalf("%d fallback anomalies were raised, want 2 (one REST, one "+
			"websocket): %+v", len(raised), raised)
	}
	for _, a := range raised {
		if a.Class != "DNS_FALLBACK" {
			t.Fatalf("fallback raised %s, want DNS_FALLBACK", a.Class)
		}
	}
	if n := got.dialsTo(f6RESTAddr) - base.dialsTo(f6RESTAddr); n != 1 {
		t.Fatalf("%d REST dials went to the last-known-good address, want 1", n)
	}
	if n := got.dialsTo(f6WSAddr) - base.dialsTo(f6WSAddr); n != 1 {
		t.Fatalf("%d websocket dials went to the last-known-good address, want 1", n)
	}
}

// §F6's SEV2, on the process sink and in the durable journal.
//
// The alert is the only reason the operator ever learns the wedge happened: the
// fallback's whole job is to make the harness keep working, so a fallback that
// worked and said nothing is indistinguishable from a machine that was fine.
// That is `M-7ZT-NOALERT`.
func TestF6FallbackQueuesSEV2ThroughTheProcessSink(t *testing.T) {
	f6NoDefaultTransport(t)
	f := newF6Fixture(t)
	anom, nt := f6Compose(t, f)
	ctx := context.Background()

	ex, err := exchangeOver(ctx, f6Config(t), f6Signer(t), nt)
	if err != nil {
		t.Fatalf("seeding the cache: %v", err)
	}
	nt.rest.CloseIdleConnections()
	f.wedge()
	f.advance(netx.FloorTTL + time.Second)
	if _, err := rest.NewClient(ex.Doer).Balance(ctx); err != nil {
		t.Fatalf("the fallback did not connect: %v", err)
	}

	// (1) The COMPOSED production callback reached the process sink.
	raised := f6Drain(anom)
	if len(raised) != 1 {
		t.Fatalf("the fallback raised %d anomalies on the process sink, want 1: "+
			"%+v.\n\nThe sink is created in `main.go` before "+
			"`productionExchange` and passed to the DNS reporter and to "+
			"`newRig`. A fallback that raises nowhere is the 2.5-hour wedge "+
			"going unnoticed for exactly as long as it did before this existed",
			len(raised), raised)
	}
	a := raised[0]
	if a.Class != "DNS_FALLBACK" || a.Sev != risk.SEV2 {
		t.Fatalf("the fallback raised %s/%v, want DNS_FALLBACK/SEV2", a.Class, a.Sev)
	}
	if a.Ticker != "" {
		t.Fatalf("the fallback is scoped to market %q; a wedged resolver is a "+
			"property of the HOST, and attributing it to whichever market "+
			"happened to be quoting makes it read as market-specific",
			a.Ticker)
	}
	if !strings.Contains(a.Text, f6RESTHost) {
		t.Fatalf("the anomaly does not name the host: %q", a.Text)
	}
	if !strings.Contains(a.Text, f6RESTAddr) {
		t.Fatalf("the anomaly does not name the cached destination it dialled: "+
			"%q. 'Is this the 2.5-hour wedge or has my network gone away' is "+
			"answered by the address and the lookup error and by nothing else",
			a.Text)
	}

	// (2) And that sink is the EXISTING durable path, not a second one. The
	// process's own submitter drains it into the §13.1 anomaly journal.
	h := newSeamHarness(t, seamOptions{})
	dnsFallbackReporter(h.anom)(netx.Fallback{
		Host:      f6RESTHost,
		Address:   net.JoinHostPort(f6RESTAddr, "443"),
		LookupErr: fmt.Errorf("scripted resolver is wedged"),
		Connected: true,
	})
	newShutdown(h.rig).handleAnomalies()
	h.await("the DNS fallback to reach the durable anomaly journal", func() bool {
		for _, class := range h.anomalyClasses() {
			if class == "DNS_FALLBACK" {
				return true
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// 15. lip-gp8 -- H-HALT-5, the trading P&L loss floor
// ---------------------------------------------------------------------------

// seamMakerFill is one owned MAKER fill row for the P&L fixture.
//
// `is_taker` false and `fee_cost` "0.0000" go together and neither is
// incidental. S2 makes a NON-ZERO FEE an independent witness of a taker fill
// whatever the flag claims, and H-ORD-8's taker stop is ranked above `pnl_kill`;
// a fixture that paid a fee would latch `portfolio_read` before the loss floor
// was ever consulted, and every test below would pass while proving nothing.
func seamMakerFill(tradeID, orderID, side, yesFP, noFP, count string,
	tsMs int64) map[string]any {

	return map[string]any{
		"fill_id":           tradeID + "-FILL",
		"trade_id":          tradeID,
		"order_id":          orderID,
		"ticker":            seamTicker,
		"side":              side,
		"yes_price_dollars": yesFP,
		"no_price_dollars":  noFP,
		"count":             count,
		"is_taker":          false,
		"fee_cost":          "0.0000",
		"ts":                fmt.Sprintf("%d", tsMs),
	}
}

// seamOwnedOrderOn makes one exchange order id OURS on a chosen side, so a
// fixture can hand the fills walk a fill on either leg.
//
// `installOwnedOrder` is fixed to the YES side and to one contract, which is all
// its own callers need. These tests need both legs: a market bought on YES and
// closed on NO is two orders on a real exchange, and `risk.Portfolio` says so --
// a NO fill arriving on a YES order state raises ORDER_SIDE_CONFLICT and is not
// applied, which leaves `q_local` holding a position the exchange has already
// closed and stops the harness on POSITION_DRIFT before the loss floor is ever
// reached.
func (h *seamHarness) seamOwnedOrderOn(orderID string, seq uint64,
	side quote.Side, cents int, count num.Qty) {

	h.t.Helper()
	coid, err := rest.Coid("SEAMPNL0", 0, side, seq)
	if err != nil {
		h.t.Fatalf("building a coid: %v", err)
	}
	o, err := rest.NewCreateOrder(seamTicker, side, cents, count, count, coid)
	if err != nil {
		h.t.Fatalf("building the reserved order: %v", err)
	}
	if _, err := h.rig.store.ReserveOrder(h.rig.run, o, quote.RoleAdding,
		h.clk.wallMs()); err != nil {
		h.t.Fatalf("reserving %s: %v", coid, err)
	}
	if _, err := h.rig.store.BindOrder(coid, orderID,
		h.clk.wallMs()); err != nil {
		h.t.Fatalf("binding %s -> %s: %v", coid, orderID, err)
	}
	h.await("the seeded ownership binding to commit", func() bool {
		got, ok := h.rig.store.Ownership().Bound(orderID)
		return ok && got == coid
	})
}

// seamPnLLeg is one fill of a P&L fixture, with the owned order it landed on.
//
// `cents` is the price the OWNED ORDER was placed at and has nothing to do with
// the fill price: the ownership ledger is what H-ORD-9 classifies by, and it
// carries no price cross-check. It is here only because `rest.NewCreateOrder`
// requires one.
type seamPnLLeg struct {
	trade string
	side  quote.Side
	// yesFP and noFP are the wire prices, which must sum to $1.0000 --
	// `rest.readPrice` enforces that exactly (H-CO-1), and it is what makes the
	// YES-equivalent conversion of a NO fill exact rather than approximate.
	yesFP string
	noFP  string
	count num.Qty
	cents int
}

// seedPnLFills registers one owned order PER LEG and hands the fills to the
// fixture's fills walk, stamped in the order given.
//
// One order per leg, rather than one order for the market. A market entered on
// YES and closed on NO is two orders on a real exchange, and `risk.Portfolio`
// agrees: a NO fill arriving on a YES order state raises ORDER_SIDE_CONFLICT and
// is NOT applied, which leaves `q_local` holding a position the exchange has
// already closed and stops the harness on POSITION_DRIFT -- with the loss floor
// never consulted and the test green.
func (h *seamHarness) seedPnLFills(legs ...seamPnLLeg) {
	h.t.Helper()
	base := h.clk.wallMs()
	for i, leg := range legs {
		id := fmt.Sprintf("SEAM-PNL-ORD-%d", i+1)
		h.seamOwnedOrderOn(id, uint64(4001+i), leg.side, leg.cents, leg.count)
		side := "yes"
		if leg.side == quote.SideNo {
			side = "no"
		}
		h.ex.addFill(seamMakerFill(leg.trade, id, side, leg.yesFP, leg.noFP,
			leg.count.Wire(), base+int64(i)+1))
	}
}

// seamFloorLegs is the two fills that lose EXACTLY $15.00 and end FLAT.
//
//	buy  30.00 YES at $0.9000   cash -= 3000 * 9000 =	-$27.00   q = +30.00
//	sell 30.00 YES at $0.4000   cash += 3000 * 4000 =	+$12.00   q =   0.00
//
// The sell is expressed the way the exchange expresses it: a NO buy at $0.6000,
// whose YES-equivalent price is $1.0000 - $0.6000 (H-CO-1). Both legs land on
// ONE poll, so the intermediate +30.00 position is never an authoritative
// reading and `inv_kill` -- which outranks this rule -- is never breached by the
// fixture that exists to test this one.
//
// Ending FLAT is what makes the figure independent of the book: a flat market
// has no unrealised component, so the total is realised cash that no mark can
// move, and the boundary is decided by the fills alone.
func seamFloorLegs() []seamPnLLeg {
	return []seamPnLLeg{
		{trade: "SEAM-PNL-BUY", side: quote.SideYes, yesFP: "0.9000",
			noFP: "0.1000", count: num.QtyFromFloat(30), cents: 90},
		{trade: "SEAM-PNL-SELL", side: quote.SideNo, yesFP: "0.4000",
			noFP: "0.6000", count: num.QtyFromFloat(30), cents: 60},
	}
}

// seamMicroLegs is a round trip of ONE QUANTUM that moves the total by exactly
// one microdollar, in whichever direction is asked for.
//
// 0.01 contracts is `num.Qty` 1 and a price4 is 1e-4 dollars, so their product
// is exactly one of the 1e-6 dollars `num.Money` counts. That is the finest step
// the exchange's own quanta can express, and it is what lets the boundary be
// tested at the resolution the comparison is actually performed at rather than
// at a cent.
//
// It nets to zero quantity, so it leaves the market flat and the fixture's
// independence from the mark intact.
func seamMicroLegs(loss bool) []seamPnLLeg {
	closeYes, closeNo := "0.5001", "0.4999"
	if loss {
		closeYes, closeNo = "0.4999", "0.5001"
	}
	return []seamPnLLeg{
		{trade: "SEAM-PNL-MICRO-OPEN", side: quote.SideYes, yesFP: "0.5000",
			noFP: "0.5000", count: 1, cents: 50},
		{trade: "SEAM-PNL-MICRO-CLOSE", side: quote.SideNo, yesFP: closeYes,
			noFP: closeNo, count: 1, cents: 50},
	}
}

// TestPnLKillIsInclusiveAtTheExactNegativeFloor is the boundary, from both
// sides and at the quantum the comparison is made at.
//
// §12's row reads "P&L <= pnl_kill", so exactly -$15.00 FIRES. That is the
// opposite sense to `inv_kill`'s strict `>` and it is not an inconsistency:
// `inv_kill` has a market-scoped neighbour immediately below it and `Validate`
// orders them strictly, so the band up to and including it belongs to the other
// row. `pnl_kill` has no neighbour -- `Validate` refuses a non-negative one
// because it would fire immediately -- so nothing else owns the figure itself.
//
// A strict `<` here is one microdollar of drawdown that the backstop lets
// through, which is not a rounding difference but a rule that does not fire on
// the value the spec names.
func TestPnLKillIsInclusiveAtTheExactNegativeFloor(t *testing.T) {
	run := func(t *testing.T, extra []seamPnLLeg) string {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.start()
		h.awaitActionable()

		h.seedPnLFills(append(seamFloorLegs(), extra...)...)
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(4)
		return h.latchTrigger()
	}

	t.Run("exactly at the floor fires", func(t *testing.T) {
		if got := run(t, nil); got != "pnl_kill" {
			t.Fatalf("a trading P&L of exactly -$15.00 against pnl_kill -$15.00 "+
				"latched %q, want pnl_kill.\n\n"+
				"§12 reads \"P&L <= pnl_kill\". The floor itself is a breach, and "+
				"a harness that treats it as safe is one whose loss backstop does "+
				"not fire at the number the operator configured", got)
		}
	})

	t.Run("one microdollar above the floor does not fire", func(t *testing.T) {
		if got := run(t, seamMicroLegs(false)); got != "" {
			t.Fatalf("a trading P&L of -$14.999999 latched %q against pnl_kill "+
				"-$15.00.\n\n"+
				"One microdollar above the floor is not a breach. A halt here is "+
				"the harness stopping on a loss the operator permitted, and it is "+
				"the direction that costs uptime rather than money -- but it also "+
				"means the comparison is not the one §12 specifies", got)
		}
	})

	t.Run("one microdollar below the floor fires", func(t *testing.T) {
		if got := run(t, seamMicroLegs(true)); got != "pnl_kill" {
			t.Fatalf("a trading P&L of -$15.000001 latched %q, want pnl_kill",
				got)
		}
	})
}

// seamHeldLossLegs is a NON-FLAT breaching fixture: 15.00 contracts still held,
// against a realised loss deep enough that the mark cannot rescue it.
//
//	buy  30.00 YES at $0.9000   cash = -$27.00   q = +30.00
//	sell 15.00 YES at $0.3000   cash = -$22.50   q = +15.00
//
// The opening book marks YES at (40 + (100-55)) * 50 = 4250, so the inventory is
// worth 1500 * 4250 = $6.375 and the total is -$16.125 -- past the floor with
// $1.125 of room, so the test is not measuring a rounding step.
//
// 15.00 contracts is deliberate at both ends: past `inv_hard` 7.00 so the market
// is REDUCING and a reducer is live to be observed, and under `inv_kill` 18.00
// so the halt that fires is this one rather than the rule ranked above it.
func seamHeldLossLegs() []seamPnLLeg {
	return []seamPnLLeg{
		{trade: "SEAM-PNL-HOLD-BUY", side: quote.SideYes, yesFP: "0.9000",
			noFP: "0.1000", count: num.QtyFromFloat(30), cents: 90},
		{trade: "SEAM-PNL-HOLD-SELL", side: quote.SideNo, yesFP: "0.3000",
			noFP: "0.7000", count: num.QtyFromFloat(15), cents: 70},
	}
}

// TestPnLKillLatchesByNameAndKeepsTheReducerLive is the wiring, end to end, and
// the §12 row's OTHER four columns.
//
// The latch is the one artefact that survives the process, and §10.4 has the
// operator read its cause to learn which row of the halt table fired.
// `portfolio_read` is the label five distinct causes already share; a named §12
// row arriving under it is a halt whose reason cannot be recovered at 3am.
//
// The rest of the row is I1, and it is the whole difference between a loss
// backstop and a kill switch: adding stops, REDUCING does not, the monitor keeps
// publishing, and the process stays alive. A harness that exits here abandons
// the inventory that caused the loss.
func TestPnLKillLatchesByNameAndKeepsTheReducerLive(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Rung: "pilot"})
	h.start()
	h.awaitActionable()

	h.seedPnLFills(seamHeldLossLegs()...)
	h.ex.setPosition(seamTicker, "15.00")
	h.clk.Advance(h.cfg.Params.PositionPoll)

	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "pnl_kill" {
		t.Fatalf("a trading P&L of -$16.125 against pnl_kill -$15.00 latched "+
			"%q, want pnl_kill", got)
	}
	h.await("the SEV1 PNL_KILL anomaly to reach the store", func() bool {
		return seamContains(h.anomalyClasses(), "PNL_KILL")
	})
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")

	// I1's four columns, read after the halt is durable.
	before := h.snapSeq()
	h.awaitTicks(4)
	if got := h.snapSeq(); got <= before {
		t.Fatalf("the snapshot sequence stopped at %d after the halt.\n\n"+
			"I2 makes the monitor unstoppable, and H-TOP-5's whole stall "+
			"detector is that number advancing. A harness that stops publishing "+
			"while continuing to hold inventory leaves the monitor re-reporting "+
			"a snapshot that was true once", got)
	}
	s := h.snapshot()
	if s.Global != quote.WindingDown {
		t.Fatalf("the global state is %s after the loss floor fired, want "+
			"WINDING_DOWN", s.Global)
	}
	m, ok := h.market()
	if !ok || m.State != quote.Reducing {
		t.Fatalf("the market is %v (present=%v) after the loss floor fired, "+
			"want REDUCING.\n\n"+
			"§12 stops ADDING and keeps the reducing quote live. 15.00 contracts "+
			"are still held; a market that went IDLE here would be one that "+
			"stopped trying to get out of the position that caused the halt",
			m.State, ok)
	}
}

// TestPnLKillRanksBelowPortfolioReadAndInvKillButAboveCanary is the
// first-writer-wins ordering, and it is the only property of the placement that
// is observable at all.
//
// `commitStop` discards a second cause arriving before the first is durable, so
// the position of the P&L block in `applyRead` decides which cause the latch
// carries when several are true of the same poll -- and several being true at
// once is the ordinary case, not the exotic one. Each subtest drives TWO real
// causes and asserts which one the operator finds.
func TestPnLKillRanksBelowPortfolioReadAndInvKillButAboveCanary(t *testing.T) {
	t.Run("inv_kill outranks it", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.start()
		h.awaitActionable()

		// 18.01 contracts held, bought at $0.9000 and partly closed at $0.3000:
		// cash -$23.403, inventory 1801 * 4250 = $7.654, total -$15.749. Both
		// rules are breached by the same poll.
		h.seedPnLFills(
			seamPnLLeg{trade: "SEAM-RANK-BUY", side: quote.SideYes,
				yesFP: "0.9000", noFP: "0.1000",
				count: num.QtyFromFloat(30), cents: 90},
			seamPnLLeg{trade: "SEAM-RANK-SELL", side: quote.SideNo,
				yesFP: "0.3000", noFP: "0.7000",
				count: num.QtyFromFloat(11.99), cents: 70},
		)
		h.ex.setPosition(seamTicker, "18.01")
		h.clk.Advance(h.cfg.Params.PositionPoll)

		h.await("the durable §12 cause to reach the latch", func() bool {
			return h.latchTrigger() != ""
		})
		if got := h.latchTrigger(); got != "inv_kill" {
			t.Fatalf("a poll breaching BOTH inv_kill and pnl_kill latched %q, "+
				"want inv_kill.\n\n"+
				"\"We hold more than we said we would\" is a fact about the "+
				"position; \"we have lost more than we said we would\" is a "+
				"calculation over it, and one that depends on a mark that may "+
				"itself be the thing that is wrong. The operator woken at 3am is "+
				"better served by the measurement than by the arithmetic", got)
		}
	})

	t.Run("portfolio_read outranks it", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.start()
		h.awaitActionable()

		// The floor fixture, with the closing leg taken as a TAKER. H-ORD-8 is
		// the cheapest detector for the most expensive bug in the system, and
		// the $0.01 fee takes the total to -$15.01 so the loss floor is
		// genuinely breached on the same poll rather than merely nearly.
		legs := seamFloorLegs()
		h.seamOwnedOrderOn("SEAM-TAKE-Y", 4101, quote.SideYes, 90,
			num.QtyFromFloat(30))
		h.seamOwnedOrderOn("SEAM-TAKE-N", 4102, quote.SideNo, 60,
			num.QtyFromFloat(30))
		base := h.clk.wallMs()
		h.ex.addFill(seamMakerFill(legs[0].trade, "SEAM-TAKE-Y", "yes",
			"0.9000", "0.1000", "30.00", base+1))
		taker := seamMakerFill(legs[1].trade, "SEAM-TAKE-N", "no",
			"0.4000", "0.6000", "30.00", base+2)
		taker["is_taker"] = true
		taker["fee_cost"] = "0.0100"
		h.ex.addFill(taker)

		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.await("the durable §12 cause to reach the latch", func() bool {
			return h.latchTrigger() != ""
		})
		if got := h.latchTrigger(); got != "portfolio_read" {
			t.Fatalf("a poll carrying a TAKER fill and breaching pnl_kill "+
				"latched %q, want portfolio_read.\n\n"+
				"A taker fill means the model of our own orders is wrong, and "+
				"every cause funnelled through `eff.Stop` outranks a calculated "+
				"loss for that reason", got)
		}
	})

	t.Run("the canary bound does not outrank it", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "canary"})
		h.start()
		h.awaitActionable()

		// At the canary rung the FIRST live owned fill stops the harness on its
		// own (§7.9). The same poll breaches the floor, and `pnl_kill` is a row
		// of the §12 halt table while "the canary traded" is a rung policy.
		h.seedPnLFills(seamFloorLegs()...)
		h.clk.Advance(h.cfg.Params.PositionPoll)

		h.await("the durable §12 cause to reach the latch", func() bool {
			return h.latchTrigger() != ""
		})
		if got := h.latchTrigger(); got != "pnl_kill" {
			t.Fatalf("a poll tripping BOTH the canary first-fill bound and "+
				"pnl_kill latched %q, want pnl_kill.\n\n"+
				"Every §12 cause outranks a rung policy. §10.4's operator reading "+
				"`canary_owned_fill` would investigate a planned, expected event "+
				"and never learn the account was $15 down", got)
		}
	})
}

// TestPnLRefusesASteppedPositionWithoutAMatchingOwnedFill is H-ORD-5a's
// principle applied to a loss floor: ignorance is reported, not resolved.
//
// The account holds 18.00 contracts and the ledger has fills for 15.00. The
// missing 3.00 were paid for by something this process never saw -- a manual
// trade, a settlement, a fill that never arrived -- and there is no honest basis
// to value them at. Valuing them AT THE MARK would assume they were acquired at
// today's price, which is the assumption most likely to hide a loss: it prices
// the unexplained position at exactly zero P&L.
//
// The step is 3.00, inside `pos_drift_hard` 5.00, so no drift stop competes; and
// 18.00 is exactly `inv_kill`, which is a strict `>` and therefore not a breach.
// The loss is real and past the floor either way, so a harness that evaluated
// anyway WOULD fire -- which is what makes an empty latch here evidence.
func TestPnLRefusesASteppedPositionWithoutAMatchingOwnedFill(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Rung: "pilot"})
	h.start()
	h.awaitActionable()

	h.seedPnLFills(seamHeldLossLegs()...)
	h.ex.setPosition(seamTicker, "18.00")
	for i := 0; i < 3; i++ {
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}

	if got := h.latchTrigger(); got != "" {
		t.Fatalf("the loss floor latched %q from a ledger holding 15.00 "+
			"contracts against an authoritative 18.00.\n\n"+
			"The two disagree, so there is no basis for 3.00 of the position and "+
			"the total is not a figure anyone can stand behind. H-HALT-5 makes "+
			"an unevaluable trigger NOT FIRED -- never fired-or-safe", got)
	}
	h.await("the SEV2 PNL_BASIS_UNAVAILABLE anomaly to reach the store",
		func() bool {
			return seamContains(h.anomalyClasses(), "PNL_BASIS_UNAVAILABLE")
		})
	if seamContains(h.anomalyClasses(), "PNL_KILL") {
		t.Fatalf("PNL_KILL was raised for a position the ledger has no fills " +
			"for")
	}
}

// TestPnLRestartIncludesEveryOwnedFillRegardlessOfRunAndBackfilledFlag is the
// history half, and it is two claims about the same table.
//
// The first is that INHERITED history counts. The fills are on the account
// before this process starts, so §7.5 adopts them and `recordBackfilled` writes
// them with `backfilled = true`; a ledger seeded only from what this incarnation
// watched happen would open at zero and never fire, while the account sat $15
// down. A position adopted at startup was paid for by an earlier run.
//
// The second is that the DURABLE table is the source. A fresh owner over the
// same store, offered no adoption at all, recovers the identical figure from
// `our_fill` alone -- which is what the next restart does, and the only reason a
// restart does not reset the loss floor's zero.
func TestPnLRestartIncludesEveryOwnedFillRegardlessOfRunAndBackfilledFlag(
	t *testing.T) {

	h := newSeamHarness(t, seamOptions{Rung: "pilot"})
	// Seeded BEFORE `start`, so these are history the process INHERITS rather
	// than trading it watches.
	h.seedPnLFills(seamFloorLegs()...)
	h.start()

	h.await("the inherited history to reach the loss floor", func() bool {
		return h.latchTrigger() == "pnl_kill"
	})

	// The durable half. `stopServe` first: the owner goroutine is the single
	// writer for everything a second owner would touch, and this one only reads
	// the store, but the discipline is the discipline.
	h.stopServe()
	o := h.ownerFor()
	o.seedPnL(nil)
	if got, want := o.pnl.Realised(seamTicker), num.MoneyFromDollars(-15); got != want {
		t.Fatalf("a fresh owner seeded from our_fill alone recovered realised "+
			"P&L %s, want %s.\n\n"+
			"`Reader.Fills` takes no argument to filter by, on purpose: every "+
			"row, every run, both provenances. A restart that read only its own "+
			"run's rows would hold inventory it has no cost for, and the "+
			"authoritative-quantity check would then refuse to evaluate the "+
			"trigger for the life of the process", got, want)
	}
	if got := o.pnl.Qty(seamTicker); got != 0 {
		t.Fatalf("the recovered ledger holds %s, want flat: the two durable "+
			"rows net to zero", got.Wire())
	}
}

// TestStaleAndAbsentPnLMarksAreSEV2AndCannotFireTheKill is H-HALT-5's refusal,
// in both of the two shapes it distinguishes.
//
// "If no mark of acceptable age exists the trigger cannot be evaluated: emit
// SEV2 and treat it as not-fired." Not fired-or-safe, and not realised-only: a
// harness that valued the held 15.00 contracts at their cost, or at zero, or at
// the last price it happened to remember would produce a number that looks like
// a P&L and is not one.
//
// The loss is past the floor in both subtests, so an empty latch is evidence
// that the refusal happened rather than evidence that nothing was wrong.
func TestStaleAndAbsentPnLMarksAreSEV2AndCannotFireTheKill(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.start()
		h.awaitActionable()

		// Six polls with no book frame takes the mark to `pnl_mark_max_age_s`
		// and one more takes it past. The fills are seeded only at the end, so
		// the first evaluation that has anything to evaluate is the stale one.
		for i := 0; i < 6; i++ {
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.awaitTicks(2)
		}
		h.seedPnLFills(seamHeldLossLegs()...)
		h.ex.setPosition(seamTicker, "15.00")
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(4)

		if got := h.latchTrigger(); got != "" {
			t.Fatalf("the loss floor latched %q from a mark 35s old against "+
				"pnl_mark_max_age_s 30s.\n\n"+
				"A stale mark is not a cheap approximation of a fresh one. The "+
				"position it is valuing may have moved the whole way to the "+
				"floor or the whole way back, and H-HALT-5 refuses to guess "+
				"which", got)
		}
		h.await("the SEV2 PNL_MARK_UNAVAILABLE anomaly to reach the store",
			func() bool {
				return seamContains(h.anomalyClasses(), "PNL_MARK_UNAVAILABLE")
			})
		if seamContains(h.anomalyClasses(), "PNL_KILL") {
			t.Fatalf("PNL_KILL was raised against a stale mark")
		}
	})

	t.Run("absent", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		o := h.ownerFor()
		h.connectGate()
		// A book with levels in it, installed straight into the decoder -- so
		// `core` holds a full book and the GATE has never accepted a frame for
		// it. That is the state a reconnect leaves: levels carried across the
		// gap, and no snapshot on this generation entitling anyone to believe
		// them (H-FAIL-5).
		h.installBook([][]string{{"0.4000", "20.00"}},
			[][]string{{"0.5500", "20.00"}})
		h.seedOwnerLedger(o, seamHeldLossLegs()...)
		o.pnlQExch = map[string]num.Qty{seamTicker: num.QtyFromFloat(15)}
		o.pnlQKnown = true

		o.evaluatePnL()

		if got := h.latchTrigger(); got != "" {
			t.Fatalf("the loss floor latched %q from a book the gate never "+
				"accepted a frame for.\n\n"+
				"An absent mark and a stale one are the same not-fired answer "+
				"and completely different things to be woken for, and neither is "+
				"a price", got)
		}
		var raised []string
		for _, a := range h.takeRaised() {
			raised = append(raised, a.Class)
		}
		if !seamContains(raised, "PNL_MARK_UNAVAILABLE") {
			t.Fatalf("no PNL_MARK_UNAVAILABLE was raised for a market with no "+
				"accepted snapshot; raised %v", raised)
		}
	})
}

// seedOwnerLedger drives fills straight into one owner's P&L ledger, for the
// owner-level tests whose subject is the MARK rather than the wiring.
func (h *seamHarness) seedOwnerLedger(o *owner, legs ...seamPnLLeg) {
	h.t.Helper()
	for i, leg := range legs {
		price4 := seamPrice4(h.t, leg.yesFP)
		if leg.side == quote.SideNo {
			price4 = seamPrice4(h.t, leg.noFP)
		}
		o.applyPnLFill(risk.FillEvent{
			TradeID: leg.trade, OrderID: "SEAM-OWNER-LEDGER",
			Ticker: seamTicker, Side: leg.side, Price4: price4,
			Count: leg.count, ExchangeTsMs: int64(i + 1),
		})
	}
}

func seamPrice4(t *testing.T, fp string) int64 {
	t.Helper()
	p, err := rest.ParsePrice4(fp)
	if err != nil {
		t.Fatalf("ParsePrice4(%q): %v", fp, err)
	}
	return int64(p)
}

// setBalance changes what `GET /portfolio/balance` reports. It exists so a test
// can move the account's cash AFTER startup, which is the only shape the reward
// regression guard can currently take.
func (f *seamExchange) setBalance(cents int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceCents = cents
}

// acceptBook delivers one snapshot through the GATE, for the owner-level tests.
//
// It goes through `applyEvent` rather than `rig.book.Handle` because that is the
// distinction the mark clock is built on: `installBook` puts levels in `core`
// without the gate ever seeing a frame, which is exactly the state a reconnect
// leaves behind, and only an ACCEPTED frame may stamp a mark.
func (h *seamHarness) acceptBook(o *owner, tsMs int64, yes, no [][]string) {
	h.t.Helper()
	frame, err := h.ws.frame("orderbook_snapshot", map[string]any{
		"market_ticker":  seamTicker,
		"ts_ms":          tsMs,
		"yes_dollars_fp": yes,
		"no_dollars_fp":  no,
	})
	if err != nil {
		h.t.Fatalf("building a snapshot frame: %v", err)
	}
	tokens := make(chan wsx.ReconcileToken, 1)
	o.applyEvent(wsx.Event{Kind: wsx.EventFrame, Frame: frame,
		At: h.clk.Now()}, tokens)
}

// seamAgedLossLegs is a breaching fixture holding 5.00 contracts.
//
//	buy  30.00 YES at $0.9000   cash = -$27.00   q = +30.00
//	sell 25.00 YES at $0.3000   cash = -$19.50   q =  +5.00
//
// Marked at 4250 the inventory is worth $2.125 and the total is -$17.375. The
// position is under `inv_hard` 7.00 on purpose: the market therefore stays out
// of REDUCING, which is what makes "the two 60-second clocks are still fresh" an
// assertion the test can actually make.
func seamAgedLossLegs() []seamPnLLeg {
	return []seamPnLLeg{
		{trade: "SEAM-PNL-AGE-BUY", side: quote.SideYes, yesFP: "0.9000",
			noFP: "0.1000", count: num.QtyFromFloat(30), cents: 90},
		{trade: "SEAM-PNL-AGE-SELL", side: quote.SideNo, yesFP: "0.3000",
			noFP: "0.7000", count: num.QtyFromFloat(25), cents: 70},
	}
}

// TestPnLMarkExpiresAtThirtySecondsBeforeBothSixtySecondClocks is the reason the
// mark needed a clock of its own.
//
// This file already keeps two 60-second clocks. `quiet_s` asks whether the
// market is still talking; `truth_max_age_s` asks whether the PORTFOLIO reads
// are current. Neither is a statement about the freshness of the PRICE, and at
// 60 seconds either would authorise valuing inventory against a mark H-HALT-5
// declared unusable thirty seconds earlier.
//
// The second subtest is the whole point: at 35 seconds the mark is gone while
// both 60-second clocks are demonstrably still fresh -- the market is
// actionable and it is not reducing -- so a comparison against either of them
// would have fired the kill on a stale price.
func TestPnLMarkExpiresAtThirtySecondsBeforeBothSixtySecondClocks(t *testing.T) {
	seed := func(h *seamHarness) {
		h.t.Helper()
		h.seedPnLFills(seamAgedLossLegs()...)
		h.ex.setPosition(seamTicker, "5.00")
	}

	t.Run("exactly pnl_mark_max_age_s is inside the bound", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.start()
		h.awaitActionable()

		// Five polls takes the opening snapshot's mark to 25s; the fills are
		// seeded there so that the first evaluation with anything to evaluate is
		// the one at exactly `pnl_mark_max_age_s`.
		for i := 0; i < 5; i++ {
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.awaitTicks(2)
		}
		seed(h)
		h.clk.Advance(h.cfg.Params.PositionPoll)

		h.await("the durable §12 cause to reach the latch", func() bool {
			return h.latchTrigger() != ""
		})
		if got := h.latchTrigger(); got != "pnl_kill" {
			t.Fatalf("a mark of exactly pnl_mark_max_age_s %v latched %q, want "+
				"pnl_kill.\n\n"+
				"§16 states the parameter as a MAXIMUM age and H-HALT-5 as "+
				"\"age <= 30s\", so the boundary belongs to the usable side. A "+
				"harness that refused here would go unevaluable once a second on "+
				"a market whose book updates at exactly the bound",
				h.cfg.Params.PnLMarkMaxAge, got)
		}
	})

	t.Run("one poll past the bound is stale", func(t *testing.T) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.start()
		h.awaitActionable()

		for i := 0; i < 6; i++ {
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.awaitTicks(2)
		}
		seed(h)
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(4)

		if got := h.latchTrigger(); got != "" {
			t.Fatalf("a mark 35s old latched %q against pnl_mark_max_age_s %v",
				got, h.cfg.Params.PnLMarkMaxAge)
		}
		h.await("the SEV2 PNL_MARK_UNAVAILABLE anomaly to reach the store",
			func() bool {
				return seamContains(h.anomalyClasses(), "PNL_MARK_UNAVAILABLE")
			})

		// Both 60-second clocks, still fresh at the moment the 30-second one has
		// expired. This is the assertion that separates `pnl_mark_max_age_s`
		// from the two thresholds it would otherwise be indistinguishable from.
		m, ok := h.market()
		if !ok || !m.BookActionable {
			t.Fatalf("the market is not actionable (present=%v) at 35s, so "+
				"truth_max_age_s %v cannot be shown to be fresh and the subject "+
				"of this test is not observable", ok, h.cfg.Params.TruthMaxAge)
		}
		if m.State == quote.Reducing {
			t.Fatalf("the market is REDUCING at 35s holding 5.00 contracts "+
				"against inv_hard %s, which means quiet_s %v has fired and the "+
				"two clocks cannot be told apart here",
				h.cfg.Params.InvHard.Wire(), h.cfg.Params.Quiet)
		}
	})
}

// TestPnLMarkRequiresAnAcceptedCurrentGenerationSnapshot is what the mark clock
// is FOR, and it is the reason it is not `lastFrame`.
//
// `lastFrame` is seeded at CONNECT and survives frames `core` refused, because
// silence is what F5 measures. Neither property is tolerable in a price: a
// connection that has said nothing has told us nothing about value, and a frame
// core declined to believe is not evidence of one. Both would produce a mark
// that looks fresh over a book nobody has confirmed.
//
// Three cases, and the third is not decoration. An assertion that nothing fired
// is satisfied by a detector that cannot fire at all.
func TestPnLMarkRequiresAnAcceptedCurrentGenerationSnapshot(t *testing.T) {
	setup := func(t *testing.T) (*seamHarness, *owner) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		o := h.ownerFor()
		h.seedOwnerLedger(o, seamHeldLossLegs()...)
		o.pnlQExch = map[string]num.Qty{seamTicker: num.QtyFromFloat(15)}
		o.pnlQKnown = true
		return h, o
	}
	yes := [][]string{{"0.4000", "20.00"}}
	no := [][]string{{"0.5500", "20.00"}}

	t.Run("a connection alone establishes no mark", func(t *testing.T) {
		h, o := setup(t)
		h.connectGate()
		// Levels in `core`, and not one frame the gate has accepted.
		h.installBook(yes, no)

		o.evaluatePnL()

		if got := h.latchTrigger(); got != "" {
			t.Fatalf("the loss floor latched %q on a connection that has not "+
				"delivered an accepted snapshot.\n\n"+
				"A mark stamped at connect time is a price nobody has quoted. "+
				"H-FAIL-5 is explicit that a book carried across a gap "+
				"authorises nothing until a fresh snapshot replaces it", got)
		}
	})

	t.Run("a prior generation's mark does not survive", func(t *testing.T) {
		h, o := setup(t)
		h.connectGate()
		h.acceptBook(o, 1, yes, no)

		// The socket drops and comes back. `core` is reset by the disconnect, so
		// the levels are re-installed directly afterwards -- leaving a FULL book
		// whose only snapshot belongs to the previous generation, which is
		// exactly the state H-FAIL-5 describes.
		tokens := make(chan wsx.ReconcileToken, 1)
		o.applyEvent(wsx.Event{Kind: wsx.EventDisconnected, At: h.clk.Now(),
			Clean: true}, tokens)
		o.applyEvent(wsx.Event{Kind: wsx.EventConnected, At: h.clk.Now()},
			tokens)
		h.installBook(yes, no)

		o.evaluatePnL()

		if got := h.latchTrigger(); got != "" {
			t.Fatalf("the loss floor latched %q from a mark established on the "+
				"PREVIOUS connection.\n\n"+
				"A generation change invalidates every reading taken under the "+
				"old one. A mark that survived it is a price from before an "+
				"outage of unknown length, presented as current", got)
		}
	})

	t.Run("an accepted current-generation snapshot marks", func(t *testing.T) {
		h, o := setup(t)
		h.connectGate()
		h.acceptBook(o, 1, yes, no)

		o.evaluatePnL()

		if got := h.latchTrigger(); got != "pnl_kill" {
			t.Fatalf("a fresh accepted snapshot on the current generation "+
				"latched %q, want pnl_kill.\n\n"+
				"Without this case the two above are satisfied by a rule that "+
				"can never fire, which is the failure mode this whole bead "+
				"exists because of", got)
		}
	})
}

// TestPnLMarkUsesExternalBestAfterSubtractingAllOurSize is H-Q-10 arriving at
// the loss floor instead of at the quote.
//
// Valuing inventory against a touch we are ourselves posting is self-reference:
// our own bid holds the mark up while the position it is valuing gets worse, and
// `pnl_kill` fires late or not at all. `Book.Mid()` is exactly that unsubtracted
// reading, which is why the helper takes levels rather than a book.
//
// The two subtests are the SAME BOOK and differ only in whether the 60c level
// belongs to us. That is the whole experiment: 60c ours gives an external touch
// of 40c and a mark of 4250, which breaches; 60c somebody else's gives 5250,
// which does not. A harness that skipped the subtraction cannot tell them apart.
func TestPnLMarkUsesExternalBestAfterSubtractingAllOurSize(t *testing.T) {
	yes := [][]string{{"0.6000", "5.00"}, {"0.4000", "20.00"}}
	no := [][]string{{"0.5500", "20.00"}}

	setup := func(t *testing.T) (*seamHarness, *owner) {
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		o := h.ownerFor()
		h.connectGate()
		h.acceptBook(o, 1, yes, no)
		h.seedOwnerLedger(o, seamHeldLossLegs()...)
		o.pnlQExch = map[string]num.Qty{seamTicker: num.QtyFromFloat(15)}
		o.pnlQKnown = true
		return h, o
	}

	t.Run("the top of book is ours", func(t *testing.T) {
		h, o := setup(t)
		h.installResting(risk.LiveOrder{
			OrderID: "SEAM-PNL-RESTING", Ticker: seamTicker,
			Side: quote.SideYes, Price4: 6000, Remaining: num.QtyFromFloat(5),
		})

		o.evaluatePnL()

		if got := h.latchTrigger(); got != "pnl_kill" {
			t.Fatalf("the loss floor did not fire (latch %q) while our own 60c "+
				"bid was the only thing holding the mark above it.\n\n"+
				"Subtracting our resting size leaves an external touch of 40c "+
				"and a mark of 4250, at which the total is -$16.125. A mark "+
				"taken from the raw book reads 5250 and -$14.625 -- inside the "+
				"floor, on the strength of our own quote", got)
		}
	})

	t.Run("the top of book is somebody else's", func(t *testing.T) {
		h, o := setup(t)

		o.evaluatePnL()

		if got := h.latchTrigger(); got != "" {
			t.Fatalf("the loss floor latched %q against a genuinely external "+
				"60c bid, at which the mark is 5250 and the total -$14.625.\n\n"+
				"This is the control for the subtraction: without it, a rule "+
				"that ignored our size entirely would pass the other half of "+
				"this test for the wrong reason", got)
		}
	})
}

// TestRewardArrivalCannotMoveTradingPnL is HR-021, kept as a regression guard.
//
// The red team found two conforming implementations disagreeing completely on
// the same position: a fills-and-cost-basis reading saw -$90 and halted, while a
// balance-delta reading saw nothing -- and, worse, "an incoming $100 LIP reward
// masks the drawdown entirely". The reward is the thing this system is measuring
// itself against. It is not a component of its P&L.
//
// `risk.TradingPnL` excludes it BY CONSTRUCTION: the only thing that can enter
// the type is an owned `FillEvent` and there is no method by which a balance
// could reach it. So this test cannot fail while that holds, which is exactly
// what makes it a guard rather than a proof -- it fails the moment someone adds
// the balance reader that would make the exclusion a choice again.
//
// The literal post-start reward-delivery path is unreachable today: the only
// runtime balance read is at startup. When `lip-o7a` adds delivery, that bead
// must strengthen this into an actual delivered-delta test.
func TestRewardArrivalCannotMoveTradingPnL(t *testing.T) {
	// The fixture is one microdollar PAST the floor, so it fires -- and firing is
	// what makes the test sensitive to the defect. HR-021's failure is MASKING:
	// a balance folded into the ledger swamps a $15 drawdown with a four-figure
	// account balance and the halt silently stops happening. A fixture that sat
	// inside the floor would go on not-firing under exactly that bug, and would
	// prove only that the harness can decline to stop.
	realised := func(t *testing.T, startCents, laterCents int64) num.Money {
		t.Helper()
		h := newSeamHarness(t, seamOptions{Rung: "pilot"})
		h.ex.setBalance(startCents)
		h.start()
		h.awaitActionable()

		h.seedPnLFills(append(seamFloorLegs(), seamMicroLegs(true)...)...)
		h.clk.Advance(h.cfg.Params.PositionPoll)

		h.await("the durable §12 cause to reach the latch", func() bool {
			return h.latchTrigger() != ""
		})
		if got := h.latchTrigger(); got != "pnl_kill" {
			t.Fatalf("an account holding %d cents and down $15.000001 latched "+
				"%q, want pnl_kill.\n\n"+
				"The balance is four figures and the drawdown is fifteen "+
				"dollars. Any reading that lets the former reach the P&L cannot "+
				"see the latter at all -- which is HR-021 exactly: \"an incoming "+
				"$100 LIP reward masks the drawdown entirely\"", startCents, got)
		}

		// The reward lands AFTER the halt. `balance_poll_s` is 60s and the
		// session's own read deadline is 60s, so it is walked there one
		// `position_poll_s` at a time rather than stepped past in one jump.
		h.ex.setBalance(laterCents)
		for i := 0; i < 13; i++ {
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.awaitTicks(2)
		}
		if got := h.latchTrigger(); got != "pnl_kill" {
			t.Fatalf("the durable cause is %q after a reward arrived, want "+
				"pnl_kill: a P&L halt is never undone by money arriving from "+
				"somewhere other than trading", got)
		}
		if s := h.snapshot(); s.Global != quote.WindingDown {
			t.Fatalf("the global state is %s after a reward arrived, want "+
				"WINDING_DOWN", s.Global)
		}

		h.stopServe()
		o := h.ownerFor()
		o.seedPnL(nil)
		return o.pnl.Realised(seamTicker)
	}

	poor := realised(t, 100_000, 100_000)
	rich := realised(t, 110_000, 120_000)

	if want := num.Money(-15_000_001); poor != want {
		t.Fatalf("trading P&L is %s, want %s: the fixture's fills alone decide "+
			"the figure", poor, want)
	}
	if poor != rich {
		t.Fatalf("trading P&L is %s on an account that started $100 richer and "+
			"was paid $100 more after startup, against %s on the poor one.\n\n"+
			"H-HALT-5 measures TRADING P&L. A reward arriving must not move it: "+
			"a balance-delta reading is masked by exactly the incentive payment "+
			"this system exists to collect, and would sit silent through the "+
			"drawdown it is supposed to stop", rich, poor)
	}
}

// seamStopPath is the sentinel path a seam fixture runs with: the override when
// a test supplied one, and the ordinary dedicated file otherwise.
func seamStopPath(dir, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(dir, "harness.stop")
}

// ---------------------------------------------------------------------------
// 16. lip-603 -- §12's out-of-band halt, `harness.stop`
// ---------------------------------------------------------------------------
//
// The sentinel is a FILE and not a signal because a signal needs a pid, and
// `launchd KeepAlive` restarts anything the operator kills. The durable thing
// has to be the instruction rather than the death of the process.

// seamCreateStop puts a regular file at the fixture's sentinel path.
func (h *seamHarness) seamCreateStop() {
	h.t.Helper()
	if err := os.WriteFile(h.cfg.Paths.Stop, []byte("stop\n"), 0o600); err != nil {
		h.t.Fatalf("creating %s: %v", h.cfg.Paths.Stop, err)
	}
}

// TestHarnessStopSentinelLatchesGlobalStopAndKeepsTheReducerLive is the whole
// unit: the operator's file reaches the durable latch under its own name, and
// the four other columns of the §12 row hold.
//
// `harness_stop` by name matters for the same reason `inv_kill` and `pnl_kill`
// do. §10.4 has the operator read the latch to learn WHY the harness stopped,
// and `portfolio_read` is the label five automatic causes already share -- a
// halt somebody REQUESTED arriving under it is indistinguishable from one the
// harness decided on its own.
//
// The position is 15.00 contracts, past `inv_hard` 7.00 so a reducer is live to
// be observed and under `inv_kill` 18.00 so the cause is this one.
func TestHarnessStopSentinelLatchesGlobalStopAndKeepsTheReducerLive(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "15.00"},
	})
	h.start()
	h.awaitActionable()

	// The control: nothing is stopped before the file exists. Without this the
	// test is satisfied by a harness that halts for some other reason.
	h.awaitTicks(2)
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("the halt latch already reads %q before the sentinel was "+
			"created", got)
	}

	h.seamCreateStop()

	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "harness_stop" {
		t.Fatalf("the operator's stop file latched %q, want harness_stop.\n\n"+
			"§10.4 reads this field to learn which row of the halt table fired. "+
			"A requested halt arriving under a generic label is one whose "+
			"reason cannot be recovered afterwards", got)
	}
	h.await("the SEV2 HARNESS_STOP_REQUESTED anomaly to reach the store",
		func() bool {
			return seamContains(h.anomalyClasses(), "HARNESS_STOP_REQUESTED")
		})
	// The SEVERITY, asserted rather than assumed. §11 makes a requested halt a
	// SEV2: it is an expected operator action and does not need somebody woken,
	// but it does need to be in the record at a level the morning review reads.
	// A class-only assertion cannot tell SEV2 from SEV3.
	if sevs := h.anomalySevs("HARNESS_STOP_REQUESTED"); len(sevs) != 1 ||
		sevs[0] != risk.SEV2 {
		t.Fatalf("HARNESS_STOP_REQUESTED was recorded as %v, want exactly one "+
			"SEV2", sevs)
	}
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")

	// I1's columns, read after the halt is durable. This stops ADDING and
	// nothing else: §5.1 is explicit that a drained harness idles and keeps
	// monitoring, and `SignalController.Confirm` refuses a drain permit to any
	// cause that is not a signal -- so this one cannot end the process however
	// long it is left.
	before := h.snapSeq()
	h.awaitTicks(4)
	if got := h.snapSeq(); got <= before {
		t.Fatalf("the snapshot sequence stopped at %d after the sentinel "+
			"halt; I2 makes the monitor unstoppable and H-TOP-5's stall "+
			"detector is that number advancing", got)
	}
	if s := h.snapshot(); s.Global != quote.WindingDown {
		t.Fatalf("the global state is %s after the sentinel halt, want "+
			"WINDING_DOWN", s.Global)
	}
	m, ok := h.market()
	if !ok || m.State != quote.Reducing {
		t.Fatalf("the market is %v (present=%v) holding 15.00 contracts after "+
			"the sentinel halt, want REDUCING.\n\n"+
			"The operator asked the harness to stop ADDING. A market that went "+
			"IDLE here would be one that stopped trying to get out of the "+
			"position it is still holding", m.State, ok)
	}
}

// TestUnreadableHarnessStopFailsClosedOnce is the fail-closed half, and it is
// the half that decides whether this control can be trusted at all.
//
// Treating an unreadable sentinel as an absent one keeps the harness adding
// while its stop switch is broken -- and the operator has no way to tell, since
// a working "no halt requested" and a broken "I cannot look" produce identical
// behaviour. So anything that is not ENOENT stops the harness, under its OWN
// cause: "you asked me to stop" and "I can no longer tell whether you asked me
// to stop" need different fixes, and the second one needs the path looked at
// before the process is started again.
//
// ENOTDIR is the deterministic failure: the sentinel's parent is a regular file,
// so `Lstat` fails without the test needing to inject a filesystem.
func TestUnreadableHarnessStopFailsClosedOnce(t *testing.T) {
	d := t.TempDir()
	notADir := filepath.Join(d, "not-a-directory")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newSeamHarness(t, seamOptions{
		Rung:     "pilot",
		StopPath: filepath.Join(notADir, "harness.stop"),
	})
	h.start()

	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() != ""
	})
	if got := h.latchTrigger(); got != "harness_stop_unreadable" {
		t.Fatalf("an unreadable sentinel latched %q, want "+
			"harness_stop_unreadable.\n\n"+
			"`harness_stop` would say the operator asked for this. They did "+
			"not: the control they were told to rely on cannot be read, and "+
			"that is the thing the next start needs to know", got)
	}
	h.await("the SEV1 HARNESS_STOP_UNREADABLE anomaly to reach the store",
		func() bool {
			return seamContains(h.anomalyClasses(), "HARNESS_STOP_UNREADABLE")
		})

	// EXACTLY once, over many ticks. The condition is standing and the raise is
	// not: four SEV1s a second would evict every other anomaly from the
	// 256-slot buffer, including the ones explaining what the harness was doing
	// when it stopped.
	h.awaitTicks(8)
	n := 0
	for _, c := range h.anomalyClasses() {
		if c == "HARNESS_STOP_UNREADABLE" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("HARNESS_STOP_UNREADABLE was raised %d times over 8 ticks, "+
			"want exactly 1", n)
	}
	// SEV1, and this one genuinely wakes somebody. The operator's stop switch
	// cannot be read; the harness has halted itself as a precaution, and the
	// path needs looking at before it is started again. A SEV2 here is a broken
	// safety control filed alongside routine notices.
	if sevs := h.anomalySevs("HARNESS_STOP_UNREADABLE"); len(sevs) != 1 ||
		sevs[0] != risk.SEV1 {
		t.Fatalf("HARNESS_STOP_UNREADABLE was recorded as %v, want exactly one "+
			"SEV1: an unreadable stop switch is not a routine notice", sevs)
	}
	if seamContains(h.anomalyClasses(), "HARNESS_STOP_REQUESTED") {
		t.Fatalf("an unreadable sentinel also raised HARNESS_STOP_REQUESTED; " +
			"the two conditions are distinct and so are their fixes")
	}
}

// TestHarnessStopRemovalCannotClearAndPersistentFileDoesNotSpam is the two
// properties that make the sentinel safe to leave lying around.
//
// REMOVAL IS NOT AN UNDO. The stop is durable the moment `commitStop` writes it,
// H-HALT-4 makes it survive the process, and §10.4 makes clearing it an operator
// action through the LATCH. An operator who stopped a harness at 3am and then
// tidied up the file must not find it trading again.
//
// AND THE FILE STAYS PUT. It is the ordinary case -- nobody deletes it
// immediately -- so the raise has to be once per condition rather than once per
// tick.
func TestHarnessStopRemovalCannotClearAndPersistentFileDoesNotSpam(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Rung:      "pilot",
		Positions: map[string]string{seamTicker: "15.00"},
	})
	h.start()
	h.awaitActionable()
	h.seamCreateStop()

	h.await("the durable §12 cause to reach the latch", func() bool {
		return h.latchTrigger() == "harness_stop"
	})

	// It stays put for a while: one anomaly, not one per tick.
	h.awaitTicks(8)
	n := 0
	for _, c := range h.anomalyClasses() {
		if c == "HARNESS_STOP_REQUESTED" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("HARNESS_STOP_REQUESTED was raised %d times over 8 ticks, "+
			"want exactly 1: a standing condition is worth saying once, and "+
			"four times a second into a 256-slot buffer evicts everything else",
			n)
	}

	// Now the operator tidies up.
	if err := os.Remove(h.cfg.Paths.Stop); err != nil {
		t.Fatalf("removing %s: %v", h.cfg.Paths.Stop, err)
	}
	h.awaitTicks(8)

	if got := h.latchTrigger(); got != "harness_stop" {
		t.Fatalf("the durable cause is %q after the sentinel was removed, want "+
			"harness_stop still.\n\n"+
			"`rm` is not an undo. The halt is on disk and H-HALT-4 makes it "+
			"survive the process; clearing it is an operator action against the "+
			"LATCH", got)
	}
	if s := h.snapshot(); s.Global != quote.WindingDown {
		t.Fatalf("the global state is %s after the sentinel was removed, want "+
			"WINDING_DOWN: removing the file must not resume adding", s.Global)
	}
	m, ok := h.market()
	if !ok || m.State == quote.Quoting {
		t.Fatalf("the market is %v (present=%v) after the sentinel was "+
			"removed; QUOTING would mean the harness resumed adding because a "+
			"file went away", m.State, ok)
	}
}

// TestHarnessStopRanksAfterSpecificAutomaticCauses is the first-writer-wins
// ordering, and it is the only property of the sentinel's placement that is
// observable at all.
//
// `commitStop` discards a second cause arriving before the first is durable, so
// when the operator's file and a real §12 finding are both true on one
// evaluation, the order of the checks decides which one §10.4 reads. The
// specific automatic cause has to win: "we have lost more than we said we would"
// tells the operator what happened, and "somebody touched a file" tells them
// something they already know, having touched it.
//
// It runs at OWNER level and calls `evaluate` once, because that is where the
// ordering is deterministic. In a composed run whether the portfolio read or the
// 250 ms tick lands first is a race, and a test of a ranking must not be decided
// by a scheduler.
func TestHarnessStopRanksAfterSpecificAutomaticCauses(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Rung: "pilot"})
	o := h.ownerFor()
	h.connectGate()
	h.acceptBook(o, 1, [][]string{{"0.4000", "20.00"}},
		[][]string{{"0.5500", "20.00"}})

	// A loss past the floor, and the operator's file, both true on the SAME
	// evaluation.
	h.seedOwnerLedger(o, seamHeldLossLegs()...)
	o.pnlQExch = map[string]num.Qty{seamTicker: num.QtyFromFloat(15)}
	o.pnlQKnown = true
	h.seamCreateStop()

	o.evaluate(h.clk.monoNow())

	if got := h.latchTrigger(); got != "pnl_kill" {
		t.Fatalf("an evaluation where BOTH the loss floor and the operator's "+
			"stop file were true latched %q, want pnl_kill.\n\n"+
			"The sentinel is the generic fact that somebody touched a file; "+
			"pnl_kill is a measurement of the account. §10.4's operator reading "+
			"`harness_stop` would learn only what they already knew, and would "+
			"never find out the account was past its loss floor", got)
	}
}

// TestEveryEntryAtHarnessStopPathRequestsStop is why the check is `Lstat` and
// not `Stat`.
//
// The question the sentinel asks is whether the operator PUT SOMETHING at that
// path, and every one of these is something. The dangling symlink is the case
// that separates the two calls: `Stat` follows the link, fails to find the
// target, and reports ENOENT -- so a halt the operator believes they have
// requested produces silence, and the harness keeps adding. It is not an exotic
// shape either; it is what a symlink into a directory that has been moved,
// renamed or not yet mounted looks like.
func TestEveryEntryAtHarnessStopPathRequestsStop(t *testing.T) {
	kinds := []struct {
		name   string
		create func(t *testing.T, path string)
	}{
		{"regular file", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("stop\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to a file that exists", func(t *testing.T, p string) {
			target := p + ".target"
			if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", func(t *testing.T, p string) {
			if err := os.Symlink(p+".this-does-not-exist", p); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{Rung: "pilot"})
			o := h.ownerFor()
			k.create(t, h.cfg.Paths.Stop)

			o.checkHarnessStop()

			if got := h.latchTrigger(); got != "harness_stop" {
				t.Fatalf("a %s at the sentinel path latched %q, want "+
					"harness_stop.\n\n"+
					"The operator put something there. Reading that as "+
					"\"no halt requested\" is the harness deciding it knows "+
					"better than the person who configured it", k.name, got)
			}
		})
	}
}

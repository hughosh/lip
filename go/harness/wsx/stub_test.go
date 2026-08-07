package wsx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// The injected clock
// ---------------------------------------------------------------------------

// fakeClock drives every deadline in this package from the test.
//
// It exists because the alternative -- millisecond parameters and a real clock
// -- makes the ping ladder and the backoff ladder into races, and a flaky
// safety test is worse than no test: it gets re-run until it passes.
//
// BlockUntilTimers is the synchronisation primitive. Advancing a clock that the
// code under test has not yet armed a timer against does nothing at all, so a
// test that advances first and asserts second is asserting on a coin flip.
type fakeClock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	mono   time.Duration
	wall   int64
	timers []*fakeTimer
	// stops counts Stop() calls. It is the only signal a test has that the
	// code under test has FINISHED handling an event rather than merely been
	// woken by one -- the session stops its pong deadline exactly when the
	// pong lands, so `stops` reaching n means n pings have been fully
	// resolved. Advancing the clock before that leaves a stale deadline armed,
	// and the next advance trips it.
	stops int
}

func newFakeClock() *fakeClock {
	c := &fakeClock{wall: 1_700_000_000_000}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *fakeClock) Now() Stamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stamp{WallMs: c.wall + c.mono.Milliseconds(), Mono: c.mono}
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, ch: make(chan time.Time, 1),
		deadline: c.mono + d, active: true}
	c.timers = append(c.timers, t)
	if d <= 0 {
		t.fireLocked()
	}
	c.cond.Broadcast()
	return t
}

// Advance moves the clock and fires every timer that has come due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d
	for _, t := range c.timers {
		if t.active && t.deadline <= c.mono {
			t.fireLocked()
		}
	}
	c.cond.Broadcast()
}

// BlockUntilTimers waits until at least n timers are armed.
func (c *fakeClock) BlockUntilTimers(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		active := 0
		for _, t := range c.timers {
			if t.active {
				active++
			}
		}
		if active >= n {
			return
		}
		c.cond.Wait()
	}
}

// BlockUntilStops waits until at least n timers have been stopped.
func (c *fakeClock) BlockUntilStops(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.stops < n {
		c.cond.Wait()
	}
}

type fakeTimer struct {
	c        *fakeClock
	ch       chan time.Time
	deadline time.Duration
	active   bool
}

func (t *fakeTimer) fireLocked() {
	t.active = false
	select {
	case t.ch <- time.Time{}:
	default:
	}
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	t.c.stops++
	t.c.cond.Broadcast()
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	select {
	case <-t.ch:
	default:
	}
	t.deadline = t.c.mono + d
	t.active = true
	if d <= 0 {
		t.fireLocked()
	}
	t.c.cond.Broadcast()
	return was
}

// ---------------------------------------------------------------------------
// The scripted socket
// ---------------------------------------------------------------------------

// scriptedSocket is a Socket whose every operation the test drives.
type scriptedSocket struct {
	mu sync.Mutex

	frames chan []byte
	// readErr, once set, is what Read returns after `frames` drains.
	readErr chan error

	// pongs gates Ping. A ping takes one value from here; an empty channel
	// means the peer never answers, which is the half-open case.
	pongs chan error

	writes  [][]byte
	closed  bool
	writeFn func([]byte) error
}

func newScriptedSocket() *scriptedSocket {
	return &scriptedSocket{
		frames:  make(chan []byte, 64),
		readErr: make(chan error, 1),
		pongs:   make(chan error, 64),
	}
}

func (s *scriptedSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.frames:
		return b, nil
	case err := <-s.readErr:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *scriptedSocket) Write(ctx context.Context, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeFn != nil {
		if err := s.writeFn(b); err != nil {
			return err
		}
	}
	s.writes = append(s.writes, append([]byte(nil), b...))
	return nil
}

func (s *scriptedSocket) Ping(ctx context.Context) error {
	select {
	case err := <-s.pongs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *scriptedSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *scriptedSocket) writtenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *scriptedSocket) written(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[i]
}

// scriptedDialer hands out sockets in order and records every attempt.
type scriptedDialer struct {
	mu       sync.Mutex
	sockets  []*scriptedSocket
	errs     []error
	attempts int
	handed   chan *scriptedSocket
}

func newScriptedDialer() *scriptedDialer {
	return &scriptedDialer{handed: make(chan *scriptedSocket, 32)}
}

// push queues one dial outcome: a socket, or an error if sock is nil.
func (d *scriptedDialer) push(sock *scriptedSocket, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sockets = append(d.sockets, sock)
	d.errs = append(d.errs, err)
}

func (d *scriptedDialer) Dial(ctx context.Context, url string,
	h http.Header) (Socket, error) {

	d.mu.Lock()
	i := d.attempts
	d.attempts++
	var sock *scriptedSocket
	var err error
	if i < len(d.sockets) {
		sock, err = d.sockets[i], d.errs[i]
	} else {
		err = errors.New("scripted dialer exhausted")
	}
	d.mu.Unlock()

	if err != nil {
		return nil, err
	}
	d.handed <- sock
	return sock, nil
}

func (d *scriptedDialer) attemptCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

// fakeSigner produces deterministic headers and never touches a key.
type fakeSigner struct {
	err   error
	lastM int64
}

func (s *fakeSigner) WSHeaders(nowMs int64) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.lastM = nowMs
	return map[string]string{
		"KALSHI-ACCESS-KEY":       "test-key",
		"KALSHI-ACCESS-TIMESTAMP": fmt.Sprintf("%d", nowMs),
		"KALSHI-ACCESS-SIGNATURE": "test-sig",
	}, nil
}

// ---------------------------------------------------------------------------
// The scripted portfolio source
// ---------------------------------------------------------------------------

type scriptedPortfolio struct {
	mu sync.Mutex

	positions rest.PositionsResult
	orders    rest.OrdersResult
	fills     rest.FillsResult

	calls int
	// order records which endpoint was asked for, in call order. Positions is
	// authoritative and overwrites, so it has to be LAST, and that is a
	// property of the poller worth asserting directly.
	order []string
	// polled receives one value per completed cycle.
	polled chan int

	// delayClk and delay make a cycle overrun its interval, so the poller's
	// coalescing can be exercised without a real slow endpoint.
	delayClk *fakeClock
	delay    time.Duration
}

func (p *scriptedPortfolio) setDelay(clk *fakeClock, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delayClk, p.delay = clk, d
}

func newScriptedPortfolio() *scriptedPortfolio {
	return &scriptedPortfolio{
		positions: completePositions(nil),
		orders:    completeOrders(nil),
		fills:     completeFills(nil),
		polled:    make(chan int, 64),
	}
}

func (p *scriptedPortfolio) Positions(ctx context.Context) rest.PositionsResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.order = append(p.order, "positions")
	return p.positions
}

func (p *scriptedPortfolio) Orders(ctx context.Context, ticker,
	status string) rest.OrdersResult {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.order = append(p.order, "orders")
	return p.orders
}

// callOrder is the sequence of endpoints the poller asked for, in one cycle.
func (p *scriptedPortfolio) callOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order...)
}

func (p *scriptedPortfolio) Fills(ctx context.Context, ticker string,
	since time.Time) rest.FillsResult {

	p.mu.Lock()
	p.calls++
	p.order = append(p.order, "fills")
	n := p.calls
	f := p.fills
	clk, d := p.delayClk, p.delay
	// One-shot: the delay applies to exactly the cycle it was armed for, so a
	// test does not have to disarm it before the next cycle starts.
	p.delayClk, p.delay = nil, 0
	p.mu.Unlock()
	if clk != nil && d > 0 {
		clk.Advance(d)
	}
	// Fills is the last endpoint the poller reads, so counting here counts
	// completed cycles.
	select {
	case p.polled <- n:
	default:
	}
	return f
}

func (p *scriptedPortfolio) set(pos rest.PositionsResult, ord rest.OrdersResult,
	fl rest.FillsResult) {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.positions, p.orders, p.fills = pos, ord, fl
}

// ---------------------------------------------------------------------------
// Result constructors
// ---------------------------------------------------------------------------

func completeWalk() rest.Walk { return rest.Walk{Outcome: rest.WalkComplete} }

func failedWalk(why string) rest.Walk {
	return rest.Walk{Outcome: rest.WalkFailed, Err: errors.New(why)}
}

func completePositions(byTicker map[string]num.Qty) rest.PositionsResult {
	if byTicker == nil {
		byTicker = map[string]num.Qty{}
	}
	return rest.PositionsResult{Walk: completeWalk(), ByTicker: byTicker}
}

func completeOrders(orders []rest.Order) rest.OrdersResult {
	return rest.OrdersResult{Walk: completeWalk(), Orders: orders}
}

func completeFills(fills []rest.Fill) rest.FillsResult {
	return rest.FillsResult{Walk: completeWalk(), Fills: fills}
}

// ledger is a fake risk.OwnershipLookup.
type ledger struct {
	ours map[string]bool
	err  error
}

func (l ledger) OwnsOrders(ids []string) ([]bool, error) {
	if l.err != nil {
		return nil, l.err
	}
	out := make([]bool, len(ids))
	for i, id := range ids {
		out[i] = l.ours[id]
	}
	return out, nil
}

func owns(ids ...string) ledger {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return ledger{ours: m}
}

// ---------------------------------------------------------------------------
// Captured-wire fixtures
// ---------------------------------------------------------------------------
//
// Shapes taken from the frames `feed`/`core` already parse in production. They
// prove that a real snapshot, delta and trade round-trip through this package's
// classifier unchanged -- which is the one thing a hand-built fixture set
// cannot establish on its own.

const (
	fxTicker = "KXNBAGAME-26AUG05LALBOS-LAL"

	fxSnapshot = `{"type":"orderbook_snapshot","sid":7,"seq":1,"msg":{` +
		`"market_ticker":"KXNBAGAME-26AUG05LALBOS-LAL","ts_ms":1754400000000,` +
		`"yes_dollars_fp":[["0.4200","310.00"],["0.4100","1200.00"]],` +
		`"no_dollars_fp":[["0.5700","88.00"],["0.5600","640.00"]]}}`

	fxDelta = `{"type":"orderbook_delta","sid":7,"seq":2,"msg":{` +
		`"market_ticker":"KXNBAGAME-26AUG05LALBOS-LAL","ts_ms":1754400001000,` +
		`"side":"yes","price_dollars":"0.4200","delta_fp":"-10.00"}}`

	// A FRACTIONAL print. 17.08% of prints are fractional cents and this must
	// stay legal (H-CO-3a).
	fxTrade = `{"type":"trade","sid":9,"seq":3,"msg":{` +
		`"trade_id":"1582884a-1f64-7c20-fab4-b40468ad5df6",` +
		`"market_ticker":"KXNBAGAME-26AUG05LALBOS-LAL","ts_ms":1754400002000,` +
		`"count_fp":"3.00","yes_price_dollars":"0.4250",` +
		`"no_price_dollars":"0.5750","taker_side":"yes"}}`

	// The same snapshot with ONE level moved off the cent grid.
	fxSnapshotFractional = `{"type":"orderbook_snapshot","sid":7,"seq":1,"msg":{` +
		`"market_ticker":"KXNBAGAME-26AUG05LALBOS-LAL","ts_ms":1754400000000,` +
		`"yes_dollars_fp":[["0.4200","310.00"],["0.4150","1200.00"]],` +
		`"no_dollars_fp":[["0.5700","88.00"]]}}`

	fxDeltaFractional = `{"type":"orderbook_delta","sid":7,"seq":2,"msg":{` +
		`"market_ticker":"KXNBAGAME-26AUG05LALBOS-LAL","ts_ms":1754400001000,` +
		`"side":"yes","price_dollars":"0.4225","delta_fp":"-10.00"}}`
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func testParams() cfg.Params {
	p := cfg.Default()
	p.NMarkets = 1
	return p
}

func contracts(c float64) num.Qty {
	half := 0.5
	if c < 0 {
		half = -0.5
	}
	return num.Qty(int64(c*num.QtyScale + half))
}

func cents4(c int) int64 { return int64(c) * 100 }

func hasClass(as []risk.Anomaly, class string) bool {
	for _, a := range as {
		if a.Class == class {
			return true
		}
	}
	return false
}

func classesOf(as []risk.Anomaly) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Class)
	}
	return out
}

func sevOf(as []risk.Anomaly, class string) (risk.Severity, bool) {
	for _, a := range as {
		if a.Class == class {
			return a.Sev, true
		}
	}
	return risk.SEV3, false
}

// restFill builds a `rest.Fill` with a well-formed fee by default.
func restFill(tradeID, orderID, ticker string, side quote.Side, count float64,
	fee string, taker bool) rest.Fill {

	return rest.Fill{
		FillID: tradeID + "-f", TradeID: tradeID, OrderID: orderID,
		Ticker: ticker, Side: side, Price4: cents4(42),
		Count: contracts(count), IsTaker: taker, FeeCost: fee,
	}
}

// okHandle is a core handler that accepts every frame.
func okHandle() error { return nil }

// rejectHandle is a core handler that refuses every frame, the way
// `core.Rig.Handle` refuses a snapshot whose sizes do not parse.
func rejectHandle(why string) func() error {
	return func() error { return errors.New(why) }
}

// connectAndReconcile brings a gate to the state where placement is licensed:
// connected, snapshotted on this generation, and all three portfolio endpoints
// reconciled against this connection's token.
func connectAndReconcile(g *Gate, ticker string, now Stamp) ReconcileToken {
	ce := g.OnConnect(now)
	g.ApplyFrame(FrameInfo{Kind: FrameSnapshot, Ticker: ticker, Deliver: true},
		okHandle, now)
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, ce.Token, now)
	}
	return ce.Token
}

// newRead assembles a PortfolioRead the way the poller would, with one stamp
// for all three endpoints. `PortfolioRead` is opaque to production callers, so
// this is the only way a test can build one.
func newRead(tok ReconcileToken, when Stamp, seq uint64,
	pos rest.PositionsResult, ord rest.OrdersResult,
	fl rest.FillsResult) PortfolioRead {

	return PortfolioRead{
		token: tok, seq: seq,
		fills: fl, orders: ord, positions: pos,
		fillsAt: when, ordersAt: when, positionsAt: when, completedAt: when,
	}
}

// newStaggeredRead gives each endpoint its own start stamp, which is what the
// poller really does: the walks run in sequence and the slow one moves the
// clock for the ones after it.
func newStaggeredRead(tok ReconcileToken, seq uint64,
	fillsAt, ordersAt, positionsAt, completedAt Stamp,
	pos rest.PositionsResult, ord rest.OrdersResult,
	fl rest.FillsResult) PortfolioRead {

	return PortfolioRead{
		token: tok, seq: seq,
		fills: fl, orders: ord, positions: pos,
		fillsAt: fillsAt, ordersAt: ordersAt, positionsAt: positionsAt,
		completedAt: completedAt,
	}
}

func at(sec int) Stamp {
	return Stamp{WallMs: 1_700_000_000_000 + int64(sec)*1000,
		Mono: time.Duration(sec) * time.Second}
}

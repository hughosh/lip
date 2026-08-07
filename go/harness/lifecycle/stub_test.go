package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// testParams is §16's defaults. Every test that needs parameters uses these, so
// a test that passes because it quietly weakened a threshold is not available.
func testParams() cfg.Params { return cfg.Default() }

// ---------------------------------------------------------------------------
// Latch stubs
// ---------------------------------------------------------------------------

// recordingLatch is a LatchStore whose disk can be made to fail on demand.
type recordingLatch struct {
	rec      LatchRecord
	present  bool
	loadErr  error
	writeErr error
	// notDurable makes Ensure return (false, nil): the store reporting, without
	// an error, that the record did not reach the disk.
	notDurable bool
	ensures    []LatchRecord
	loads      int
}

func (l *recordingLatch) Load() (LatchRecord, bool, error) {
	l.loads++
	return l.rec, l.present, l.loadErr
}

func (l *recordingLatch) Ensure(rec LatchRecord) (bool, error) {
	l.ensures = append(l.ensures, rec)
	if l.writeErr != nil {
		return false, l.writeErr
	}
	if l.notDurable {
		return false, nil
	}
	if !l.present {
		l.present = true
		l.rec = rec
	}
	return true, nil
}

// tempLatch builds a real FileLatch under the test's own directory.
func tempLatch(t *testing.T) *FileLatch {
	t.Helper()
	l, err := NewFileLatch(filepath.Join(t.TempDir(), "harness.halt"))
	if err != nil {
		t.Fatalf("NewFileLatch: %v", err)
	}
	return l
}

// ---------------------------------------------------------------------------
// Ownership
// ---------------------------------------------------------------------------

// ledger is a durable-ownership stand-in for tests ONLY. Production has no such
// type: `risk.OwnershipLookup`'s implementation is `lip-6w5`'s, against storage.
type ledger struct {
	owns map[string]bool
	// err is the ledger being unavailable: not "nothing is ours".
	err error
}

func (l ledger) OwnsOrders(orderIDs []string) ([]bool, error) {
	if l.err != nil {
		return nil, l.err
	}
	out := make([]bool, len(orderIDs))
	for i, id := range orderIDs {
		out[i] = l.owns[id]
	}
	return out, nil
}

func ownsAll(ids ...string) ledger {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return ledger{owns: m}
}

// ---------------------------------------------------------------------------
// Portfolio source
// ---------------------------------------------------------------------------

// call is one recorded endpoint invocation, in order.
type call struct {
	what   string
	ticker string
	status string
	since  time.Time
}

// fakeSource records the ORDER of the four startup reads, which is the property
// §7.5 fixes and `M-L-ADOPTORDER` breaks.
type fakeSource struct {
	calls []call

	positions rest.PositionsResult
	orders    rest.OrdersResult
	fills     rest.FillsResult
	balance   rest.Balance
	balErr    error
}

func (s *fakeSource) Positions(ctx context.Context) rest.PositionsResult {
	s.calls = append(s.calls, call{what: "positions"})
	return s.positions
}

func (s *fakeSource) Orders(ctx context.Context, ticker, status string) rest.OrdersResult {
	s.calls = append(s.calls, call{what: "orders", ticker: ticker, status: status})
	return s.orders
}

func (s *fakeSource) Fills(ctx context.Context, ticker string, since time.Time) rest.FillsResult {
	s.calls = append(s.calls, call{what: "fills", ticker: ticker, since: since})
	return s.fills
}

func (s *fakeSource) Balance(ctx context.Context) (rest.Balance, error) {
	s.calls = append(s.calls, call{what: "balance"})
	return s.balance, s.balErr
}

func (s *fakeSource) sequence() []string {
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, c.what)
	}
	return out
}

// completeWalk is a walk that may replace state.
func completeWalk() rest.Walk { return rest.Walk{Outcome: rest.WalkComplete, Pages: 1} }

// failedWalk is a walk that may not.
func failedWalk(why string) rest.Walk {
	return rest.Walk{Outcome: rest.WalkFailed, Err: errors.New(why)}
}

// okSource is a source whose four reads all succeed and report nothing held.
func okSource() *fakeSource {
	return &fakeSource{
		positions: rest.PositionsResult{
			Walk: completeWalk(), ByTicker: map[string]num.Qty{},
		},
		orders:  rest.OrdersResult{Walk: completeWalk()},
		fills:   rest.FillsResult{Walk: completeWalk()},
		balance: rest.Balance{Cents: 10_000},
	}
}

// ---------------------------------------------------------------------------
// Adoption policy and sweeper
// ---------------------------------------------------------------------------

// fixedPolicy answers the same way for every order, and records what it saw.
type fixedPolicy struct {
	decision AdoptionDecision
	err      error
	seen     []rest.Order
	facts    []AdoptionFacts
}

func (p *fixedPolicy) DecideAdopted(ctx context.Context, o rest.Order,
	f AdoptionFacts) (AdoptionDecision, error) {

	p.seen = append(p.seen, o)
	p.facts = append(p.facts, f)
	if p.err != nil {
		return AdoptionUnset, p.err
	}
	return p.decision, nil
}

func keepAll() *fixedPolicy { return &fixedPolicy{decision: AdoptionKeep} }

// recordingSweeper records every sweep request and answers with a fixed verdict.
//
// When bound to a source it also REMOVES the swept orders from it, which is what
// a real exchange does: `Clean` means a complete verifying read found none of
// the requested orders resting, so the rewalk that follows cannot see them
// again. A sweeper that leaves them in place models an exchange contradicting
// its own verification, and startup treats that as a failure rather than
// cancelling forever.
type recordingSweeper struct {
	clean    bool
	src      *fakeSource
	requests map[string][]rest.Order
}

func newSweeper(clean bool) *recordingSweeper {
	return &recordingSweeper{clean: clean, requests: map[string][]rest.Order{}}
}

// newSweeperOn binds the sweeper to the source it cancels against.
func newSweeperOn(src *fakeSource, clean bool) *recordingSweeper {
	s := newSweeper(clean)
	s.src = src
	return s
}

func (s *recordingSweeper) CancelAndSweep(ctx context.Context, ticker string,
	orders []rest.Order) rest.SweepResult {

	s.requests[ticker] = append(s.requests[ticker], orders...)
	res := rest.SweepResult{Walk: completeWalk(), Rounds: 1, Clean: s.clean}
	if !s.clean {
		res.StillResting = append(res.StillResting, orders...)
		return res
	}
	if s.src != nil {
		gone := make(map[string]bool, len(orders))
		for _, o := range orders {
			gone[o.OrderID] = true
		}
		kept := make([]rest.Order, 0, len(s.src.orders.Orders))
		for _, o := range s.src.orders.Orders {
			if !gone[o.OrderID] {
				kept = append(kept, o)
			}
		}
		s.src.orders.Orders = kept
	}
	return res
}

func (s *recordingSweeper) swept() []rest.Order {
	out := make([]rest.Order, 0)
	for _, os := range s.requests {
		out = append(out, os...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// ourOrder is a resting order bearing one of our coids.
func ourOrder(id, ticker string) rest.Order {
	return rest.Order{
		OrderID: id, ClientOrderID: "lipH-" + id, Ticker: ticker,
		Remaining: num.QtyFromFloat(1), Status: rest.StatusResting, Ours: true,
	}
}

// foreignOrder is a resting order on the account that is not ours.
func foreignOrder(id, ticker string) rest.Order {
	return rest.Order{
		OrderID: id, ClientOrderID: "manual-" + id, Ticker: ticker,
		Remaining: num.QtyFromFloat(1), Status: rest.StatusResting, Ours: false,
	}
}

// makerFill is a fill of ours with the zero fee a maker fill has (S2).
func makerFill(trade, order, ticker string) rest.Fill {
	return rest.Fill{
		FillID: "f-" + trade, TradeID: trade, OrderID: order, Ticker: ticker,
		Price4: 5000, Count: num.QtyFromFloat(1), IsTaker: false,
		FeeCost: "0.0000", TsMillis: 1,
	}
}

// newStartup builds a Startup over a real bootstrapped controller.
func newStartup(t *testing.T, store LatchStore, src PortfolioSource,
	own ledger, policy AdoptionPolicy, sweep CancelSweeper,
	selected ...string) *Startup {

	t.Helper()
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	guard, err := NewForeignGuard(own)
	if err != nil {
		t.Fatalf("NewForeignGuard: %v", err)
	}
	s, err := NewStartup(ctrl, src, guard, policy, sweep, testParams(), selected)
	if err != nil {
		t.Fatalf("NewStartup: %v", err)
	}
	return s
}

// classesOf lists the anomaly classes present, so assertions can care which
// alarm fired rather than how it was worded.
func classesOf(anoms []risk.Anomaly) []string {
	out := make([]string, 0, len(anoms))
	for _, a := range anoms {
		out = append(out, a.Class)
	}
	return out
}

// hasClass reports whether an anomaly of that class was raised.
func hasClass(anoms []risk.Anomaly, want string) bool {
	for _, a := range anoms {
		if a.Class == want {
			return true
		}
	}
	return false
}

// sevOf returns the severity of the first anomaly of that class.
func sevOf(anoms []risk.Anomaly, class string) (risk.Severity, bool) {
	for _, a := range anoms {
		if a.Class == class {
			return a.Sev, true
		}
	}
	return risk.SEV3, false
}

func describe(seq []string) string { return fmt.Sprint(seq) }

// startingInput is the global input a STARTING process presents to Startup.Run.
func startingInput() quote.GlobalInput {
	return quote.GlobalInput{State: quote.Starting}
}

// grantedPermit issues a real DrainPermit the way production does: through a
// SignalController bound to a controller whose latch write succeeds. There is no
// other way to obtain one, which is the point.
func grantedPermit(t *testing.T, store LatchStore) (DrainPermit, GlobalDecision) {
	t.Helper()
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	sc, err := NewSignalController(ctrl)
	if err != nil {
		t.Fatalf("NewSignalController: %v", err)
	}
	eff := sc.Handle(syscall.SIGTERM, quote.GlobalInput{State: quote.Running},
		1_700_000_000_000, time.Minute)
	return eff.Permit, eff.Decision
}

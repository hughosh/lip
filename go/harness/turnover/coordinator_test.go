package turnover

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
)

type fakeDurable struct {
	state *State
	fail  bool
}

func (d *fakeDurable) Load(context.Context) (*State, error) {
	if d.state == nil {
		return nil, nil
	}
	out := cloneState(*d.state)
	return &out, nil
}
func (d *fakeDurable) Replace(_ context.Context, s State) error {
	if d.fail {
		return errors.New("disk unavailable")
	}
	copy := cloneState(s)
	d.state = &copy
	return nil
}

type fakeObservations struct {
	in         Input
	requested  []string
	cash       map[Shard]num.Money
	failTicker string
}

func (f *fakeObservations) Account(context.Context) (Input, error) { return cloneInput(f.in), nil }
func (f *fakeObservations) Market(_ context.Context, ticker string) (Observation, error) {
	f.requested = append(f.requested, ticker)
	if ticker == f.failTicker {
		return Observation{}, errors.New("book unavailable")
	}
	var shard Shard
	for _, id := range f.in.Identities {
		if id.Ticker == ticker {
			shard = id.Shard
		}
	}
	proof := f.in.OrdersProof
	return Observation{Identity: Identity{Ticker: ticker, Shard: shard, Proof: proof},
		Cash: Cash{Available: f.cash[shard], Proof: proof}, Book: Book{Ticker: ticker, Proof: proof, Mark4: 5000},
		Schedule: Schedule{Ticker: ticker, Proof: proof, AddUntil: f.in.Now + time.Hour, ReduceUntil: f.in.Now + 2*time.Hour}}, nil
}

func setupCoordinator(t *testing.T) (*Coordinator, *fakeObservations, *fakeDurable, *time.Duration, cfg.Params) {
	t.Helper()
	in := fixture()
	now := in.Now
	src := &fakeObservations{in: in, cash: map[Shard]num.Money{in.Identities[0].Shard: dollars(10), in.Identities[1].Shard: dollars(20)}}
	store := &fakeDurable{}
	p := cfg.Default()
	p.CapitalMax = in.AggregateCap
	c, err := NewCoordinator(context.Background(), src, store, func() time.Duration { return now }, p, in.FrozenCaps)
	if err != nil {
		t.Fatal(err)
	}
	return c, src, store, &now, p
}

func reducer(id string, qty num.Qty) Intent {
	return Intent{Order: Order{ID: id, Ticker: "A", Side: quote.SideNo, Quantity: qty, Price4: 4000, Ownership: Owned}, Role: quote.RoleReducing}
}
func adding(id string) Intent {
	return Intent{Order: Order{ID: id, Ticker: "B", Side: quote.SideYes, Quantity: 10, Price4: 4000, Ownership: Owned}, Role: quote.RoleAdding, AddingAuthorized: true}
}

func TestCoordinatorTurnoverRestartObservesOldAndNewMarkets(t *testing.T) {
	c, src, store, now, params := setupCoordinator(t)
	ctx := context.Background()
	if _, err := c.Turn(ctx, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	src.requested = nil
	p, err := c.Turn(ctx, []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.requested, []string{"A", "B"}) || p.Markets[0].State != quote.Reducing || p.Markets[0].Selected {
		t.Fatalf("turnover: %+v %v", p, src.requested)
	}
	if err := c.Admit(ctx, reducer("unknown-A", 100)); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(ctx, reducer("oversized-A", 101)); err == nil {
		t.Fatal("oversized aggregate reducer admitted")
	}
	// Simulate a process with a new monotonic origin and no order in REST yet.
	*now = time.Second
	src.in.Now = *now
	proof := Proof{Complete: true, At: *now}
	src.in.PositionsProof, src.in.OrdersProof, src.in.OwnershipProof, src.in.PendingProof = proof, proof, proof, proof
	restarted, err := NewCoordinator(ctx, src, store, func() time.Duration { return *now }, params, src.in.FrozenCaps)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Admit(ctx, adding("before-observe")); err == nil {
		t.Fatal("restart reused old observations")
	}
	if _, err := restarted.PnLInputs(nil); err == nil {
		t.Fatal("restart exposed old P&L truth")
	}
	src.requested = nil
	p, err = restarted.Turn(ctx, []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.requested, []string{"A", "B"}) || len(p.Owned) != 1 || p.Owned[0].ID != "unknown-A" {
		t.Fatalf("restart dropped old obligations: %+v", p)
	}
	inputs, err := restarted.PnLInputs([]string{"HISTORICAL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].Ticker != "A" || !inputs[0].MarkOK || inputs[0].QExch != 200 || inputs[1].Ticker != "HISTORICAL" {
		t.Fatalf("P&L inputs: %+v", inputs)
	}
}

func TestCoordinatorFailureRetainsPlanAndBlocksAdmission(t *testing.T) {
	for _, failure := range []string{"partial", "book", "disk"} {
		t.Run(failure, func(t *testing.T) {
			c, src, store, _, _ := setupCoordinator(t)
			ctx := context.Background()
			before, err := c.Turn(ctx, []string{"A"})
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "partial":
				src.in.PositionsProof.Complete = false
			case "book":
				src.failTicker = "B"
			case "disk":
				store.fail = true
			}
			after, err := c.Turn(ctx, []string{"B"})
			if err == nil {
				t.Fatal("failed observation accepted")
			}
			if !reflect.DeepEqual(before.Markets, after.Markets) {
				t.Fatal("failure changed published selection")
			}
			if err := c.Admit(ctx, adding("blocked")); err == nil {
				t.Fatal("failure admitted addition")
			}
		})
	}
}

func TestCoordinatorReservationsRespectCashAndPersistBeforeAdmission(t *testing.T) {
	c, src, store, _, _ := setupCoordinator(t)
	ctx := context.Background()
	a := src.in.Identities[0].Shard
	src.cash[a] = 400000
	if _, err := c.Turn(ctx, []string{"B"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(ctx, reducer("one", 100)); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(ctx, reducer("two", 1)); err == nil {
		t.Fatal("A borrowed B cash")
	}
	if len(store.state.Snapshot.Orders) != 1 {
		t.Fatal("reservation was not persisted")
	}
	store.fail = true
	if err := c.Admit(ctx, adding("disk-failed")); err == nil {
		t.Fatal("unpersisted reservation admitted")
	}
	store.fail = false
	if err := c.Admit(ctx, adding("after-failure")); err == nil {
		t.Fatal("admission recovered without reconciliation")
	}
}

func TestCoordinatorForeignStopPersistsAndNeverCancelsForeign(t *testing.T) {
	c, src, store, now, params := setupCoordinator(t)
	ctx := context.Background()
	src.in.Identities = append(src.in.Identities, Identity{Ticker: "C", Shard: src.in.Identities[0].Shard, Proof: src.in.OrdersProof})
	src.in.Orders = []Order{
		{ID: "foreign-C", Ticker: "C", Side: quote.SideYes, Quantity: 100, Price4: 5000, Ownership: Foreign},
		{ID: "own-add-A", Ticker: "A", Side: quote.SideYes, Quantity: 10, Price4: 4000, Ownership: Owned},
	}
	if _, err := c.Turn(ctx, []string{"B", "C"}); err != nil {
		t.Fatal(err)
	}
	cancels := c.CancelCandidates()
	if len(cancels) != 1 || cancels[0].ID != "own-add-A" || !store.state.StopAdding {
		t.Fatalf("foreign stop/cancels: %+v", cancels)
	}
	src.in.Orders = src.in.Orders[1:]
	restarted, err := NewCoordinator(ctx, src, store, func() time.Duration { return *now }, params, src.in.FrozenCaps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Turn(ctx, []string{"B"}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Admit(ctx, adding("foreign-gone")); err == nil {
		t.Fatal("foreign disappearance cleared durable stop")
	}
}

func TestCoordinatorCancelCandidatesSelectedReducingMarket(t *testing.T) {
	for _, tc := range []struct {
		name         string
		position     num.Qty
		addingSide   quote.Side
		reducingSide quote.Side
	}{
		{name: "held-yes", position: 200, addingSide: quote.SideYes, reducingSide: quote.SideNo},
		{name: "held-no", position: -200, addingSide: quote.SideNo, reducingSide: quote.SideYes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, src, _, _, _ := setupCoordinator(t)
			ctx := context.Background()
			src.in.Positions["A"] = tc.position
			src.in.Orders = []Order{
				{ID: "own-add-A", Ticker: "A", Side: tc.addingSide, Quantity: 10, Price4: 4000, Ownership: Owned},
				{ID: "own-reduce-A", Ticker: "A", Side: tc.reducingSide, Quantity: 20, Price4: 4000, Ownership: Owned},
			}
			plan, err := c.Turn(ctx, []string{"A"})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Markets) != 1 || !plan.Markets[0].Selected || plan.Markets[0].State != quote.Reducing || c.state.StopAdding {
				t.Fatalf("expected selected reducing market without global stop: %+v", plan.Markets)
			}
			if got := c.CancelCandidates(); len(got) != 1 || got[0].ID != "own-add-A" {
				t.Fatalf("selected reducing market cancels: %+v", got)
			}

			// A foreign order elsewhere must never become our cancel candidate.
			src.in.Orders = append(src.in.Orders, Order{ID: "foreign-B", Ticker: "B", Side: quote.SideYes, Quantity: 10, Price4: 4000, Ownership: Foreign})
			if _, err := c.Turn(ctx, []string{"A", "B"}); err != nil {
				t.Fatal(err)
			}
			if got := c.CancelCandidates(); len(got) != 1 || got[0].ID != "own-add-A" {
				t.Fatalf("foreign or reducer cancel proposed: %+v", got)
			}
		})
	}
}

func TestCoordinatorForeignCommitmentsReduceReducerCash(t *testing.T) {
	c, src, _, _, _ := setupCoordinator(t)
	ctx := context.Background()
	a := src.in.Identities[0].Shard
	src.cash[a] = 500000
	src.in.Orders = []Order{{ID: "foreign-A", Ticker: "A", Side: quote.SideYes, Quantity: 100, Price4: 4000, Ownership: Foreign}}
	if _, err := c.Turn(ctx, []string{"B"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(ctx, reducer("would-spend-foreign", 100)); err == nil {
		t.Fatal("reducer consumed foreign reservation")
	}
	if len(c.CancelCandidates()) != 0 {
		t.Fatal("foreign cancel proposed")
	}
}

func TestCoordinatorFreshnessAndDefensiveCopies(t *testing.T) {
	c, _, _, now, params := setupCoordinator(t)
	ctx := context.Background()
	p, err := c.Turn(ctx, []string{"B"})
	if err != nil {
		t.Fatal(err)
	}
	p.Markets[0].Ticker = "corrupt"
	if c.Current().Markets[0].Ticker != "A" {
		t.Fatal("published plan aliases coordinator")
	}
	*now += params.TruthMaxAge + 1
	if err := c.Admit(ctx, reducer("stale", 1)); err == nil {
		t.Fatal("stale proof admitted reducer")
	}
	if _, err := c.PnLInputs(nil); err == nil {
		t.Fatal("stale truth reported as P&L input")
	}
}

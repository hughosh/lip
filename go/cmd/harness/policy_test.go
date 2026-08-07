package main

import (
	"context"
	"testing"

	"lip/harness/cfg"
	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
)

// ord builds a well-formed adopted order. Every field the policy reads is set,
// so a test that wants a malformed one has to say which field it broke.
func ord(id, ticker string, side quote.Side, cents int, count float64) rest.Order {
	return rest.Order{
		OrderID:       id,
		ClientOrderID: "lipH-TESTRUN-000-" + side.String() + "-0000000" + id,
		Ticker:        ticker,
		Side:          side,
		Price4:        int64(cents) * 100,
		PriceCents:    cents,
		Remaining:     num.QtyFromFloat(count),
		Status:        rest.StatusResting,
		Ours:          true,
	}
}

// facts assembles AdoptionFacts around a set of owned orders. The defaults are
// the permissive ones -- selected, adding permitted, flat -- so each test states
// only the condition it is about.
func facts(orders []rest.Order, opts ...func(*lifecycle.AdoptionFacts)) lifecycle.AdoptionFacts {
	f := lifecycle.AdoptionFacts{
		Positions:       map[string]num.Qty{},
		Selected:        []string{"MKT"},
		AddingPermitted: true,
		OwnedResting:    orders,
		Balance:         num.MoneyFromDollars(100),
	}
	for _, o := range opts {
		o(&f)
	}
	return f
}

func decide(t *testing.T, p cfg.Params, o rest.Order,
	f lifecycle.AdoptionFacts) lifecycle.AdoptionDecision {

	t.Helper()
	d, err := newAdoptionPolicy(p).DecideAdopted(context.Background(), o, f)
	if err != nil {
		t.Fatalf("DecideAdopted(%s): unexpected error: %v", o.OrderID, err)
	}
	if d != lifecycle.AdoptionKeep && d != lifecycle.AdoptionCancel {
		t.Fatalf("DecideAdopted(%s) returned %v; AdoptionUnset is the zero "+
			"value and H-ORD-5c has no keep-by-default path, so every decided "+
			"order must be explicitly kept or explicitly cancelled",
			o.OrderID, d)
	}
	return d
}

// H-ORD-5c's own worked example: "including an adding order in a market that is
// now flat and unselected".
func TestFlatUnselectedMarketKeepsNothing(t *testing.T) {
	p := cfg.Default()
	o := ord("1", "MKT", quote.SideYes, 40, 5)
	f := facts([]rest.Order{o}, func(f *lifecycle.AdoptionFacts) {
		f.Selected = []string{"OTHER"}
	})

	if got := decide(t, p, o, f); got != lifecycle.AdoptionCancel {
		t.Fatalf("an adding order on a flat, unselected market was %v; §7.5's "+
			"own example of what adoption must not inherit", got)
	}
}

// §7.5 step 6 plus §5.2's REDUCING row. The adding side goes whatever the
// selection says, and it goes because the market is HELD, not because of size.
func TestHeldMarketCancelsTheAddingSideAndKeepsTheExit(t *testing.T) {
	p := cfg.Default()
	adding := ord("1", "MKT", quote.SideYes, 40, 5)
	exit := ord("2", "MKT", quote.SideNo, 45, 3)
	f := facts([]rest.Order{adding, exit}, func(f *lifecycle.AdoptionFacts) {
		// q > 0, so the reducing side is NO (§6.2).
		f.Positions["MKT"] = num.QtyFromFloat(5)
	})

	if got := decide(t, p, adding, f); got != lifecycle.AdoptionCancel {
		t.Fatalf("the adding side of a held market was %v; §5.2 gives REDUCING "+
			"an adding side that is cancelled and confirmed absent (A8)", got)
	}
	if got := decide(t, p, exit, f); got != lifecycle.AdoptionKeep {
		t.Fatalf("the reducer on a held market was %v; A4 requires a live exit "+
			"and this one is inside |q|", got)
	}
}

// A held market that is NOT selected still keeps its exit. H-SEL-11 beats F15
// where they collide: a market can be barred from new selection and still hold
// inventory that must be reduced.
func TestUnselectedHeldMarketStillKeepsItsExit(t *testing.T) {
	p := cfg.Default()
	exit := ord("1", "MKT", quote.SideNo, 45, 3)
	f := facts([]rest.Order{exit}, func(f *lifecycle.AdoptionFacts) {
		f.Positions["MKT"] = num.QtyFromFloat(5)
		f.Selected = nil
		f.Excluded = []string{"MKT"}
	})

	if got := decide(t, p, exit, f); got != lifecycle.AdoptionKeep {
		t.Fatalf("the exit on an excluded, unselected, HELD market was %v; "+
			"cancelling it would leave inventory with no way out, which is the "+
			"inversion this whole harness is built against", got)
	}
}

// H-Q-5a / A12 / HR-004: the reducing side's AGGREGATE may not exceed |q|. The
// excess is cancelled and the best exit survives -- not the other way round, and
// not the whole side.
func TestReducerAggregateIsCappedAtQAndTheBestExitSurvives(t *testing.T) {
	p := cfg.Default()
	near := ord("1", "MKT", quote.SideNo, 60, 4)
	far := ord("2", "MKT", quote.SideNo, 55, 3)
	f := facts([]rest.Order{near, far}, func(f *lifecycle.AdoptionFacts) {
		f.Positions["MKT"] = num.QtyFromFloat(5)
	})

	if got := decide(t, p, near, f); got != lifecycle.AdoptionKeep {
		t.Fatalf("the exit nearest the touch was %v, but it fits inside "+
			"|q| = 5 on its own", got)
	}
	if got := decide(t, p, far, f); got != lifecycle.AdoptionCancel {
		t.Fatalf("the second exit was %v; 4 + 3 = 7 contracts against |q| = 5 "+
			"is the aggregate H-Q-5a forbids, and two reducers that both fill "+
			"flip the position (HR-004)", got)
	}
}

// H-CO-6 / A3. At q == 0 neither leg is an exit, so neither has A4's claim to
// survive and both go.
func TestSelfCrossingPairOnAFlatMarketLosesBothLegs(t *testing.T) {
	p := cfg.Default()
	yes := ord("1", "MKT", quote.SideYes, 55, 2)
	no := ord("2", "MKT", quote.SideNo, 48, 2)
	f := facts([]rest.Order{yes, no})

	if !quote.Crosses(55, 48) {
		t.Fatal("the fixture does not cross; the test asserts nothing")
	}
	for _, o := range []rest.Order{yes, no} {
		if got := decide(t, p, o, f); got != lifecycle.AdoptionCancel {
			t.Fatalf("leg %s of a self-crossing pair was %v; A3 requires every "+
				"resting pair of ours to sum under 100", o.OrderID, got)
		}
	}
}

// I1 stated from the policy's side: a process that may not add does not adopt a
// risk-adding order, and at q == 0 every order is one.
func TestRevokedAddingAuthorityKeepsNoOrderOnAFlatMarket(t *testing.T) {
	p := cfg.Default()
	o := ord("1", "MKT", quote.SideYes, 40, 5)
	f := facts([]rest.Order{o}, func(f *lifecycle.AdoptionFacts) {
		f.AddingPermitted = false
	})

	if got := decide(t, p, o, f); got != lifecycle.AdoptionCancel {
		t.Fatalf("an adding order was %v while adding authority was revoked", got)
	}
}

// The deliberate ABSENCE that keeps `M-L-EXCLUDESELECT` catchable.
//
// `lifecycle` promises `Selected` is the EFFECTIVE set with every
// foreign-excluded ticker already removed, and the mutation passes the RAW set
// instead. A policy that defensively subtracted `Excluded` itself would return
// the right answer under the mutation and the mutation would go uncaught -- so
// this asserts that the policy does NOT do that.
func TestPolicyDoesNotSubtractExcludedFromSelectedItself(t *testing.T) {
	p := cfg.Default()
	o := ord("1", "MKT", quote.SideYes, 40, 5)
	f := facts([]rest.Order{o}, func(f *lifecycle.AdoptionFacts) {
		f.Selected = []string{"MKT"}
		f.Excluded = []string{"MKT"}
	})

	if got := decide(t, p, o, f); got != lifecycle.AdoptionKeep {
		t.Fatalf("a ticker present in BOTH Selected and Excluded was %v. The "+
			"policy must trust Selected as the effective set: filtering "+
			"Excluded again here masks M-L-EXCLUDESELECT, which breaks "+
			"lifecycle by passing the raw selection", got)
	}
}

// §6.2's S_max is an aggregate per side per market, and it binds at adoption
// however the previous incarnation reached it.
func TestAggregatePastSMaxIsTrimmedToThePrefixThatFits(t *testing.T) {
	p := cfg.Default() // S_max = 48 contracts
	big := ord("1", "MKT", quote.SideYes, 40, 40)
	rest2 := ord("2", "MKT", quote.SideYes, 39, 20)
	f := facts([]rest.Order{big, rest2})

	if got := decide(t, p, big, f); got != lifecycle.AdoptionKeep {
		t.Fatalf("40 contracts against S_max = 48 was %v", got)
	}
	if got := decide(t, p, rest2, f); got != lifecycle.AdoptionCancel {
		t.Fatalf("the order taking the side's aggregate to 60 against "+
			"S_max = 48 was %v", got)
	}
}

// H-CAP-1/2/3 at adoption. The prefix that fits the per-market ceiling stays;
// the rest is cancelled rather than the whole market being emptied.
func TestAddingOrdersBeyondTheCapitalCeilingAreCancelled(t *testing.T) {
	p := cfg.Default()
	// capital_max $20 over 6 markets at concentration 2.0 is a $6.67 per-market
	// ceiling (H-CAP-2), which two $4-ish orders cannot both fit under.
	p.CapitalMax = num.MoneyFromDollars(20)
	if err := p.Validate(); err != nil {
		t.Fatalf("the fixture is not a valid §16 parameter set: %v", err)
	}

	near := ord("1", "MKT", quote.SideYes, 45, 10) // $4.50
	far := ord("2", "MKT", quote.SideYes, 40, 10)  // $4.00
	f := facts([]rest.Order{near, far})

	if got := decide(t, p, near, f); got != lifecycle.AdoptionKeep {
		t.Fatalf("the first adding order was %v; $4.50 fits a $6.67 "+
			"per-market ceiling on its own", got)
	}
	if got := decide(t, p, far, f); got != lifecycle.AdoptionCancel {
		t.Fatalf("the second adding order was %v; $4.50 + $4.00 exceeds "+
			"H-CAP-2's per-market ceiling", got)
	}
}

// A price this harness cannot express is a price it cannot requote against.
func TestUnusablePricesAndSizesAreNeverAdopted(t *testing.T) {
	p := cfg.Default()
	fractional := ord("1", "MKT", quote.SideYes, 40, 5)
	fractional.Fractional = true
	tooHigh := ord("2", "MKT", quote.SideYes, 100, 5)
	empty := ord("3", "MKT", quote.SideYes, 40, 0)
	unaddressable := ord("4", "MKT", quote.SideYes, 40, 5)
	unaddressable.OrderID = ""

	for _, o := range []rest.Order{fractional, tooHigh, empty, unaddressable} {
		f := facts([]rest.Order{o})
		if got := decide(t, p, o, f); got != lifecycle.AdoptionCancel {
			t.Fatalf("order %q (%+v) was %v; nothing downstream can price, "+
				"size or address it", o.ClientOrderID, o, got)
		}
	}
}

// The aggregate rules are answered by rebuilding one total ordering over the
// whole owned set, so the exchange's listing order must not change any answer.
func TestDecisionsDoNotDependOnTheOrderTheExchangeListedTheWalkIn(t *testing.T) {
	p := cfg.Default()
	a := ord("1", "MKT", quote.SideNo, 60, 4)
	b := ord("2", "MKT", quote.SideNo, 55, 3)
	c := ord("3", "MKT", quote.SideNo, 50, 2)
	pos := func(f *lifecycle.AdoptionFacts) {
		f.Positions["MKT"] = num.QtyFromFloat(5)
	}

	forward := facts([]rest.Order{a, b, c}, pos)
	reversed := facts([]rest.Order{c, b, a}, pos)

	for _, o := range []rest.Order{a, b, c} {
		one := decide(t, p, o, forward)
		two := decide(t, p, o, reversed)
		if one != two {
			t.Fatalf("order %s decided %v in walk order and %v reversed; the "+
				"aggregate caps would then depend on how the exchange "+
				"happened to page the response", o.OrderID, one, two)
		}
	}
}

// A cancelled context is an error, never a decision. A pass that will not finish
// must not put a cancel on the wire for an order nobody re-examined.
func TestACancelledContextYieldsNoDecision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	o := ord("1", "MKT", quote.SideYes, 40, 5)
	d, err := newAdoptionPolicy(cfg.Default()).DecideAdopted(ctx, o, facts([]rest.Order{o}))
	if err == nil {
		t.Fatal("a cancelled context produced a decision rather than an error")
	}
	if d != lifecycle.AdoptionUnset {
		t.Fatalf("the error path returned %v rather than AdoptionUnset", d)
	}
}

package risk

import (
	"strings"
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
)

func dollars(d float64) num.Money { return num.MoneyFromDollars(d) }
func contracts(c float64) num.Qty {
	half := 0.5
	if c < 0 {
		half = -0.5
	}
	return num.Qty(int64(c*num.QtyScale + half))
}

// cents4 converts an integer-cent price to the 1e-4 USD price quantum.
func cents4(c int) int64 { return int64(c) * 100 }

// TestSideCostAgainstHandComputedCases is V1.6.
//
// §10.2: `capital(market) = S · (p_yes + p_no) / 100` dollars. Evaluated one
// side at a time and summed, never as `S × (p_yes + p_no)` -- the two sides
// carry different quantities once the quote is skewed (§6.2), and a formula
// that assumes they cannot would silently under-reserve exactly when inventory
// is the thing being managed.
func TestSideCostAgainstHandComputedCases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		qty   num.Qty
		cents int
		want  num.Money
	}{
		{"12 contracts at 50c", contracts(12), 50, dollars(6)},
		{"12 contracts at 99c", contracts(12), 99, dollars(11.88)},
		{"one contract at 1c", contracts(1), 1, dollars(0.01)},
		{"a fractional remainder", contracts(0.07), 50, dollars(0.035)},
		{"nothing resting", 0, 50, 0},
		{"a short aggregate is still a cost", contracts(-12), 50, dollars(6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SideCost(tc.qty, cents4(tc.cents)); got != tc.want {
				t.Errorf("SideCost(%s, %dc) = %s, want %s",
					tc.qty.Wire(), tc.cents, got, tc.want)
			}
		})
	}

	// The two-sided quote of §10.2's worked formula, both ways round. A skewed
	// quote is the case that separates them.
	t.Run("the two sides are summed, not multiplied", func(t *testing.T) {
		add, red := contracts(4), contracts(12) // a SKEWED market
		yes, no := 60, 38

		got := SideCost(add, cents4(yes)) + SideCost(red, cents4(no))
		want := dollars(4*0.60 + 12*0.38)
		if got != want {
			t.Fatalf("a skewed two-sided quote costs %s, want %s", got, want)
		}

		// The shortcut formula, S × (p_yes + p_no), only agrees when the sides
		// are equal -- and it UNDER-states the cost here, which is the
		// direction that matters.
		shortcut := SideCost(add, cents4(yes+no))
		if shortcut >= got {
			t.Fatalf("premise broken: the shortcut gave %s against the correct "+
				"%s, so this test no longer demonstrates the under-reserve",
				shortcut, got)
		}
	})
}

// TestFundedContractsFloors pins the rounding direction.
//
// H-CAP-5 makes an insufficient_balance reject a CORRECTNESS failure -- SEV1
// and a global WINDING_DOWN -- not a market condition. So a count rounded UP to
// be generous does not buy an extra hundredth of a contract; it trips the most
// serious stop condition in the system.
func TestFundedContractsFloors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		money num.Money
		cents int
		want  num.Qty
	}{
		{"exact", dollars(6), 50, contracts(12)},
		{"a residue is dropped", dollars(6.004), 50, contracts(12)},
		{"just short of a quantum", dollars(5.999), 50, contracts(11.99)},
		{"nothing available", 0, 50, 0},
		{"already over the cap", dollars(-1), 50, 0},
		{"a free market is not infinite size", dollars(6), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FundedContracts(tc.money, cents4(tc.cents)); got != tc.want {
				t.Errorf("FundedContracts(%s, %dc) = %s, want %s",
					tc.money, tc.cents, got.Wire(), tc.want.Wire())
			}
		})
	}

	// Whatever comes back must actually be affordable. Sweep it.
	for c := 1; c <= 99; c++ {
		for _, m := range []num.Money{0, 1, dollars(0.07), dollars(11.88), dollars(75)} {
			q := FundedContracts(m, cents4(c))
			if cost := SideCost(q, cents4(c)); cost > m {
				t.Fatalf("FundedContracts(%s, %dc) = %s, which costs %s -- "+
					"more than was available", m, c, q.Wire(), cost)
			}
		}
	}
}

// TestReducingBudgetIgnoresTheConcentrationCapAndTheReserve is H-CAP-6 and
// H-CAP-3, and it is the HR-005 regression.
//
// The per-market concentration cap is a diversification rule for TAKING risk.
// Applying it to the exit is the probe's defect reached by arithmetic: under
// the previous defaults a q = −100 position needing a 200-contract reducer at
// 89c required $178, exceeding both the $166.67 per-market cap and the entire
// $125 reserve, so the only exit was unfundable under the harness's own opening
// configuration even after cancelling everything else.
func TestReducingBudgetIgnoresTheConcentrationCapAndTheReserve(t *testing.T) {
	p := cfg.Default() // capital_max $100, reserve 25%, n_markets 6, concentration 2

	// One market holding a large position, well past its concentration share of
	// $100/6 × 2 = $33.33.
	ex := []Exposure{{Ticker: "A", Position: dollars(60)}}

	now, afterCuts := ReducingBudget(ex, p)
	if now != dollars(40) {
		t.Errorf("ReducingBudget now = %s, want $40.00: capital_max less what "+
			"is committed. The reserve is not withheld from the exit -- it "+
			"exists precisely so that a fill never strands the reducing quote "+
			"(H-CAP-3)", now)
	}
	if afterCuts != now {
		t.Errorf("afterCuts = %s, want %s: there are no adding orders to cut",
			afterCuts, now)
	}

	// The concentration cap would have allowed this market $33.33 in total, and
	// it is already at $60. The exit is still funded.
	if now <= 0 {
		t.Fatal("a market past its concentration cap could not fund an exit " +
			"at all: H-CAP-6 exempts the exit from that cap, and a capital " +
			"rule that forbids the exit is the same defect as the probe's")
	}
}

// TestReducingBudgetSeparatesReachableFromAvailable is H-CAP-4.
//
// "Reducing quotes are funded before adding quotes, globally. If capital is
// short, the adding side is what gets cut. Always."
//
// That is a priority, not an accounting identity. Capital tied in an adding
// order is not free until the order is cancelled AND the exchange confirms it
// (H-FAIL-3), so a reducer sized against the post-cut figure before the cancels
// land gets an insufficient_balance reject -- which under H-CAP-5 is a
// correctness failure and a global WINDING_DOWN. The two numbers must therefore
// be distinguishable by the caller.
func TestReducingBudgetSeparatesReachableFromAvailable(t *testing.T) {
	p := cfg.Default()

	ex := []Exposure{
		{Ticker: "A", Position: dollars(30), Reducing: dollars(10)},
		{Ticker: "B", Adding: dollars(50)},
	}

	now, afterCuts := ReducingBudget(ex, p)
	if now != dollars(10) {
		t.Errorf("now = %s, want $10.00 -- $100 less the $90 committed", now)
	}
	if afterCuts != dollars(60) {
		t.Errorf("afterCuts = %s, want $60.00 -- B's $50 of adding orders are "+
			"cuttable under H-CAP-4", afterCuts)
	}
	if afterCuts <= now {
		t.Fatal("the two figures are identical, so a caller cannot tell what " +
			"it can fund NOW from what it could fund after cancelling: sizing " +
			"the exit against the second before the cancels confirm is an " +
			"insufficient_balance reject, and H-CAP-5 makes that a global " +
			"WINDING_DOWN")
	}

	// Over-committed: neither figure goes negative, because a negative budget
	// read as a size would be a short order.
	over := []Exposure{{Ticker: "A", Position: dollars(500)}}
	if n, a := ReducingBudget(over, p); n < 0 || a < 0 {
		t.Errorf("over-committed budgets went negative: now %s, afterCuts %s", n, a)
	}
}

// TestAddingBudgetAppliesAllThreeCaps walks H-CAP-1, H-CAP-2 and H-CAP-3.
func TestAddingBudgetAppliesAllThreeCaps(t *testing.T) {
	p := cfg.Default() // $100, reserve 25% -> $75 deployable; per-market $33.33

	t.Run("the reserve is withheld from adding quotes", func(t *testing.T) {
		// One market at concentration 1, so the per-market cap is the whole of
		// capital_max and cannot be what binds. What is left is H-CAP-3.
		solo := p
		solo.NMarkets, solo.Concentration = 1, 1

		got := AddingBudget("A", nil, 0, solo)
		if got != dollars(75) {
			t.Errorf("an empty book allows %s of adding, want $75.00: "+
				"H-CAP-3 holds 25%% unallocated so a fill never strands the "+
				"reducing quote", got)
		}
	})

	t.Run("the per-market cap binds even on an empty book", func(t *testing.T) {
		// At the shipped six-market configuration the concentration cap is
		// $100/6 × 2 = $33.33, which is tighter than the $75 deployable. A
		// single market never gets the whole deployable pool, empty book or
		// not -- that is what diversification means.
		got := AddingBudget("A", nil, 0, p)
		if want := dollars(100.0 / 6 * 2); got != want {
			t.Errorf("AddingBudget on an empty book = %s, want %s", got, want)
		}
		if got >= dollars(75) {
			t.Errorf("one market was offered %s of the $75 deployable pool", got)
		}
	})

	t.Run("the per-market cap binds before the global one", func(t *testing.T) {
		ex := []Exposure{{Ticker: "A", Adding: dollars(20)}}
		got := AddingBudget("A", ex, 0, p)
		if want := dollars(100.0/6*2) - dollars(20); got != want {
			t.Errorf("AddingBudget = %s, want %s: capital_max / n_markets · "+
				"concentration is $33.33 and $20 of it is spent", got, want)
		}
	})

	t.Run("the position counts toward concentration", func(t *testing.T) {
		held := []Exposure{{Ticker: "A", Position: dollars(33)}}
		if got := AddingBudget("A", held, 0, p); got > dollars(1) {
			t.Errorf("a market already holding $33 of position may add %s "+
				"more: concentration is a rule about risk TAKEN in one "+
				"market, and a position is taken risk", got)
		}
	})

	t.Run("the exit does not count toward concentration", func(t *testing.T) {
		// The same $30 committed in one market, first to its adding side and
		// then to its exit. Under H-CAP-6 only the first consumes the
		// concentration cap, so the two must give different budgets.
		adding := []Exposure{{Ticker: "A", Adding: dollars(30)}}
		exiting := []Exposure{{Ticker: "A", Reducing: dollars(30)}}

		withAdding := AddingBudget("A", adding, 0, p)
		withExit := AddingBudget("A", exiting, 0, p)

		if withExit <= withAdding {
			t.Fatalf("$30 committed to the exit left %s of adding budget "+
				"against %s for the same $30 committed to adding: H-CAP-6 "+
				"says the concentration cap NEVER applies to a risk-reducing "+
				"order, and charging the exit against it is the probe's "+
				"defect reached by arithmetic (HR-005)", withExit, withAdding)
		}
		// Concretely: the exit leaves the per-market cap untouched, so the
		// budget is the full $33.33 share.
		if want := dollars(100.0 / 6 * 2); withExit != want {
			t.Errorf("AddingBudget with $30 of exit committed = %s, want %s",
				withExit, want)
		}

		var e Exposure
		e.Reducing = dollars(33)
		if e.Concentrated() != 0 {
			t.Errorf("Concentrated() counted %s of reducing collateral",
				e.Concentrated())
		}
		e.Adding, e.Position = dollars(2), dollars(3)
		if want := dollars(5); e.Concentrated() != want {
			t.Errorf("Concentrated() = %s, want %s: the position and the "+
				"adding orders are both risk TAKEN in this market",
				e.Concentrated(), want)
		}
	})

	t.Run("reducers are funded first, globally", func(t *testing.T) {
		// A quiet market wants to add; another market's exit needs $70.
		ex := []Exposure{{Ticker: "A"}, {Ticker: "B", Position: dollars(5)}}
		with := AddingBudget("A", ex, dollars(70), p)
		without := AddingBudget("A", ex, 0, p)
		if with >= without {
			t.Fatalf("a $70 reducer need did not reduce the adding budget "+
				"(%s vs %s): H-CAP-4 funds reducers before adding quotes "+
				"GLOBALLY, so an adding quote in a quiet market does not get "+
				"to consume the capital another market's exit is about to need",
				with, without)
		}
		if with < 0 {
			t.Errorf("AddingBudget = %s: a negative budget read as a size is "+
				"a short order", with)
		}
	})

	t.Run("never negative", func(t *testing.T) {
		ex := []Exposure{{Ticker: "A", Position: dollars(500)}}
		if got := AddingBudget("A", ex, dollars(500), p); got != 0 {
			t.Errorf("AddingBudget = %s, want 0", got)
		}
	})
}

// TestCheckFundableIsV1_12 is H-CAP-8 across §10.3's parameter space.
//
// "A configuration that cannot fund the reducer for the worst permitted
// simultaneous fill set is rejected at startup, checked as arithmetic, not
// discovered at fill time."
func TestCheckFundableIsV1_12(t *testing.T) {
	t.Run("the shipped configuration passes", func(t *testing.T) {
		if err := CheckFundable(cfg.Default()); err != nil {
			t.Fatalf("§10.3's opening configuration is not fundable: %v", err)
		}
	})

	// The lip-afr / HR-005 regression, exactly as it stood: S = 100 with a $500
	// capital_max. 6 · 100 · $0.99 = $594 against $375 deployable. The
	// recommended opening configuration was required to reject itself.
	t.Run("the withdrawn configuration is rejected", func(t *testing.T) {
		p := cfg.Default()
		p.CapitalMax = dollars(500)
		p.S = contracts(100)
		p.SMax = contracts(400)
		p.InvSoft, p.InvHard, p.InvKill = contracts(25), contracts(60), contracts(150)

		err := CheckFundable(p)
		if err == nil {
			t.Fatal("the pre-rescale configuration was accepted: 6 · 100 · " +
				"$0.99 = $594 cannot be funded from $500, and no " +
				"implementation could satisfy §10.3 and H-CAP-8 at once")
		}
		if !strings.Contains(err.Error(), "H-CAP-8") {
			t.Errorf("the rejection does not name the rule: %v", err)
		}
	})

	// The round-trip half. A configuration can fit the fill set inside the
	// deployable capital and still be unable to hold every position AND every
	// exit at once, which is the state H-CAP-8 actually names.
	t.Run("the round trip is checked separately", func(t *testing.T) {
		p := cfg.Default()
		p.CapitalReserve = 0 // everything deployable, so (a) cannot bind
		p.CapitalMax = dollars(70)
		p.S = contracts(12)
		// (a): 6 · 12 · $0.99 = $71.28 > $70 -- still binds. Shrink S.
		p.S = contracts(11) // (a): $65.34 <= $70 passes
		// (b): 6 · 11 · $1.00 = $66 <= $70 also passes. Tighten capital.
		p.CapitalMax = dollars(65.5) // (a): $65.34 <= $65.50 passes
		// (b): $66.00 > $65.50 fails.
		err := CheckFundable(p)
		if err == nil {
			t.Fatal("a configuration whose fill set fits but whose positions " +
				"plus exits do not was accepted")
		}
		if !strings.Contains(err.Error(), "its exits") {
			t.Errorf("the rejection is not the round-trip one: %v", err)
		}
	})

	// Sweep §10.3's own dimensions. Every accepted configuration must satisfy
	// both bounds; every rejected one must violate at least one.
	t.Run("sweep", func(t *testing.T) {
		var accepted, rejected int
		for _, nMarkets := range []int{1, 2, 6, 12} {
			for _, s := range []float64{1, 12, 48, 100} {
				for _, capital := range []float64{1, 100, 500} {
					for _, reserve := range []float64{0, 0.25, 0.5} {
						p := cfg.Default()
						p.NMarkets = nMarkets
						p.S = contracts(s)
						p.CapitalMax = dollars(capital)
						p.CapitalReserve = reserve

						err := CheckFundable(p)

						fills := num.Notional(p.S, MaxEntryPrice4) * num.Money(nMarkets)
						trip := num.Notional(p.S, SettlementPrice4) * num.Money(nMarkets)
						deployable := num.Money(float64(p.CapitalMax) * (1 - reserve))
						ok := fills <= deployable && trip <= p.CapitalMax

						if ok && err != nil {
							t.Fatalf("n=%d S=%v cap=%v reserve=%v was "+
								"rejected but satisfies both bounds "+
								"(fills %s <= %s, trip %s <= %s): %v",
								nMarkets, s, capital, reserve,
								fills, deployable, trip, p.CapitalMax, err)
						}
						if !ok && err == nil {
							t.Fatalf("n=%d S=%v cap=%v reserve=%v was "+
								"accepted but violates a bound (fills %s vs "+
								"%s, trip %s vs %s)",
								nMarkets, s, capital, reserve,
								fills, deployable, trip, p.CapitalMax)
						}
						if err == nil {
							accepted++
						} else {
							rejected++
						}
					}
				}
			}
		}
		if accepted == 0 || rejected == 0 {
			t.Fatalf("the sweep did not exercise both outcomes: %d accepted, "+
				"%d rejected", accepted, rejected)
		}
	})
}

// TestDeployedIsA7sMeasure.
func TestDeployedIsA7sMeasure(t *testing.T) {
	ex := []Exposure{
		{Ticker: "A", Adding: dollars(10), Reducing: dollars(5), Position: dollars(20)},
		{Ticker: "B", Adding: dollars(3)},
	}
	if got, want := Deployed(ex), dollars(38); got != want {
		t.Errorf("Deployed = %s, want %s -- A7 measures deployed capital at "+
			"every placement decision, and H-CAP-7 counts positions plus every "+
			"RESTING, SENDING and UNKNOWN order", got, want)
	}
	if got := Deployed(nil); got != 0 {
		t.Errorf("Deployed(nil) = %s, want 0", got)
	}
}

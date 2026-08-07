package quote

import (
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
)

// q returns a position in contracts as exact quanta, without going through
// num.QtyFromFloat -- the rounding rule under test elsewhere.
func qty(contracts float64) num.Qty {
	return num.Qty(int64(contracts*num.QtyScale + 0.5*sign(contracts)))
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

// TestReducingSideIsTheOppositeOfThePosition pins §6.2's R and A.
//
// The sign convention is YES-positive (§8.1), so a long YES position is reduced
// by BIDDING NO -- not by selling YES, which is the same trade under H-CO-1's
// transform but is the phrasing that invites an implementer to reach for a
// sell-side code path the harness does not have (H-Q-3: post_only, no taker
// path, no side that crosses).
//
// The q == 0 case is the one with teeth. There is no reducing side when flat,
// and a function that returned SideNo by default would give the caller a side
// to quote an exit on when no exit is needed -- and, worse, would make A4's
// "while q != 0" obligation look satisfiable by inspection of a flat market.
func TestReducingSideIsTheOppositeOfThePosition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		q      num.Qty
		reduce Side
		add    Side
		ok     bool
	}{
		{"long yes", qty(12), SideNo, SideYes, true},
		{"short yes", qty(-12), SideYes, SideNo, true},
		{"one quantum long", num.Qty(1), SideNo, SideYes, true},
		{"one quantum short", num.Qty(-1), SideYes, SideNo, true},
		{"flat", 0, SideYes, SideNo, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := ReducingSide(tc.q)
			if ok != tc.ok {
				t.Fatalf("ReducingSide(%s) reported ok = %v, want %v -- at "+
					"q == 0 there is no exit to size and A4 imposes no "+
					"obligation", tc.q.Wire(), ok, tc.ok)
			}
			a, aok := AddingSide(tc.q)
			if aok != tc.ok {
				t.Fatalf("AddingSide(%s) reported ok = %v, want %v",
					tc.q.Wire(), aok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if r != tc.reduce {
				t.Errorf("ReducingSide(%s) = %s, want %s: a long YES position "+
					"is reduced by bidding NO", tc.q.Wire(), r, tc.reduce)
			}
			if a != tc.add {
				t.Errorf("AddingSide(%s) = %s, want %s", tc.q.Wire(), a, tc.add)
			}
			if r == a {
				t.Errorf("both sides came back as %s -- the adding side is "+
					"defined as the opposite of the reducing one", r)
			}
		})
	}
}

// TestSizeATaperIsContinuousAndMonotone is V1.3's core: size_A across the whole
// q domain, one quantum at a time, both signs.
//
// "The taper is linear and continuous so a single contract never flips the
// whole quote on and off. A cliff produces order churn against the rate limit
// for no scoring benefit."
//
// A sweep at contract granularity would step over the defect: at S = 12 with a
// 4-contract taper band, a quantized taper moves ~3 contracts per contract of
// inventory, so any discontinuity smaller than that hides between samples. The
// sweep is therefore per QUANTUM, and the bound on the step is one quantum more
// than the ideal slope -- the most a correctly floored linear function can
// move between adjacent inputs.
func TestSizeATaperIsContinuousAndMonotone(t *testing.T) {
	p := cfg.Default()

	// Ideal slope in quanta of size per quantum of |q|, from the §6.2 formula.
	slope := float64(p.S) / float64(p.InvHard-p.InvSoft)
	maxStep := num.Qty(int64(slope) + 2) // floor(slope) + rounding + 1

	for sgn := range 2 {
		mul := num.Qty(1)
		name := "long"
		if sgn == 1 {
			mul, name = -1, "short"
		}
		t.Run(name, func(t *testing.T) {
			prev := SizeA(0, p)
			// One quantum past inv_hard, so the far branch is covered.
			for a := num.Qty(0); a <= p.InvHard+p.S; a++ {
				got := SizeA(a*mul, p)

				if got < 0 || got > p.S {
					t.Fatalf("size_A(%s) = %s, outside [0, S = %s]",
						(a * mul).Wire(), got.Wire(), p.S.Wire())
				}
				if got > prev {
					t.Fatalf("size_A(%s) = %s rose from %s: the taper must be "+
						"non-increasing in |q| -- an adding quote that grows "+
						"with inventory is the sign error the whole mechanism "+
						"turns on", (a * mul).Wire(), got.Wire(), prev.Wire())
				}
				if step := prev - got; step > maxStep {
					t.Fatalf("size_A dropped %s between |q| = %s and %s "+
						"(max %s): that is a cliff, and §6.2 requires a single "+
						"contract never to flip the whole quote on and off",
						step.Wire(), (a - 1).Wire(), a.Wire(), maxStep.Wire())
				}
				prev = got
			}
		})
	}
}

// TestSizeAPinsTheBranchBoundaries names every value §6.2 states outright,
// including the fractional q V1.3 asks for by name.
//
// The boundaries are where the three branches meet and where an off-by-one
// comparison lives. `|q| <= inv_soft` gives S and `|q| > inv_hard` gives 0, so
// both are inclusive-on-the-left: at exactly inv_soft the quote is still full,
// and at exactly inv_hard it is exactly zero rather than "nearly zero" or
// "already off".
func TestSizeAPinsTheBranchBoundaries(t *testing.T) {
	p := cfg.Default() // S = 12, inv_soft = 3, inv_hard = 7

	for _, tc := range []struct {
		q    num.Qty
		want num.Qty
		why  string
	}{
		{0, p.S, "flat quotes the full size"},
		{qty(3), p.S, "at exactly inv_soft the taper has not begun: §6.2's " +
			"first branch is |q| <= inv_soft"},
		{qty(-3), p.S, "and the taper is a function of |q|, so it is symmetric"},
		{qty(7), 0, "at exactly inv_hard the taper has reached zero: " +
			"S·(inv_hard − |q|)/(inv_hard − inv_soft) = 0"},
		{qty(-7), 0, "symmetric at the far boundary too"},
		{qty(8), 0, "and stays at zero beyond it, which is also where §5.2 " +
			"sends the market to REDUCING"},
		{qty(5), qty(6), "the midpoint of a 3→7 band is half of S = 12"},
		{qty(-5), qty(6), "symmetric at the midpoint"},

		// The fractional cases V1.3 names. Sizes are fractional in ~20.5% of
		// resting levels (H-CO-4), so a q of 3.01 is ordinary, and it is the
		// first input for which the taper is neither S nor a round number.
		{qty(3.01), num.Qty(1197), "one quantum past inv_soft: " +
			"12·(7 − 3.01)/4 = 11.97, and the taper must engage there rather " +
			"than waiting for a whole contract"},
		{qty(6.99), num.Qty(3), "one quantum short of inv_hard: 12·0.01/4 = " +
			"0.03. Still strictly positive -- the last quantum before the " +
			"cliff is a real quote, which is what makes the taper continuous"},
		{qty(0.07), p.S, "a 0.07 partial fill is well inside inv_soft and " +
			"changes nothing about the quote"},
	} {
		if got := SizeA(tc.q, p); got != tc.want {
			t.Errorf("size_A(%s) = %s, want %s -- %s",
				tc.q.Wire(), got.Wire(), tc.want.Wire(), tc.why)
		}
	}
}

// TestSizeAFloorsRatherThanRounds pins the rounding DIRECTION.
//
// The taper is real-valued and the quote is quantized, so some rounding is
// forced. Flooring is the conservative direction: size_A is the risk-ADDING
// size, and rounding up places a fraction of a contract more exposure than the
// taper asked for, at exactly the inventory levels where the taper exists to be
// removing exposure. The magnitude is a hundredth of a contract, so this is
// pinned for its direction, not its size.
func TestSizeAFloorsRatherThanRounds(t *testing.T) {
	p := cfg.Default()

	// 12·(7 − 6.98)/4 = 0.06 exactly -- a case with no residue, to establish
	// the arithmetic before testing what happens to one.
	if got, want := SizeA(qty(6.98), p), num.Qty(6); got != want {
		t.Fatalf("size_A(6.98) = %s, want %s", got.Wire(), want.Wire())
	}

	// Choose a band whose division does not come out even: S = 10, band of 3.
	// At |q| = 4.00: 10·(6 − 4)/3 = 6.666... contracts = 666.66 quanta.
	p.S, p.InvSoft, p.InvHard = qty(10), qty(3), qty(6)
	got := SizeA(qty(4), p)
	if want := num.Qty(666); got != want {
		t.Fatalf("size_A = %s (%d quanta), want %s (%d): 10·(6−4)/3 is "+
			"6.6666 contracts and the quote must be FLOORED to the quantum, "+
			"never rounded up -- the taper is removing exposure and must not "+
			"add back a fraction of a contract of it",
			got.Wire(), int64(got), want.Wire(), int64(want))
	}
}

// TestSizeRNeverOvershootsFlat is H-Q-5a / A12, driven over the whole domain.
//
// "There is no q and no fill sequence for which a reducing order can change the
// sign of q." The strong form is what is checked: for every position in the
// sweep, a FULL fill of the sized reducer must leave the position with the same
// sign or exactly flat, never the opposite sign.
//
// This is the HR-003 regression. The withdrawn |q| + S sizing passed every
// bound anyone had written -- it was under S_max, it was funded, and it was a
// "reducing" order by name. What it was not was capped at |q|, and with
// S > inv_hard the designed success case re-entered the failure state: q = +61
// posted 161, a full fill gave −100, above inv_hard, immediately back in
// REDUCING now posting 200, alternating indefinitely.
func TestSizeRNeverOvershootsFlat(t *testing.T) {
	p := cfg.Default()
	funded := qty(1000) // not the binding term here

	for a := num.Qty(0); a <= p.SMax+p.S; a++ {
		for _, q := range []num.Qty{a, -a} {
			r := SizeR(q, p.SMax, funded)

			if r < 0 {
				t.Fatalf("size_R(%s) = %s: a negative size is not a quote",
					q.Wire(), r.Wire())
			}
			if r > q.Abs() {
				t.Fatalf("size_R(%s) = %s exceeds |q| = %s -- H-Q-5a caps the "+
					"reducer at |q| and A12 states it as a running invariant",
					q.Wire(), r.Wire(), q.Abs().Wire())
			}
			if r > p.SMax {
				t.Fatalf("size_R(%s) = %s exceeds S_max = %s",
					q.Wire(), r.Wire(), p.SMax.Wire())
			}

			// A full fill moves the position toward zero by exactly r.
			after := q - num.Qty(q.Sign())*r
			if q.Sign() != 0 && after.Sign() == -q.Sign() {
				t.Fatalf("a full fill of a %s reducer took q from %s to %s: "+
					"the reducer changed the sign of q, which is the HR-003 "+
					"−100/+100 cycle", r.Wire(), q.Wire(), after.Wire())
			}
			// And a partial fill strictly decreases |q| whenever there is
			// anything to reduce.
			if q.Sign() != 0 && r > 0 {
				partial := q - num.Qty(q.Sign())
				if partial.Abs() >= q.Abs() {
					t.Fatalf("a one-quantum fill of a %s reducer left |q| at "+
						"%s from %s: every positive partial fill must "+
						"STRICTLY decrease |q|",
						r.Wire(), partial.Abs().Wire(), q.Abs().Wire())
				}
			}
		}
	}
}

// TestSizeRTakesTheBindingCap pins which of the three terms wins, and what an
// unfundable exit produces.
func TestSizeRTakesTheBindingCap(t *testing.T) {
	p := cfg.Default() // S_max = 48

	for _, tc := range []struct {
		name   string
		q      num.Qty
		funded num.Qty
		want   num.Qty
		why    string
	}{
		{"position binds", qty(10), qty(1000), qty(10),
			"an exit larger than the position is an adding quote wearing the " +
				"reducer's name (H-Q-5a)"},
		{"s_max binds", qty(200), qty(1000), p.SMax,
			"S_max is an aggregate per-side cap and applies to the exit too"},
		{"funding binds", qty(200), qty(20), qty(20),
			"H-CAP-4 funds reducers before adding quotes, but it cannot " +
				"create collateral"},
		{"unfundable", qty(200), 0, 0,
			"an unfundable reducer is not placed, and A4 explicitly does not " +
				"accept an unfundable intent as satisfying the obligation to " +
				"have an exit (H-CAP-6)"},
		{"negative funding", qty(200), qty(-5), 0,
			"already over the cap: the answer is zero, not a negative size"},
		{"flat", 0, qty(1000), 0,
			"nothing to reduce"},
		{"fractional position", qty(0.07), qty(1000), qty(0.07),
			"a 0.07 remainder is exited at 0.07, and H-CO-4b permits the " +
				"count because it does not exceed the position it derives from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SizeR(tc.q, p.SMax, tc.funded); got != tc.want {
				t.Fatalf("size_R(q=%s, S_max=%s, funded=%s) = %s, want %s -- %s",
					tc.q.Wire(), p.SMax.Wire(), tc.funded.Wire(),
					got.Wire(), tc.want.Wire(), tc.why)
			}
			// The short side is the mirror image, always.
			if got := SizeR(-tc.q, p.SMax, tc.funded); got != tc.want {
				t.Fatalf("size_R is not symmetric in the sign of q: got %s at "+
					"q = %s against %s at q = %s",
					got.Wire(), (-tc.q).Wire(), tc.want.Wire(), tc.q.Wire())
			}
		})
	}
}

// TestSizesForMatchesTheStateTable walks §5.2's table row by row.
//
// The two rows that carry the safety content are REDUCING and SETTLING, and
// what they assert is a distinction the type makes rather than a number:
// HasAdd is FALSE, not Add == 0. A8 requires that no market in REDUCING has an
// adding-side order resting or in flight -- an obligation to have cancelled
// something, exchange-confirmed (H-FAIL-3). A caller reading Add == 0 as "place
// nothing more" would satisfy the arithmetic and leave the order resting, which
// is the state A8 exists to forbid.
func TestSizesForMatchesTheStateTable(t *testing.T) {
	p := cfg.Default()
	funded := qty(1000)

	t.Run("idle and closed quote nothing", func(t *testing.T) {
		for _, st := range []MarketState{Idle, Closed} {
			s := SizesFor(SizeInput{State: st, Q: qty(5), Funded: funded}, p)
			if s.HasAdd || s.HasReduce {
				t.Errorf("%s produced a quote: %+v", st, s)
			}
		}
	})

	t.Run("quoting is symmetric and designates no reducer", func(t *testing.T) {
		// q = +2 is inside inv_soft = 3, so the market is QUOTING and §5.2
		// gives "size S at touch" in BOTH columns.
		s := SizesFor(SizeInput{State: Quoting, Q: qty(2), Funded: funded}, p)
		if !s.Symmetric {
			t.Error("QUOTING did not report itself symmetric")
		}
		if !s.HasAdd || !s.HasReduce {
			t.Fatalf("QUOTING must rest a quote on both sides: %+v", s)
		}
		if s.Add != p.S || s.Reduce != p.S {
			t.Fatalf("QUOTING sized %s / %s, want S = %s on both sides. "+
				"Applying size_R's |q| cap here would quote %s against a "+
				"2-contract position and stop two-sided market making at any "+
				"nonzero q -- inside the very band inv_soft exists to call "+
				"negligible. H-Q-5a and A12 are scoped to a REDUCER, and "+
				"QUOTING designates none",
				s.Add.Wire(), s.Reduce.Wire(), p.S.Wire(), qty(2).Wire())
		}
		// Flat is symmetric too, and names both sides without asserting an
		// asymmetry that does not exist.
		flat := SizesFor(SizeInput{State: Quoting, Q: 0, Funded: funded}, p)
		if flat.Add != p.S || flat.Reduce != p.S || flat.AddSide == flat.ReduceSide {
			t.Fatalf("flat QUOTING: %+v", flat)
		}
	})

	t.Run("skewed tapers the adding side and caps the exit", func(t *testing.T) {
		q := qty(5) // inv_soft < 5 < inv_hard
		s := SizesFor(SizeInput{State: Skewed, Q: q, Funded: funded}, p)
		if !s.HasAdd || s.Add != SizeA(q, p) {
			t.Errorf("SKEWED adding side = %s, want the taper %s",
				s.Add.Wire(), SizeA(q, p).Wire())
		}
		if !s.HasReduce || s.Reduce != SizeR(q, p.SMax, funded) {
			t.Errorf("SKEWED reducing side = %s, want %s",
				s.Reduce.Wire(), SizeR(q, p.SMax, funded).Wire())
		}
		if s.AddSide != SideYes || s.ReduceSide != SideNo {
			t.Errorf("a long YES position must add YES and reduce NO, got "+
				"add %s / reduce %s", s.AddSide, s.ReduceSide)
		}
		if s.Symmetric {
			t.Error("SKEWED reported itself symmetric")
		}
	})

	t.Run("reducing and settling have no adding side at all", func(t *testing.T) {
		for _, st := range []MarketState{Reducing, Settling} {
			q := qty(10)
			s := SizesFor(SizeInput{State: st, Q: q, Funded: funded}, p)
			if s.HasAdd {
				t.Errorf("%s offered an adding side (%s): §5.2 says the "+
					"adding side is CANCELLED and exchange-confirmed absent, "+
					"and A8 forbids one resting or in flight", st, s.Add.Wire())
			}
			if s.Add != 0 {
				t.Errorf("%s sized the adding side at %s", st, s.Add.Wire())
			}
			if !s.HasReduce || s.Reduce != SizeR(q, p.SMax, funded) {
				t.Errorf("%s must keep the capped exit: got %s, want %s. "+
					"M13 -- cancel everything on entry to SETTLING and never "+
					"place the capped reducer -- is probebot.py's exact defect "+
					"confined to the close window",
					st, s.Reduce.Wire(), SizeR(q, p.SMax, funded).Wire())
			}
		}
	})

	t.Run("flat in reducing has no exit to place", func(t *testing.T) {
		s := SizesFor(SizeInput{State: Reducing, Q: 0, Funded: funded}, p)
		if s.HasReduce || s.HasAdd {
			t.Errorf("a flat REDUCING market quoted something: %+v -- q == 0 "+
				"is §5.2's exit edge back to IDLE", s)
		}
	})

	// The aggregate reducing size never exceeds |q| in any state that has one.
	// This is A12 stated over the state machine rather than over SizeR alone,
	// because the state table is where a future edit would reintroduce it.
	t.Run("no state can size an exit past the position", func(t *testing.T) {
		for _, st := range []MarketState{Idle, Quoting, Skewed, Reducing, Settling, Closed} {
			for a := num.Qty(0); a <= p.SMax; a += 7 {
				for _, q := range []num.Qty{a, -a} {
					s := SizesFor(SizeInput{State: st, Q: q, Funded: funded}, p)
					if !s.HasReduce || s.Symmetric {
						continue // QUOTING is symmetric and designates no reducer
					}
					if s.Reduce > q.Abs() {
						t.Fatalf("%s sized the exit at %s against q = %s (A12)",
							st, s.Reduce.Wire(), q.Wire())
					}
				}
			}
		}
	})
}

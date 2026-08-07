package quote

import (
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
)

// TestCrossesIsExhaustiveOverEveryPricePair is V1.4: no (yes, no) price pair
// the harness can emit sums to >= 100.
//
// Every pair in the exchange's whole tradable range, both orders of placement,
// against an independently computed oracle. The oracle is the arithmetic
// H-CO-1 makes the pair mean rather than a restatement of the predicate: a NO
// bid at `no` cents IS a YES ask at `100 − no` cents, so the pair crosses
// exactly when our YES bid reaches our own YES ask.
//
// 99x99 is small enough to enumerate, and enumerating it is the point. A
// boundary rule that is right in the middle of its domain and wrong at 50/50 is
// the failure mode here, and 50/50 is the single most likely pair the harness
// will ever hold -- a market trading at even odds, quoted at the touch on both
// sides.
func TestCrossesIsExhaustiveOverEveryPricePair(t *testing.T) {
	for yes := MinPrice; yes <= MaxPrice; yes++ {
		for no := MinPrice; no <= MaxPrice; no++ {
			// H-CO-1: our NO bid at `no` is a YES ask at 100 − no. The pair
			// crosses when our bid meets or passes our own ask.
			yesAsk := 100 - no
			want := yes >= yesAsk

			if got := Crosses(yes, no); got != want {
				t.Fatalf("Crosses(yes=%dc, no=%dc) = %v, want %v: a NO bid at "+
					"%dc is a YES ask at %dc (H-CO-1), so a YES bid at %dc "+
					"%s it", yes, no, got, want, no, yesAsk, yes,
					map[bool]string{true: "meets or passes", false: "stays below"}[want])
			}

			// The predicate is symmetric in its two arguments, because the sum
			// is. A version that special-cased one side would pass every test
			// written from that side's point of view.
			if Crosses(no, yes) != Crosses(yes, no) {
				t.Fatalf("Crosses is asymmetric at (%d, %d)", yes, no)
			}

			// CheckPlacement agrees, from both directions of placement: the
			// answer cannot depend on which leg we happen to be sending.
			errYes := CheckPlacement(SideYes, yes, no, true)
			errNo := CheckPlacement(SideNo, no, yes, true)
			if (errYes != nil) != want || (errNo != nil) != want {
				t.Fatalf("CheckPlacement disagrees with Crosses at "+
					"(yes=%dc, no=%dc): placing yes gave %v, placing no gave "+
					"%v, want cross = %v", yes, no, errYes, errNo, want)
			}
		}
	}

	// The boundary, named. 50/50 is the pair a market at even odds produces,
	// quoted at the touch on both sides under H-Q-1, and it is a cross.
	if !Crosses(50, 50) {
		t.Error("50c/50c is not reported as a cross: the pair sums to exactly " +
			"100, which is the full settlement value, and A3 requires the sum " +
			"to be strictly under it")
	}
	if Crosses(50, 49) {
		t.Error("50c/49c is reported as a cross: 99 < 100, and this is the " +
			"tightest legal two-sided quote there is -- rejecting it would " +
			"stop the harness quoting the markets it most wants")
	}
}

// TestCheckPlacementRejectsUntradablePrices pins the range.
//
// A price of 0 or 100 is not merely uneconomic, it is unplaceable, and a guard
// that only checked the sum would pass a YES bid at 0c against nothing resting
// -- sum 0, no cross -- and send an order the exchange rejects. A rejected
// placement is a presence gap (S4), and on a reducing side it is a missing exit.
func TestCheckPlacementRejectsUntradablePrices(t *testing.T) {
	for _, p := range []int{-1, 0, 100, 101} {
		if ValidPrice(p) {
			t.Errorf("ValidPrice(%d) = true, want false", p)
		}
		if err := CheckPlacement(SideYes, p, 0, false); err == nil {
			t.Errorf("CheckPlacement accepted a %dc placement", p)
		}
	}
	for _, p := range []int{MinPrice, 50, MaxPrice} {
		if !ValidPrice(p) {
			t.Errorf("ValidPrice(%d) = false, want true", p)
		}
		if err := CheckPlacement(SideYes, p, 0, false); err != nil {
			t.Errorf("CheckPlacement rejected a %dc placement: %v", p, err)
		}
	}
}

// TestCheckPlacementWithNothingRestingOpposite is the case a zero value would
// silently answer wrongly.
//
// "Check ... against our own current resting state." When we rest nothing on
// the other side there is no pair, so there is no cross, and every price in
// range is placeable. A signature that took a bare int would represent that
// state as 0 -- and 0 passes the sum test for every price, which happens to
// give the right answer here and the wrong one the moment a real 0 is ever
// meaningful. The boolean makes the absence explicit rather than lucky.
func TestCheckPlacementWithNothingRestingOpposite(t *testing.T) {
	for p := MinPrice; p <= MaxPrice; p++ {
		for _, side := range []Side{SideYes, SideNo} {
			if err := CheckPlacement(side, p, 0, false); err != nil {
				t.Fatalf("CheckPlacement(%s, %dc) with nothing resting "+
					"opposite was rejected: %v -- there is no pair to cross",
					side, p, err)
			}
		}
	}
}

// TestMaxOppositeIsTheLargestNonCrossingPrice checks the solved form against
// the predicate it derives from, exhaustively.
func TestMaxOppositeIsTheLargestNonCrossingPrice(t *testing.T) {
	// At a resting price of 99c there is no placeable price on the other side
	// at all: the cheapest bid the exchange accepts is 1c, and 1 + 99 = 100.
	// That is a real state -- a market trading at near-certainty, quoted at the
	// touch -- and the answer is "quote nothing on this side", not "quote 0c".
	if _, ok := MaxOpposite(MaxPrice); ok {
		t.Errorf("MaxOpposite(%dc) offered a placeable price, but every bid "+
			"from 1c up sums to at least 100 against it", MaxPrice)
	}

	for other := MinPrice; other < MaxPrice; other++ {
		max, ok := MaxOpposite(other)
		if !ok {
			t.Fatalf("MaxOpposite(%dc) reported no placeable price, but a "+
				"1c bid sums to %dc, which is under 100", other, other+1)
		}
		if Crosses(max, other) {
			t.Errorf("MaxOpposite(%dc) = %dc, which crosses", other, max)
		}
		if max < MaxPrice && !Crosses(max+1, other) {
			t.Errorf("MaxOpposite(%dc) = %dc, but %dc does not cross either "+
				"-- the clamp is one tick tighter than it needs to be, and "+
				"every tick back halves the score at DF = 0.5 (H-Q-1)",
				other, max, max+1)
		}
		if !ValidPrice(max) {
			t.Errorf("MaxOpposite(%dc) = %dc, outside the tradable range",
				other, max)
		}
	}
}

// TestSkewCannotEmitACrossingPair is V1.4 read as it is written: "no (yes, no)
// price pair the SKEW FUNCTION can emit sums to >= 100".
//
// The skew function chooses sizes; the prices come from the external touch on
// each side (H-Q-1, H-Q-10). So the pair the harness emits is
// (external_best(yes), external_best(no)) -- and the question V1.4 is really
// asking is whether a book can hand us a crossing pair of touches, and what
// happens when it does.
//
// It can. A book whose two bid sides sum to 100 or more is not malformed, it is
// a crossed or locked market, and it occurs. The guarantee therefore cannot be
// "the skew function never emits such a pair"; it has to be "the placement path
// refuses it". That is what is checked here, over every book pair, in every
// market state that quotes anything.
func TestSkewCannotEmitACrossingPair(t *testing.T) {
	p := cfg.Default()
	states := []MarketState{Quoting, Skewed, Reducing, Settling}
	positions := []num.Qty{0, qty(2), qty(-2), qty(5), qty(-5), qty(10), qty(-10)}

	var placed, refused int
	for yesTouch := MinPrice; yesTouch <= MaxPrice; yesTouch++ {
		for noTouch := MinPrice; noTouch <= MaxPrice; noTouch++ {
			for _, st := range states {
				for _, q := range positions {
					s := SizesFor(SizeInput{State: st, Q: q, Funded: qty(1000)}, p)

					// The harness quotes each side at its own touch. Whichever
					// leg is dispatched second sees the first already resting.
					yesResting, noResting := false, false
					if s.HasAdd && s.Add > 0 {
						if s.AddSide == SideYes {
							yesResting = true
						} else {
							noResting = true
						}
					}
					if s.HasReduce && s.Reduce > 0 {
						if s.ReduceSide == SideYes {
							yesResting = true
						} else {
							noResting = true
						}
					}
					if !yesResting || !noResting {
						continue // only one side quoted: no pair to cross
					}

					err := CheckPlacement(SideNo, noTouch, yesTouch, true)
					if Crosses(yesTouch, noTouch) {
						if err == nil {
							t.Fatalf("%s at q = %s placed yes %dc / no %dc, "+
								"which sums to %dc: a self-cross is reachable "+
								"from the state machine (H-CO-6, A3)",
								st, q.Wire(), yesTouch, noTouch,
								yesTouch+noTouch)
						}
						refused++
					} else {
						if err != nil {
							t.Fatalf("%s at q = %s refused a legal pair "+
								"yes %dc / no %dc (sum %dc): %v",
								st, q.Wire(), yesTouch, noTouch,
								yesTouch+noTouch, err)
						}
						placed++
					}
				}
			}
		}
	}

	// Both arms were actually exercised. A sweep in which every pair took one
	// branch would pass while proving only half of what it claims.
	if placed == 0 || refused == 0 {
		t.Fatalf("the sweep did not exercise both outcomes: %d placed, "+
			"%d refused", placed, refused)
	}
}

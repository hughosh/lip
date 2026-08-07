package num

import (
	"fmt"
	"math"
	"testing"
)

// TestQtyIsFlatIsExact is the H-CO-4a / HR-026 regression.
//
// Sizes are fractional in ~20.5% of observed resting book levels, so this is
// reachable rather than theoretical. Accumulated as float64, fills of 0.10,
// 0.20 and -0.30 leave a binary residue near 5.6e-17: the market is
// economically flat, but `q != 0` is TRUE. A float equality test was gating
// three lifecycle transitions -- REDUCING never reaching IDLE, SIGTERM never
// seeing a drained market, and SETTLING formatting |q| as "0.00" and rejecting
// in a loop.
func TestQtyIsFlatIsExact(t *testing.T) {
	// The exact sequence from HR-026, accumulated the way a fill stream
	// arrives. As float64 this does not reach zero.
	var f float64
	for _, x := range []float64{0.10, 0.20, -0.30} {
		f += x
	}
	if f == 0 {
		t.Fatalf("premise broken: float64 accumulation gave exact 0, "+
			"got %v -- this test no longer reproduces HR-026", f)
	}
	if math.Abs(f) > 1e-9 {
		t.Fatalf("premise broken: residue %v is larger than expected", f)
	}

	// Accumulated in the exchange's own quantum it reaches exactly zero.
	var q Qty
	for _, x := range []float64{0.10, 0.20, -0.30} {
		q += QtyFromFloat(x)
	}
	if !q.IsFlat() {
		t.Fatalf("q = %d (%s) is not flat after a net-zero fill sequence; "+
			"REDUCING would never reach IDLE and SIGTERM would never drain",
			int64(q), q.Wire())
	}
	if q.Wire() != "0.00" {
		t.Fatalf("flat position formats as %q, want \"0.00\"", q.Wire())
	}
}

// TestQtyIsFlatRejectsNonzero pins the other half of H-CO-4a: IsFlat means
// EXACTLY zero, in both directions.
//
// TestQtyIsFlatIsExact proves flat is reachable -- a net-zero fill sequence
// lands on true. It says nothing about the converse, so an IsFlat that answered
// true for a small nonzero position would still pass it. That direction is the
// dangerous one: §5.2 lets REDUCING leave for IDLE only at q == 0, and H-HALT-3
// lets the process exit only when every market is flat or closed. An IsFlat
// that is generous about zero does not stall the harness -- it abandons an open
// position, deselects the market that still holds it, and lets SIGTERM report a
// drain that never happened.
//
// The table is in raw Qty, not QtyFromFloat, so the boundary is not obscured by
// a float conversion on the way in: Qty(1) is exactly one quantum, one
// hundredth of a contract, the smallest long the exchange can express, and
// Qty(-1) is its symmetric short.
func TestQtyIsFlatRejectsNonzero(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    Qty
		want bool
		why  string
	}{
		{
			name: "exact zero",
			q:    Qty(0),
			want: true,
			why:  "§5.2: REDUCING reaches IDLE at q == 0",
		},
		{
			name: "smallest long",
			q:    Qty(1),
			want: false,
			why: "a long of one quantum (0.01) is a position; calling it " +
				"flat lets REDUCING leave for IDLE and lets SIGTERM " +
				"report a drain, both while still holding inventory",
		},
		{
			name: "smallest short",
			q:    Qty(-1),
			want: false,
			why: "a short of one quantum (-0.01) is a position; the " +
				"boundary is symmetric about zero and neither sign is flat",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.IsFlat(); got != tc.want {
				t.Fatalf("Qty(%d).IsFlat() = %v, want %v (%s = %q) -- %s",
					int64(tc.q), got, tc.want, tc.name, tc.q.Wire(), tc.why)
			}
		})
	}
}

// reducingSide is §6.2's reducing-side rule written out in full: `R` is "no" if
// q > 0, "yes" if q < 0, and none if q == 0.
//
// It lives here rather than being imported because harness/num is a leaf and
// the quoting layer sits above it. What it pins is the shape of the dependency:
// the selection consumes nothing but Qty.Sign(). Every fact §6.2 needs to
// choose a side has already been reduced to that one integer by the time the
// rule runs, so a Sign that reports a live position as signless does not
// mis-select a side -- it leaves the harness with no side to quote at all.
func reducingSide(q Qty) string {
	switch q.Sign() {
	case 1:
		return "no"
	case -1:
		return "yes"
	}
	return ""
}

// TestQtySignPreservesOneQuantumPositions pins the exact zero boundary of
// Qty.Sign, the other half of the H-CO-4a quantum from
// TestQtyIsFlatRejectsNonzero.
//
// That test proves IsFlat is exactly zero in both directions. Sign is a
// separate function with a separate boundary, and it is the one §6.2 reads:
// the reducing side is "no" when q > 0, "yes" when q < 0, and none only when
// q == 0. Nothing above currently constrains it, so a Sign widened to answer 0
// for a small nonzero position passes the whole existing suite.
//
// That defect is worse than a mis-signed reducer, because it does not produce a
// wrong side -- it produces no side. A4 requires that while q != 0 in any
// market, that market has a reducing quote resting or an in-flight intent to
// place one. A signless +0.01 satisfies no branch of §6.2, so no reducer is
// sized and none is placed, while IsFlat -- still exact -- keeps reporting the
// market as holding inventory. The two functions disagree about the same
// position, and the disagreement is load-bearing: H-HALT-3 says SIGTERM does
// not exit but sets WINDING_DOWN and stays alive until every market is flat or
// closed. A market that is not flat and has no reducing side cannot drain, so
// WINDING_DOWN never ends and drain_timeout_h escalates SEV1 forever against a
// position of one hundredth of a contract.
//
// The table is raw Qty literals, not QtyFromFloat, ParseQty, Wire or an
// epsilon, so the boundary is asserted against the quantum itself and the test
// shares no conversion rule with production: Qty(1) is exactly one quantum,
// +0.01 contracts, the smallest long the exchange can express, and Qty(-1) is
// its symmetric short. V1.3 asks for size_R across the whole q domain; this is
// its lower edge, where |q| is one quantum and the sign is all that is left.
func TestQtySignPreservesOneQuantumPositions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		q        Qty
		want     int
		wantSide string
		why      string
	}{
		{
			name:     "exact zero",
			q:        Qty(0),
			want:     0,
			wantSide: "",
			why: "§6.2 selects no reducing side only at q == 0, and zero is " +
				"the sole value for which that is correct",
		},
		{
			name:     "smallest long",
			q:        Qty(1),
			want:     1,
			wantSide: "no",
			why: "a long of one quantum (+0.01) is actionable inventory: " +
				"§6.2 must select \"no\" as the reducer. Reporting it " +
				"signless places no reducer at all (A4) against a position " +
				"IsFlat still calls open, so WINDING_DOWN cannot drain " +
				"(H-HALT-3)",
		},
		{
			name:     "smallest short",
			q:        Qty(-1),
			want:     -1,
			wantSide: "yes",
			why: "a short of one quantum (-0.01) is the symmetric case: " +
				"§6.2 must select \"yes\". The quantum is symmetric about " +
				"zero (H-CO-4a) and a widened boundary strands either sign",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.Sign(); got != tc.want {
				t.Fatalf("Qty(%d).Sign() = %d, want %d (%s = %q) -- %s",
					int64(tc.q), got, tc.want, tc.name, tc.q.Wire(), tc.why)
			}

			// Sign and IsFlat must agree about the same position. Exactly one
			// value is both flat and signless; every other q is both non-flat
			// and signed, and a q that is one but not the other is the state
			// no part of §5.2, §6.2 or H-HALT-3 has a transition out of.
			wantFlat := tc.want == 0
			if got := tc.q.IsFlat(); got != wantFlat {
				t.Fatalf("Qty(%d).IsFlat() = %v but Sign() = %d: the two zero "+
					"tests disagree about the same position. A market that is "+
					"not flat and has no sign holds inventory no reducing "+
					"side can be selected for (§6.2), so A4 places nothing "+
					"and H-HALT-3's drain never completes",
					int64(tc.q), got, tc.want)
			}

			// And the sign is consumed the way §6.2 consumes it.
			if got := reducingSide(tc.q); got != tc.wantSide {
				t.Fatalf("reducing side for q = %s is %q, want %q -- §6.2 "+
					"selects \"no\" when q > 0, \"yes\" when q < 0, and none "+
					"only when q == 0: %s",
					tc.q.Wire(), got, tc.wantSide, tc.why)
			}
		})
	}
}

// TestQtySignPreservesDeployedFullFillPositions carries the H-CO-4a sign
// boundary off the quantum and onto the position §10.3 actually deploys.
//
// TestQtySignPreservesOneQuantumPositions pins the lower edge of V1.3's `q`
// domain, where |q| is a single quantum and the sign is all that is left. That
// edge is where a widened zero test bites, but it is not where the harness
// spends its time. §10.3 posts S = 12 contracts a side in each of six markets,
// and V2-FILL enumerates a full fill as one of the legal outcomes of a print
// against a resting order of ours -- it is not the assumed outcome, but it is an
// admissible one, so it cannot be dismissed as unreachable. A base quote taken
// from flat therefore lands on exactly ±12.00 contracts, which under H-CO-4a's
// contracts × 100 is Qty(±1200). That, not one hundredth, is the ordinary
// position the reducer is sized against: size_R = min(|q|, S_max, funded) with
// S_max = 4·S = 48 contracts leaves |q| the binding term all the way up the
// deployed ladder.
//
// The failure excluded here is a Sign that is right at the quantum and wrong at
// the working point -- signless, or reversed. §8.1 makes `q` YES-positive, so
// §6.2 must answer "no" for +1200 and "yes" for -1200:
//
//   - Signless is A4's failure. No branch of §6.2 selects a side, so no reducer
//     is sized and none is placed against a position IsFlat still reports open.
//   - Reversed is worse, because it is silent. It selects the ADDING side as the
//     reducer, and every downstream rule then reads as satisfied while the
//     position runs the wrong way: A8 forbids an adding-side order in REDUCING
//     and this is one by construction; H-Q-5a's cap is on |q|, which a mis-signed
//     reducer of exactly |q| respects; and A12's "no fill sequence can change the
//     sign of q" is broken by the first full fill, which takes +12.00 to +24.00
//     rather than to flat -- past inv_hard = 7 and clear through inv_kill = 18.
//
// The expected sign, the expected side and the reducer's magnitude are all table
// literals, so this test's oracle shares no quantizer, parser or sign rule with
// production. Only the DIRECTION of the fill is taken from Sign, routed through
// the same reducingSide helper §6.2's rule is written out in, because that is
// the thing under test: the side selection consumes nothing else.
func TestQtySignPreservesDeployedFullFillPositions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		q        Qty
		want     int
		wantSide string
		// fill is |q| written out independently: §10.3's S = 12 contracts in
		// H-CO-4a's quantum. It is never taken from q.Abs().
		fill Qty
		why  string
	}{
		{
			name:     "long twelve contracts",
			q:        Qty(1200),
			want:     1,
			wantSide: "no",
			fill:     Qty(1200),
			why: "a full fill of one of §10.3's 12-contract base quotes from " +
				"flat is long YES (§8.1), so §6.2 must select \"no\" as the " +
				"reducer; selecting \"yes\" doubles the position instead of " +
				"clearing it",
		},
		{
			name:     "short twelve contracts",
			q:        Qty(-1200),
			want:     -1,
			wantSide: "yes",
			fill:     Qty(1200),
			why: "the symmetric deployed case: a fully filled 12-contract NO " +
				"quote is long NO, and §6.2 must select \"yes\". The failure " +
				"is symmetric about zero (H-CO-4a), so both signs are kept",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.Sign(); got != tc.want {
				t.Fatalf("Qty(%d).Sign() = %d, want %d (%s = %q contracts) -- %s",
					int64(tc.q), got, tc.want, tc.name, tc.q.Wire(), tc.why)
			}

			side := reducingSide(tc.q)
			if side != tc.wantSide {
				t.Fatalf("reducing side for q = %s is %q, want %q -- §6.2 "+
					"selects \"no\" when q > 0, \"yes\" when q < 0, and none "+
					"only when q == 0: %s", tc.q.Wire(), side, tc.wantSide, tc.why)
			}

			// A complete fill of a |q|-sized reducer moves the position by
			// exactly that count against the side the reducer quotes: a "no"
			// reducer takes on NO exposure and drives a YES-positive q down, a
			// "yes" reducer drives it up. The direction is the one selected
			// above -- production's Sign, via §6.2's rule. The magnitude is the
			// table's own literal.
			var rem Qty
			switch side {
			case "no":
				rem = tc.q - tc.fill
			case "yes":
				rem = tc.q + tc.fill
			default:
				t.Fatalf("§6.2 selected no reducing side for q = %s: a market "+
					"holding %s contracts has no reducer to size, so A4 has "+
					"nothing resting and nothing in flight while q != 0, and "+
					"H-HALT-3's drain can never complete", tc.q.Wire(), tc.q.Wire())
			}

			// H-Q-5a: a full fill of the reducer must produce EXACTLY zero.
			// Compared as a raw int64 so the verdict does not route back
			// through IsFlat or Sign, and diagnosed by which of the three
			// wrong outcomes it is.
			if int64(rem) != 0 {
				switch {
				case int64(rem)*int64(tc.want) < 0:
					t.Fatalf("a full fill of a %s reducer on side %q moved q "+
						"from %s to %s: the reducer changed the sign of the "+
						"position. H-Q-5a admits no q and no fill sequence for "+
						"which that is possible, and A12 states it as a running "+
						"invariant", tc.fill.Wire(), side, tc.q.Wire(), rem.Wire())
				case int64(rem)*int64(tc.want) > int64(tc.q)*int64(tc.want):
					// Multiplying by the starting sign reads off the magnitude,
					// without asking Abs or Sign about the result.
					t.Fatalf("a full fill of a %s reducer on side %q moved q "+
						"from %s to %s: |q| grew. The reducer quoted the "+
						"ADDING side (A8) and drove the position toward "+
						"inv_kill instead of toward flat (§6.2, H-Q-5a)",
						tc.fill.Wire(), side, tc.q.Wire(), rem.Wire())
				case rem == tc.q:
					t.Fatalf("a full fill of a %s reducer on side %q left q at "+
						"%s: the position was preserved rather than reduced, so "+
						"REDUCING never reaches IDLE (§5.2) and WINDING_DOWN "+
						"never drains (H-HALT-3)",
						tc.fill.Wire(), side, rem.Wire())
				default:
					t.Fatalf("a full fill of a %s reducer on side %q left q at "+
						"%s against a starting position of %s, want exactly "+
						"0.00 -- H-Q-5a requires a full fill to produce exactly "+
						"zero, and |q| to strictly decrease on every positive "+
						"partial", tc.fill.Wire(), side, rem.Wire(), tc.q.Wire())
				}
			}
		})
	}
}

// wireQuanta renders an exact quantum count as the exchange's canonical
// two-decimal wire string (H-CO-2), using integer arithmetic only.
//
// This is the independent oracle the parser and the formatter are measured
// against below. It never touches float64, QtyFromFloat or Qty.Wire, so a
// rounding defect shared by the production quantizer and the production
// formatter cannot hide behind the two of them agreeing with each other.
func wireQuanta(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/QtyScale, n%QtyScale)
}

// TestQtyRoundTripAndFormat pins the H-CO-2 wire encoding and the H-CO-4a
// quantum in every direction the harness uses: float -> Qty, string -> Qty, and
// Qty -> string.
//
// The parser half is the lip-d50 oracle gap. ParseQty is the only path from an
// exchange-supplied count to a local `q`, and every number it reads is already
// exact: H-CO-2 puts `count`, `fill_count` and `remaining_count` on the wire as
// two-decimal fixed-point STRINGS, and H-CO-4a defines `q` as exactly that
// value in contracts x 100. A parse is therefore a transcription with no
// rounding decision left to make -- but ParseQty makes one anyway, by way of
// float64 and QtyFromFloat, and nothing here proved it always transcribes.
//
// A parser that rounds up by one quantum is not a display defect. Sizes are
// fractional in ~20.5% of resting levels (H-CO-4) and §10.3 posts 12 contracts
// in each of six markets, so a 0.07 partial fill is ordinary rather than
// exotic. Size a reducer from an inflated |q| and it still passes ValidateCount
// -- H-CO-4b's bound is that same inflated number -- so the count is "valid"
// against a position that does not exist. A full fill then takes the real
// exchange position from +0.07 to -0.01: the reducer changed the sign of q,
// which H-Q-5a says no q and no fill sequence may do and A12 states as a
// running invariant. V1.3 asks for exactly this, fractional `q` included.
func TestQtyRoundTripAndFormat(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0.00"}, {1, "1.00"}, {100, "100.00"}, {0.5, "0.50"},
		{-61, "-61.00"}, {0.01, "0.01"}, {-0.01, "-0.01"}, {161.25, "161.25"},
	} {
		if got := QtyFromFloat(tc.in).Wire(); got != tc.want {
			t.Errorf("QtyFromFloat(%v).Wire() = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Rounding is symmetric about zero: half away from zero, both signs.
	if got := QtyFromFloat(0.005); got != 1 {
		t.Errorf("QtyFromFloat(0.005) = %d, want 1", int64(got))
	}
	if got := QtyFromFloat(-0.005); got != -1 {
		t.Errorf("QtyFromFloat(-0.005) = %d, want -1", int64(got))
	}

	// The falsification target, named. H-CO-4a binds q to the exchange's own
	// contracts x 100 integer, so "0.07" is seven quanta and nothing else.
	t.Run("parse canonical", func(t *testing.T) {
		for _, tc := range []struct {
			wire string
			want Qty
			why  string
		}{
			{
				wire: "0.00",
				want: Qty(0),
				why: "a flat count on the wire must parse flat; §5.2 lets " +
					"REDUCING leave for IDLE only at q == 0",
			},
			{
				wire: "0.07",
				want: Qty(7),
				why: "a 0.07 partial fill of one of §10.3's 12-contract " +
					"orders is ordinary (H-CO-4); parsing it as Qty(8) " +
					"credits us a quantum the exchange never gave us",
			},
			{
				wire: "-0.07",
				want: Qty(-7),
				why: "the quantum is symmetric about zero (H-CO-4a): a short " +
					"of seven hundredths is Qty(-7), not Qty(-8)",
			},
		} {
			got, err := ParseQty(tc.wire)
			if err != nil {
				t.Fatalf("ParseQty(%q) returned an error: %v", tc.wire, err)
			}
			if got != tc.want {
				t.Fatalf("ParseQty(%q) = Qty(%d), want Qty(%d) -- %s",
					tc.wire, int64(got), int64(tc.want), tc.why)
			}
		}
	})

	// Every count the exchange can express inside §10.3's deployed
	// S_max = 48 contracts/side, both signs, one quantum at a time. The wire
	// string and the expected Qty are both built from the integer quantum, so
	// nothing in this loop shares a rounding rule with the code under test.
	t.Run("parse sweep to S_max", func(t *testing.T) {
		const sMaxQuanta = 48 * QtyScale // §6.2 / §10.3: S_max = 48 contracts
		for n := int64(-sMaxQuanta); n <= sMaxQuanta; n++ {
			wire := wireQuanta(n)
			got, err := ParseQty(wire)
			if err != nil {
				t.Fatalf("ParseQty(%q) returned an error: %v", wire, err)
			}
			if int64(got) != n {
				t.Fatalf("ParseQty(%q) = Qty(%d), want Qty(%d): the parser is "+
					"not transcribing an exact two-decimal count (H-CO-4a). "+
					"An inflated |q| sizes a reducer the position cannot "+
					"absorb, which is the H-Q-5a / A12 sign flip",
					wire, int64(got), n)
			}
			if back := got.Wire(); back != wire {
				t.Fatalf("Qty(%d).Wire() = %q, want %q -- H-CO-2 requires a "+
					"count to go back out as the same fixed-point string it "+
					"came in as", n, back, wire)
			}
		}
	})

	// H-Q-5a / A12, driven end to end from the wire: parse a position, size a
	// reducer from it, fill the reducer completely, and land exactly flat.
	//
	// `quanta` is the exchange's real position, known independently of the
	// parser as a literal. The remainder is computed against THAT, not against
	// the parsed value, because that is where the defect shows: a parse that
	// inflates |q| by one quantum produces a count ValidateCount happily
	// accepts (its bound is the same inflated number) and a fill that moves the
	// real position past zero.
	t.Run("reducer from parsed position", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			wire   string
			quanta int64
		}{
			{name: "long seven hundredths", wire: "0.07", quanta: 7},
			{name: "short seven hundredths", wire: "-0.07", quanta: -7},
		} {
			t.Run(tc.name, func(t *testing.T) {
				q, err := ParseQty(tc.wire)
				if err != nil {
					t.Fatalf("ParseQty(%q) returned an error: %v", tc.wire, err)
				}

				// size_R = min(|q|, S_max, funded); at 0.07 contracts |q| is
				// the binding term and the other two are irrelevant.
				count := q.Abs()
				if err := ValidateCount(count, q); err != nil {
					t.Fatalf("a reducer of %s against q = %s was rejected: %v "+
						"-- H-CO-4b permits a count up to the quantized "+
						"position it derives from", count.Wire(), q.Wire(), err)
				}
				// A reducer trades the side opposite the position, so a full
				// fill moves the real position toward zero by exactly `count`.
				actual := Qty(tc.quanta)
				rem := actual - Qty(actual.Sign())*count

				if rem.Sign() == -actual.Sign() {
					t.Fatalf("a full fill of a %s reducer moved the position "+
						"from %s to %s: the reducer changed the sign of q. "+
						"H-Q-5a admits no q and no fill sequence for which "+
						"that is possible, and A12 states it as an invariant",
						count.Wire(), actual.Wire(), rem.Wire())
				}
				if !rem.IsFlat() {
					t.Fatalf("a full fill of a %s reducer left %s against a "+
						"wire position of %s, want exactly flat -- H-Q-5a "+
						"requires a full fill to produce exactly zero",
						count.Wire(), rem.Wire(), tc.wire)
				}

				// And the count that went out on the wire was the position's
				// own magnitude, formatted per H-CO-2.
				absQuanta := tc.quanta
				if absQuanta < 0 {
					absQuanta = -absQuanta
				}
				if got, want := count.Wire(), wireQuanta(absQuanta); got != want {
					t.Fatalf("reducer count formats as %q against a wire "+
						"position of %q, want %q -- the dispatched count must "+
						"be the position's own magnitude (H-CO-4b)",
						got, tc.wire, want)
				}
			})
		}
	})
}

// TestValidateCountRejectsOvershoot is the H-Q-5a / H-CO-4b gate.
//
// H-Q-5a: the reducer never overshoots flat. Every positive partial fill must
// strictly decrease |q|, and a full fill must produce exactly zero. There is no
// q and no fill sequence for which a reducing order can change the sign of q.
//
// An earlier version of the spec sized the reducer at |q| + S and claimed a
// full fill left "a normal-sized position ... the system is self-correcting".
// That was false against §10.3's own parameters: with S = 100 and
// inv_hard = 60, q = +61 posted 161 contracts, a full fill gave q = -100 --
// ABOVE inv_hard, so immediately back in REDUCING, now posting 200. Full fills
// alternate -100, +100 indefinitely.
func TestValidateCountRejectsOvershoot(t *testing.T) {
	q := QtyFromFloat(61) // the HR-003 case

	// A reducer sized at |q| is legal and lands exactly flat.
	if err := ValidateCount(q.Abs(), q); err != nil {
		t.Fatalf("reducer sized at |q| rejected: %v", err)
	}
	if rem := q - q.Abs(); !rem.IsFlat() {
		t.Fatalf("a full fill of a |q|-sized reducer left %s, want flat",
			rem.Wire())
	}

	// The withdrawn |q| + S sizing must be rejected.
	overshoot := q.Abs() + QtyFromFloat(100)
	if err := ValidateCount(overshoot, q); err == nil {
		t.Fatalf("ValidateCount accepted a reducer of %s against q = %s "+
			"-- a full fill would flip the position to %s and re-enter "+
			"REDUCING (HR-003's -100/+100 sign-flip cycle)",
			overshoot.Wire(), q.Wire(), (q - overshoot).Wire())
	}

	// H-CO-4b: "0.00" is never sent, and neither is a negative count.
	if err := ValidateCount(0, q); err == nil {
		t.Fatal("ValidateCount accepted a zero count")
	}
	if err := ValidateCount(QtyFromFloat(-1), q); err == nil {
		t.Fatal("ValidateCount accepted a negative count")
	}

	// The bound is on magnitude, so it holds for a short position too.
	short := QtyFromFloat(-61)
	if err := ValidateCount(QtyFromFloat(61), short); err != nil {
		t.Fatalf("reducer sized at |q| rejected for a short position: %v", err)
	}
	if err := ValidateCount(QtyFromFloat(62), short); err == nil {
		t.Fatal("ValidateCount accepted an overshoot against a short position")
	}
}

// TestParseQtyRejectsMalformed is the lip-6h0 regression.
//
// fmt.Sscanf("%g") does not require consuming its input. ParseQty("0.07abc")
// returned Qty(7) with a nil error, so anything that is a number followed by
// anything at all parsed as a quantity. That matters more here than it would in
// an ordinary parser: ParseQty is the only path from an exchange-supplied count
// to a local `q`, and H-CO-4a makes `q` the foundation of every sign and zero
// comparison in the lifecycle. A count that is not really a count -- a
// truncated frame, a field misread as `count`, a wire format that grows a
// suffix -- became a position, and the caller had no error to escalate.
//
// The non-finite half is the more dangerous direction. QtyFromFloat maps NaN
// and Inf to Qty(0), and Qty(0) is not a neutral answer: it is precisely the
// answer that lets §5.2 leave REDUCING for IDLE and lets H-HALT-3 exit a
// process with inventory. Parsing "NaN" as flat abandons an open position; the
// only safe response is to refuse the parse.
func TestParseQtyRejectsMalformed(t *testing.T) {
	for _, tc := range []struct {
		wire string
		why  string
	}{
		{
			wire: "0.07abc",
			why: "the named lip-6h0 case: Sscanf(\"%g\") stopped at the first " +
				"non-numeric byte and reported success, so a malformed count " +
				"silently became a 0.07 position",
		},
		{"1.00 ", "a trailing space is still unconsumed input"},
		{" 1.00", "H-CO-2's wire count carries no leading whitespace"},
		{"12.00,", "a truncated frame can leave a delimiter attached"},
		{"1.00.00", "two decimal points is not a fixed-point count"},
		{"", "an absent field is not a zero position -- the caller must see " +
			"the error and treat the count as unknown, not as flat"},
		{"null", "a JSON null reaching the parser as a bare string must fail " +
			"rather than quantize to flat"},
		{"abc", "no numeric prefix at all"},
		{"+-1.00", "malformed sign"},
		{"1_000.00", "strconv.ParseFloat accepts Go literal underscores; " +
			"H-CO-2's wire count has no such spelling, and reading it as " +
			"1000 contracts is a three-orders-of-magnitude misread"},
		{"0x1p+10", "nor is a hex float, which ParseFloat reads as 1024"},
		{"1e3", "an exponent is not what a %.2f serializer emits"},
		{"1.00e0", "including an exponent on an otherwise well-formed count"},
		{".", "a bare decimal point has no digits"},
		{"-", "a bare sign has no digits"},
		{
			wire: "NaN",
			why: "QtyFromFloat maps NaN to Qty(0), and flat is the answer " +
				"that lets REDUCING reach IDLE and lets H-HALT-3 exit with " +
				"inventory (H-CO-4a)",
		},
		{"Inf", "an infinite count quantizes to flat, same abandonment"},
		{"-Inf", "and so does the negative one"},
		{
			wire: "1e400",
			why: "an overflowing literal returns ErrRange with a non-finite " +
				"value; taking the value anyway would quantize to flat",
		},
	} {
		got, err := ParseQty(tc.wire)
		if err == nil {
			t.Errorf("ParseQty(%q) = Qty(%d), want an error -- %s",
				tc.wire, int64(got), tc.why)
		}
		if got != 0 {
			t.Errorf("ParseQty(%q) returned Qty(%d) alongside its error, "+
				"want the zero value: a caller that ignores the error must "+
				"not receive a usable quantity", tc.wire, int64(got))
		}
	}

	// The valid forms the exchange actually sends still parse. Rejecting
	// trailing garbage must not narrow the accepted grammar.
	for _, tc := range []struct {
		wire string
		want Qty
	}{
		{"0.00", 0}, {"1.00", 100}, {"12.00", 1200}, {"0.07", 7},
		{"-0.07", -7}, {"50.00", 5000}, {"-12.00", -1200},
	} {
		got, err := ParseQty(tc.wire)
		if err != nil {
			t.Errorf("ParseQty(%q) returned an error: %v -- this is a count "+
				"the exchange sends (H-CO-2)", tc.wire, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseQty(%q) = Qty(%d), want Qty(%d)",
				tc.wire, int64(got), int64(tc.want))
		}
	}
}

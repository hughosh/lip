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

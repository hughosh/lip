package num

import (
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

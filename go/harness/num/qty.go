// Package num is the harness's exact quantity representation: harness-spec.md
// §4 H-CO-4a.
//
// It is a leaf. It imports nothing from this module, because both harness/quote
// (which sizes orders) and harness/risk (which holds positions) need Qty, and
// risk already imports quote for the market states. Putting Qty in either one
// makes the dependency a cycle; putting it below both makes the layering
// num <- cfg <- quote <- risk, which is acyclic and matches the direction the
// facts flow.
package num

import (
	"fmt"
	"math"
)

// QtyScale is the exchange's own quantum for a contract count: hundredths,
// matching the two-decimal fixed-point wire format ("50.00", "1.00").
const QtyScale = 100

// Qty is a contract quantity in exact fixed-point units of 1/QtyScale of a
// contract. Signed: positive is long YES, negative is long NO (§8.1).
//
// H-CO-4a. Sizes are float64 on the wire and fractional in ~20.5% of observed
// resting book levels, so this is reachable rather than theoretical. Fills of
// 0.10, 0.20 and -0.30 accumulated as float64 leave a binary residue near
// 5.6e-17. The market is economically flat, but `q != 0` is TRUE, so:
//
//   - REDUCING never reaches IDLE;
//   - SIGTERM never sees a drained market and the process never exits
//     (H-HALT-3);
//   - SETTLING formats |q| as "0.00" and rejects in a loop.
//
// A float equality test was gating three lifecycle transitions. Storing the
// quantum as an integer removes the class rather than tuning an epsilon.
type Qty int64

// QtyFromFloat quantizes a wire or computed size to the exchange's quantum.
//
// Rounds half away from zero, which is symmetric about zero. Banker's rounding
// (core.ParsePriceCents, P1) is deliberately NOT used here: that rule exists to
// reproduce CPython's price parsing bit-for-bit for the measurement rig, and
// applying it to quantities would make |q| depend on the parity of the
// hundredth -- a rule with no counterpart at the exchange.
func QtyFromFloat(f float64) Qty {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	scaled := f * QtyScale
	if scaled >= 0 {
		return Qty(math.Floor(scaled + 0.5))
	}
	return Qty(math.Ceil(scaled - 0.5))
}

// ParseQty parses a fixed-point wire count ("50.00", "1.00", null-safe caller).
func ParseQty(s string) (Qty, error) {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, fmt.Errorf("count %q: %w", s, err)
	}
	return QtyFromFloat(f), nil
}

// Float returns the quantity in contracts. For display and for arithmetic
// against float-valued book sizes only -- never round-trip a Qty through this.
func (q Qty) Float() float64 { return float64(q) / QtyScale }

// Wire formats the quantity as the exchange's fixed-point string (H-CO-2).
func (q Qty) Wire() string { return fmt.Sprintf("%.2f", q.Float()) }

func (q Qty) Abs() Qty {
	if q < 0 {
		return -q
	}
	return q
}

func (q Qty) Sign() int {
	switch {
	case q > 0:
		return 1
	case q < 0:
		return -1
	}
	return 0
}

// IsFlat is the only permitted zero test for a position. It is exact, because
// Qty is exact -- which is the entire point of H-CO-4a.
func (q Qty) IsFlat() bool { return q == 0 }

// ValidateCount checks a count about to be dispatched, per H-CO-4b: strictly
// positive, and no greater than the quantized position it derives from.
// "0.00" is never sent.
//
// `derivedFrom` is the aggregate bound the count was sized against -- |q| for a
// reducer (H-Q-5a), size_A for an adding quote. Pass 0 for "no bound".
func ValidateCount(count, derivedFrom Qty) error {
	if count <= 0 {
		return fmt.Errorf("count %s is not strictly positive", count.Wire())
	}
	if derivedFrom != 0 && count > derivedFrom.Abs() {
		return fmt.Errorf("count %s exceeds its bound %s",
			count.Wire(), derivedFrom.Abs().Wire())
	}
	return nil
}

// confidence: high

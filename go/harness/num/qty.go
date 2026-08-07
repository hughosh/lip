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
	"strconv"
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

// isFixedPoint reports whether s is exactly H-CO-2's fixed-point wire grammar:
// an optional sign, then digits with at most one decimal point, and at least
// one digit somewhere. No exponent, no underscore, no base prefix, no
// whitespace, no Inf or NaN spelling.
//
// This exists because strconv.ParseFloat accepts Go *literal* syntax, which is
// strictly wider than the wire: "1_000.00" is 1000, "0x1p+10" is 1024, "1e3" is
// 1000. None of those is a count the exchange emits, and each one turns a
// string we do not understand into a confident quantity. Narrowing the grammar
// follows H-CO-3a's precedent -- assert the measured regularity rather than
// assume it, and let a violation surface as an error instead of a number.
func isFixedPoint(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	var digits, points int
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			points++
			if points > 1 {
				return false
			}
		default:
			return false
		}
	}
	return digits > 0
}

// ParseQty parses a fixed-point wire count ("50.00", "1.00", null-safe caller).
//
// The whole string must be a finite fixed-point number. fmt.Sscanf("%g") was
// wrong here for two independent reasons, and both of them end in a position:
//
//   - It does not require consuming its input, so ParseQty("0.07abc") returned
//     Qty(7) with a nil error. A truncated frame, a field that is not the count
//     we think it is, or a wire format that grows a unit suffix all become a
//     silently plausible quantity rather than a parse failure. This is the
//     parser H-CO-4a makes the foundation of every sign and zero comparison, so
//     a bad transcription here reaches the lifecycle transitions directly.
//   - It accepts "NaN" and "Inf", which QtyFromFloat maps to Qty(0). A position
//     the exchange reported as unrepresentable would read back as flat, and
//     flat is exactly the answer that lets §5.2 leave REDUCING for IDLE and lets
//     H-HALT-3 exit the process. Refusing the parse leaves the caller with an
//     error to escalate; quantizing it to zero abandons the position.
//
// The grammar check runs first, so the value mapping for everything that is
// still accepted is unchanged: this narrows the input set, it does not
// renegotiate what a valid count means. The IsInf check then catches a decimal
// literal too long to represent, which ParseFloat reports as ErrRange with a
// non-finite value.
func ParseQty(s string) (Qty, error) {
	if !isFixedPoint(s) {
		return 0, fmt.Errorf("count %q is not a fixed-point number", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("count %q: %w", s, err)
	}
	if math.IsInf(f, 0) {
		return 0, fmt.Errorf("count %q is not finite", s)
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

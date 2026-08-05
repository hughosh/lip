package num

import (
	"fmt"
	"math"
)

// MoneyScale is micro-dollars per dollar. Money is stored as an integer count
// of 1e-6 USD.
//
// The scale is chosen so that every monetary quantity the harness computes is
// EXACT rather than merely close, by the same argument H-CO-4a makes for Qty:
//
//   - Prices on the wire are 4-decimal dollar strings. Measured against
//     archive/raw-*.jsonl.gz, every one of 79,526 sampled `yes_price_dollars`
//     values carried exactly four decimals, so 1e-4 USD is the exchange's own
//     price quantum. Resting book levels are integer cents on top of that
//     (H-CO-3a: 14,799,781 book price strings, zero fractional).
//   - Qty is hundredths of a contract.
//   - Therefore contracts x dollars = (Qty/1e2) x (price4/1e4) = Qty*price4 in
//     units of 1e-6 USD, with no division and no rounding.
//
// Notional() below is that identity, and it is why the scale is 1e6 and not
// 1e4: at 1e4 a fractional-cent fill price would not divide exactly.
const MoneyScale = 1_000_000

// Money is an amount in exact units of 1/MoneyScale USD. Signed.
//
// H-CAP-5 makes an `insufficient_balance` reject a CORRECTNESS failure with a
// global WINDING_DOWN rather than a market condition, and H-CAP-7 has K REST
// workers reserving and releasing collateral against one running total before
// dispatch. A float64 accumulator under repeated reserve/release is HR-026 one
// layer up: the residue is invisible until a comparison against capital_max
// decides the wrong way, and the failure it produces is defined to be a global
// stop. Integers remove the class instead of bounding it.
type Money int64

// MoneyFromDollars converts a configured or human-entered dollar amount.
// Rounds half away from zero, symmetric about zero, as QtyFromFloat does.
func MoneyFromDollars(d float64) Money {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return 0
	}
	scaled := d * MoneyScale
	if scaled >= 0 {
		return Money(math.Floor(scaled + 0.5))
	}
	return Money(math.Ceil(scaled - 0.5))
}

// Price4FromCents converts an integer-cent book price to the exchange's 1e-4
// USD price quantum. Book prices are always integer cents (H-CO-3a), so this is
// the conversion the quoting path uses; fill prints from the trade stream are
// parsed at full wire precision and do not go through it (V2-FILL).
func Price4FromCents(cents int) int64 { return int64(cents) * 100 }

// Notional is quantity x price, exactly. `price4` is in units of 1e-4 USD.
//
// This is the collateral formula of §10.2 for one side:
//
//	capital(market) = S * (p_yes + p_no) / 100 dollars
//
// evaluated as Notional(S, Price4FromCents(p_yes)) + Notional(S, ...p_no).
// Summing the two sides rather than multiplying by their sum is deliberate: the
// two sides can carry different quantities once the quote is skewed (§6.2), and
// a formula that assumes they cannot would silently under-reserve exactly when
// inventory is the thing being managed.
func Notional(q Qty, price4 int64) Money { return Money(int64(q) * price4) }

// Dollars returns the amount in USD. For display and for arithmetic against
// float-valued exchange responses only -- never round-trip a Money through it.
func (m Money) Dollars() float64 { return float64(m) / MoneyScale }

// String formats as USD to the cent, which is the resolution an operator reads.
// It is lossy on purpose: exactness is for the comparisons, not for the ping.
func (m Money) String() string { return fmt.Sprintf("$%.2f", m.Dollars()) }

func (m Money) Abs() Money {
	if m < 0 {
		return -m
	}
	return m
}

// confidence: high

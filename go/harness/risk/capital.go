package risk

import (
	"fmt"

	"lip/harness/cfg"
	"lip/harness/num"
)

// ---------------------------------------------------------------------------
// §10.2 — capital
// ---------------------------------------------------------------------------

// MaxEntryPrice4 is H-SEL-7's bound: no market is entered above 99c. In the
// exchange's 1e-4 USD price quantum.
const MaxEntryPrice4 = 99 * 100

// SettlementPrice4 is the full value of one contract at settlement, $1.00.
const SettlementPrice4 = 100 * 100

// Exposure is one market's committed collateral, split by what the collateral
// is doing. The split is the whole point: H-CAP-2's concentration cap applies
// to two of these three and H-CAP-6 explicitly exempts the third.
//
// Every figure is AGGREGATE across RESTING + SENDING + UNKNOWN +
// unconfirmed-cancel (H-CAP-7, H-Q-5b, A11). An order we have requested a
// cancel for still holds its collateral, because the exchange has not told us
// otherwise and H-FAIL-3 says cancel-requested is still live.
type Exposure struct {
	Ticker string

	// Adding is collateral committed by adding-side orders.
	Adding num.Money
	// Reducing is collateral committed by reducing-side orders.
	Reducing num.Money
	// Position is collateral already spent on the held position.
	Position num.Money
}

// Total is everything this market has committed.
func (e Exposure) Total() num.Money { return e.Adding + e.Reducing + e.Position }

// Concentrated is the part H-CAP-2's per-market cap measures: the risk we have
// TAKEN in this market. The reducing side is excluded, because H-CAP-2 is a
// diversification rule for taking risk and applying it to the exit is the
// probe's defect reached by arithmetic (H-CAP-6).
func (e Exposure) Concentrated() num.Money { return e.Adding + e.Position }

// SideCost is the collateral an aggregate quantity commits at a price.
//
// §10.2's formula for a two-sided quote is `S · (p_yes + p_no) / 100` dollars.
// It is evaluated here one side at a time and summed by the caller, never as
// `S × (p_yes + p_no)`: the two sides carry DIFFERENT quantities once the quote
// is skewed (§6.2), and a formula that assumes they cannot would silently
// under-reserve exactly when inventory is the thing being managed.
//
// Exact by construction -- num.Notional is Qty × price4 with no division.
func SideCost(atRisk num.Qty, price4 int64) num.Money {
	if atRisk < 0 {
		atRisk = -atRisk
	}
	return num.Notional(atRisk, price4)
}

// Deployed is total committed collateral across every market. H-CAP-1's
// measure, and A7's.
func Deployed(ex []Exposure) num.Money {
	var total num.Money
	for _, e := range ex {
		total += e.Total()
	}
	return total
}

// FundedContracts converts an amount of collateral into a contract count at a
// price, which is the unit §6.2's `size_R` needs.
//
// Floors. A count that rounds UP is a count the account cannot pay for, and
// H-CAP-5 makes an insufficient_balance reject a CORRECTNESS failure -- SEV1
// and a global WINDING_DOWN -- rather than a market condition. Rounding a
// reducer up by one quantum to be generous would therefore trip the most
// serious stop condition in the system in order to place one extra hundredth
// of a contract.
func FundedContracts(available num.Money, price4 int64) num.Qty {
	if available <= 0 || price4 <= 0 {
		return 0
	}
	return num.Qty(int64(available) / price4)
}

// ReducingBudget is what a reducing quote may commit, in two figures.
//
// `now` is what can be funded without cancelling anything: everything not
// already committed.
//
// `afterCuts` is H-CAP-4's figure: "reducing quotes are funded before adding
// quotes, globally. If capital is short, the adding side is what gets cut.
// Always." It is what would be available once every adding order in every
// market is cancelled, so it is a budget the caller can REACH, not one it
// already has.
//
// The distinction is not pedantry. Capital tied up in an adding order is not
// free until that order is cancelled and the exchange confirms it, and a
// reducer sized against `afterCuts` before the cancels land gets an
// insufficient_balance reject -- which under H-CAP-5 is a correctness failure
// and a global WINDING_DOWN. So: size against `now`; if the exit needs more,
// cancel adding orders and size again on the next tick.
//
// Neither figure applies the concentration cap (H-CAP-6) or withholds the
// reserve (H-CAP-3). The reserve exists precisely so that a fill never strands
// the reducing quote; withholding it from the exit would invert its purpose.
//
// > Red-team HR-005: under the previous defaults the per-market cap was
// > 500/6 × 2 = $166.67. A q = −100 position needing a 100-contract YES reducer
// > with YES at 89c requires $89 -- fine. But the earlier |q| + S sizing needed
// > 200 contracts and $178, exceeding the per-market cap and the entire $125
// > reserve, so the only exit was unfundable under the harness's own opening
// > configuration, even after cancelling everything else. A capital rule that
// > forbids the exit is the same defect as the probe's, arrived at from the
// > opposite direction.
func ReducingBudget(ex []Exposure, p cfg.Params) (now, afterCuts num.Money) {
	var adding, committed num.Money
	for _, e := range ex {
		adding += e.Adding
		committed += e.Total()
	}
	now = p.CapitalMax - committed
	afterCuts = p.CapitalMax - (committed - adding)
	if now < 0 {
		now = 0
	}
	if afterCuts < 0 {
		afterCuts = 0
	}
	return now, afterCuts
}

// AddingBudget is what an adding quote in `ticker` may commit.
//
// Three caps, and the binding one wins:
//
//   - H-CAP-1 global deployed capital <= capital_max, less
//   - H-CAP-3's reserve, held unallocated so a fill never strands the reducer;
//   - H-CAP-2 per-market <= capital_max / n_markets · concentration, measured
//     against what this market has TAKEN (position plus adding orders) and not
//     against its exit (H-CAP-6).
//
// `reducerNeed` is collateral the exits require and do not yet hold. H-CAP-4
// funds reducers before adding quotes GLOBALLY, so it is subtracted from the
// global budget before this market gets to look at it -- an adding quote in a
// quiet market does not get to consume the capital another market's exit is
// about to need.
func AddingBudget(ticker string, ex []Exposure, reducerNeed num.Money, p cfg.Params) num.Money {
	deployable := num.Money(float64(p.CapitalMax) * (1 - p.CapitalReserve))

	global := deployable - Deployed(ex) - reducerNeed

	var mine Exposure
	for _, e := range ex {
		if e.Ticker == ticker {
			mine = e
			break
		}
	}
	perMarket := num.Money(0)
	if p.NMarkets > 0 {
		perMarket = num.Money(
			float64(p.CapitalMax) / float64(p.NMarkets) * p.Concentration)
	}
	local := perMarket - mine.Concentrated()

	budget := global
	if local < budget {
		budget = local
	}
	if budget < 0 {
		return 0
	}
	return budget
}

// ---------------------------------------------------------------------------
// H-CAP-8 — startup fundability
// ---------------------------------------------------------------------------

// CheckFundable is H-CAP-8: "a configuration that cannot fund the reducer for
// the worst permitted simultaneous fill set is rejected at startup, checked as
// arithmetic, not discovered at fill time."
//
// Two arithmetic checks, because the rule has two halves that bind at different
// configurations and §10.3 derived the shipped parameters from the first alone.
//
// **(a) The fill set itself must fit the deployable capital.** At
// `n_markets · S · max_price` with the 99c bound H-SEL-7 permits. This is
// §10.3's own derivation, run in reverse: with capital_reserve = 0.25, $100
// leaves $75 deployable, so `6 · S · $0.99 <= $75` gives `S <= 12`. The previous
// defaults failed it -- `6 · 100 · $0.99 = $594 > $500` -- which meant the
// recommended opening configuration was required to reject itself at startup,
// and no implementation could satisfy §10.3 and H-CAP-8 at once (lip-afr).
//
// **(b) The position and its exit together must fit capital_max.** A contract
// entered at `p` and exited on the other side at `100 − p` costs the full
// settlement value, $1.00, for the round trip. So
// `n_markets · S · $1.00 <= capital_max` is the bound on holding every position
// AND every reducer at once, which is the state H-CAP-8 names. At the shipped
// parameters this is `6 · 12 · $1.00 = $72` against $100.
//
// **The residual (b) does not cover, stated rather than hidden.** The `100 − p`
// bound on the exit price holds while the book is uncrossed. It is our own pair
// that A3 and H-CO-6 keep under 100, not the market's, and the exit is placed
// at a touch that may have moved a long way from where the position was
// entered: long YES bought at 20c with the NO touch now at 85c costs 105c for
// the round trip, not 100c. That is a loss, and it is also a genuine funding
// requirement above what this check reserves for. It is bounded by the account
// balance and by nothing else in this file.
func CheckFundable(p cfg.Params) error {
	if p.NMarkets < 1 || p.S <= 0 {
		return fmt.Errorf("n_markets = %d and S = %s: neither is a "+
			"configuration to check", p.NMarkets, p.S.Wire())
	}

	worstFills := num.Money(0)
	roundTrip := num.Money(0)
	for i := 0; i < p.NMarkets; i++ {
		worstFills += num.Notional(p.S, MaxEntryPrice4)
		roundTrip += num.Notional(p.S, SettlementPrice4)
	}
	deployable := num.Money(float64(p.CapitalMax) * (1 - p.CapitalReserve))

	if worstFills > deployable {
		return fmt.Errorf("H-CAP-8: the worst permitted simultaneous one-sided "+
			"fill set costs %s (%d markets · %s contracts · $0.99) against %s "+
			"deployable (%s capital_max less a %.0f%% reserve). The "+
			"configuration would have to reject itself at startup",
			worstFills, p.NMarkets, p.S.Wire(), deployable,
			p.CapitalMax, p.CapitalReserve*100)
	}
	if roundTrip > p.CapitalMax {
		return fmt.Errorf("H-CAP-8: holding the worst permitted fill set AND "+
			"its exits costs %s (%d · %s · $1.00) against %s capital_max, so "+
			"the reducer for a fully filled book cannot be funded",
			roundTrip, p.NMarkets, p.S.Wire(), p.CapitalMax)
	}
	return nil
}

// confidence: high

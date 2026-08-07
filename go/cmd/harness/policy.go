package main

import (
	"context"
	"fmt"
	"sort"

	"lip/harness/cfg"
	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// adoptionPolicy is H-ORD-5c, and it lives here because `lifecycle` refuses to
// implement it:
//
//	Deciding whether an adopted order is valid requires the selection rules,
//	the close schedule, the size and price rules and the capital model -- four
//	subsystems whose wiring is the unit after this one. An interim "keep
//	everything" implementation here would be H-ORD-5c deleted, and it would
//	pass every gate in this package.
//
// The rule it implements is one sentence of §7.5:
//
//	Adoption preserves orders we would place today; it is not a licence for a
//	prior incarnation's orders to persist unexamined -- including an adding
//	order in a market that is now flat and unselected.
//
// So the question this type answers is not "is this order harmless?" but "would
// this harness, with the position and selection it has RIGHT NOW, place this
// order?" Anything else is cancelled and swept (H-ORD-4), because an unverified
// cancel is a live order (H-FAIL-3).
//
// # Why there is no keep-by-default branch
//
// `AdoptionUnset` is the zero value and it is a hard error upstream. Every path
// through `DecideAdopted` therefore returns `AdoptionKeep` or `AdoptionCancel`
// explicitly, and the final `Keep` is reached only by falling through every
// test rather than by an early return. A policy that returned early on a case it
// had not thought about would be handing a prior incarnation's order exactly the
// unexamined licence the rule forbids.
//
// # Determinism, and why it matters more than it looks
//
// `DecideAdopted` is called ONCE PER ORDER with the same `AdoptionFacts` every
// time. Several of the rules below are aggregate -- H-Q-5a's reducer cap, S_max
// per side, H-CAP-2's per-market ceiling -- so they cannot be answered from one
// order in isolation, and `AdoptionFacts.OwnedResting` exists precisely to make
// them answerable. Each is evaluated by rebuilding the SAME deterministic
// ordering over the whole set and asking where this order falls in it, so the
// answers are mutually consistent: it is not possible for this policy to keep
// two orders that together break a cap it enforced on each of them separately.
type adoptionPolicy struct {
	p cfg.Params
}

func newAdoptionPolicy(p cfg.Params) *adoptionPolicy {
	return &adoptionPolicy{p: p}
}

// DecideAdopted is H-ORD-5c evaluated for one order.
//
// The tests are ordered, and the order is the rule rather than an
// implementation detail:
//
//  1. **Well-formedness.** An order whose price or size this harness cannot
//     express cannot be requoted, resized or reasoned about, so it is cancelled
//     before any rule that would need to read it.
//  2. **q != 0 -- the market is REDUCING.** §7.5 step 6 is unconditional:
//     "Any market with q != 0 enters at REDUCING, not QUOTING, regardless of
//     size." §5.2 gives REDUCING an adding side that is "cancelled, confirmed",
//     so on a held market every adding-side order goes, whatever its size and
//     whatever the selection says. The reducer stays, capped at |q|.
//  3. **q == 0 -- the market is QUOTING if selected and IDLE otherwise.** At
//     q == 0 there is no reducing side (§6.2), so EVERY order here adds risk,
//     and the selection, adding-authority, size, self-cross and capital rules
//     all apply.
//
// Step 2 before step 3 is what makes the two branches exhaustive without
// overlapping: "is there a position" partitions the markets, and the meaning of
// a side depends on which half you are in.
func (a *adoptionPolicy) DecideAdopted(ctx context.Context, o rest.Order,
	f lifecycle.AdoptionFacts) (lifecycle.AdoptionDecision, error) {

	if err := ctx.Err(); err != nil {
		// Not a cancellation. A cancelled context means the process is going
		// away, and returning a decision derived from a pass that will not
		// finish would put a cancel on the wire for an order nobody re-examined.
		return lifecycle.AdoptionUnset, err
	}
	if o.Ticker == "" {
		return lifecycle.AdoptionUnset, fmt.Errorf("adopted order %s names no "+
			"market; every rule below is scoped to one, and guessing which "+
			"would attribute it to a position it has nothing to do with",
			o.OrderID)
	}

	// --- 1. well-formedness -------------------------------------------------

	if o.OrderID == "" {
		// Cancelled rather than errored, and the difference matters: §7.5
		// retries a failed pass indefinitely, so returning an error for an order
		// the exchange keeps re-reporting would leave STARTING forever. Cancel
		// is also honest about what happens next -- `CancelAndSweep` cannot
		// address an order with no id, so the sweep does not come back Clean,
		// and §7.5 escalates that rather than adopting it.
		//
		// It is also what makes the aggregate rules below sound: they identify
		// this order within the owned set BY id, and two ids that are both empty
		// are indistinguishable.
		return lifecycle.AdoptionCancel, nil
	}
	if o.Remaining <= 0 {
		// Nothing left to keep. Cancelling is also the honest answer for a
		// negative remainder, which is a wire value we cannot interpret.
		return lifecycle.AdoptionCancel, nil
	}
	if o.Fractional {
		// H-CO-3a. Every price comparison downstream -- the requote trigger, the
		// stranded brake, H-CO-6's self-cross bound, the capital cost -- is in
		// whole cents, and a resting price that is not one cannot be compared
		// against a touch without rounding it first. Rounding a price we did not
		// choose, in a direction we did not choose, is how an order ends up one
		// tick inside the touch permanently (H-Q-2).
		return lifecycle.AdoptionCancel, nil
	}
	if !quote.ValidPrice(o.PriceCents) {
		// Outside the tradable range. Nothing in §6.5 can move it, because
		// every target price it computes is inside the range.
		return lifecycle.AdoptionCancel, nil
	}

	q := f.Positions[o.Ticker]
	reducing, held := quote.ReducingSide(q)

	// --- 2. the market holds a position ------------------------------------

	if held {
		if o.Side != reducing {
			// §5.2's REDUCING row: adding side cancelled, and A8 requires the
			// cancel be exchange-CONFIRMED absent rather than merely requested.
			// This fires regardless of selection and regardless of size: an
			// adding order on a market we hold is exposure we would not add
			// today, and §7.5 step 6 puts every held market in REDUCING before
			// the selection rules get a say.
			return lifecycle.AdoptionCancel, nil
		}
		// H-Q-5a and A12: no reducing order's aggregate quantity exceeds |q|,
		// and no fill sequence changes the sign of q via a reducer. HR-004 is
		// the sequence this prevents -- two individually-capped reducers that
		// both fill flip the position.
		//
		// The cap is on the AGGREGATE (H-Q-5b), so the question for one order is
		// whether it fits under the cap once the orders that outrank it have
		// taken their share.
		if !a.fitsUnderQty(o, f, q.Abs()) {
			return lifecycle.AdoptionCancel, nil
		}
		return lifecycle.AdoptionKeep, nil
	}

	// --- 3. the market is flat ---------------------------------------------
	//
	// Everything below adds risk. At q == 0 there is no reducing side at all
	// (§6.2's R is undefined there), so none of these orders is an exit and none
	// of them carries A4's protection.

	if !selected(f.Selected, o.Ticker) {
		// H-ORD-5c's own worked example, verbatim: "including an adding order in
		// a market that is now flat and unselected".
		//
		// `Selected` is used exactly as supplied and `Excluded` is NOT
		// subtracted from it here. `lifecycle` documents that it hands over the
		// EFFECTIVE set -- the operator's selection with every foreign-excluded
		// ticker already removed -- and `M-L-EXCLUDESELECT` breaks that by
		// passing the raw set instead. A policy that defensively subtracted
		// `Excluded` itself would produce the right answer under the mutation
		// and the mutation would go uncaught, so the check that would look
		// prudent here is the one that blinds the gate.
		return lifecycle.AdoptionCancel, nil
	}
	if !f.AddingPermitted {
		// The lifecycle overrides a Keep into a verified cancellation for an
		// adding order when adding authority has been revoked, so this is
		// belt-and-braces rather than the only line of defence. It is written
		// anyway because the two answers agreeing is worth more than one of them
		// silently correcting the other: a policy that returned Keep here would
		// be asserting that a latched, halted or storage-failed process should
		// rest a new risk-adding quote, and that assertion should not exist in
		// the tree even where something downstream is going to ignore it.
		return lifecycle.AdoptionCancel, nil
	}
	if !a.fitsUnderQty(o, f, a.p.SMax) {
		// §6.2's S_max: the aggregate cap per side per market. At q == 0 the
		// state is QUOTING, which quotes S on both sides; S_max is the ceiling
		// the aggregate may not pass however the previous incarnation got there.
		return lifecycle.AdoptionCancel, nil
	}
	if a.selfCrosses(o, f) {
		// H-CO-6 and A3: our own YES and NO bids may not sum to 100 or more, or
		// we trade with ourselves. Both legs go rather than one, and the choice
		// is deliberate: at q == 0 neither leg is an exit, so neither has A4's
		// claim to survive, and picking a survivor means picking which of two
		// prices this incarnation did not choose it would rather keep. §6.5
		// re-places from the touch on the next tick at the cost of two writes.
		//
		// This test lives in the flat branch only. A crossing pair on a HELD
		// market has already lost its adding leg to §5.2 above, so what is left
		// is a single reducer, which cannot cross itself.
		return lifecycle.AdoptionCancel, nil
	}
	if !a.fitsUnderCapital(o, f) {
		return lifecycle.AdoptionCancel, nil
	}

	return lifecycle.AdoptionKeep, nil
}

// rank orders the orders on one side of one market from most to least worth
// keeping, and it is the SAME ordering for every call.
//
// Highest price first. Both sides of a Kalshi book are BIDS (§4), so higher is
// nearer the touch on either of them, and nearness to the touch is what both
// roles want for opposite reasons: a reducer at the touch is the one that
// actually exits (A4's obligation is a LIVE exit, not a theoretical one), and an
// adding quote at the touch is the one that scores at N = 0 (H-Q-1). An order
// several ticks back is the one §6.5 would move anyway.
//
// The coid breaks ties, so the ordering is total and the answer does not depend
// on the order the exchange happened to list the walk in.
func rank(orders []rest.Order) []rest.Order {
	out := append([]rest.Order(nil), orders...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PriceCents != out[j].PriceCents {
			return out[i].PriceCents > out[j].PriceCents
		}
		return out[i].ClientOrderID < out[j].ClientOrderID
	})
	return out
}

// sameSide is every owned resting order on one market and one side, well-formed
// enough to be counted against a cap.
//
// Malformed orders are EXCLUDED from the aggregate rather than counted in it.
// They are being cancelled by rule 1 whatever this returns, and counting a
// fractional or out-of-range order against the cap would let one order this
// harness cannot manage evict one it can.
func sameSide(f lifecycle.AdoptionFacts, ticker string, side quote.Side) []rest.Order {
	out := make([]rest.Order, 0, len(f.OwnedResting))
	for _, r := range f.OwnedResting {
		if r.Ticker != ticker || r.Side != side {
			continue
		}
		if r.OrderID == "" || r.Remaining <= 0 || r.Fractional ||
			!quote.ValidPrice(r.PriceCents) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// fitsUnderQty answers whether `o` survives an AGGREGATE quantity cap on its own
// side of its own market.
//
// The aggregate is H-Q-5b's: every size cap in this system is evaluated against
// the total working quantity on a side, never against one order. So the
// per-order question is which orders the cap has room for, and the answer is the
// prefix of `rank` that fits.
//
// A prefix rather than "cancel the whole side and re-place". Cancelling
// everything is simpler and it is what an earlier draft of this file did, but on
// a reducing side it opens a window with NO exit resting while the replacement
// is queued behind a confirmed cancel -- and A4's obligation is that the exit is
// there, not that it is correctly sized on the second attempt. The prefix keeps
// the best exits and cancels only the excess, which is both smaller and safer.
func (a *adoptionPolicy) fitsUnderQty(o rest.Order, f lifecycle.AdoptionFacts,
	limit num.Qty) bool {

	if limit <= 0 {
		return false
	}
	var used num.Qty
	for _, r := range rank(sameSide(f, o.Ticker, o.Side)) {
		if used+r.Remaining > limit {
			// This one does not fit. It is not a break: a later, smaller order
			// may still fit in the remaining headroom, and refusing it because a
			// larger one came first would cancel more than the cap requires.
			if r.OrderID == o.OrderID {
				return false
			}
			continue
		}
		used += r.Remaining
		if r.OrderID == o.OrderID {
			return true
		}
	}
	// `o` was not in the set at all, which means rule 1 already disqualified it.
	return false
}

// selfCrosses is H-CO-6 evaluated over what we actually hold.
//
// The test is best-against-best. Both sides are bids, so the highest YES and the
// highest NO are the pair most likely to sum past 100; if they do not cross,
// nothing behind them can.
func (a *adoptionPolicy) selfCrosses(o rest.Order, f lifecycle.AdoptionFacts) bool {
	best := func(side quote.Side) (int, bool) {
		top, found := 0, false
		for _, r := range sameSide(f, o.Ticker, side) {
			if !found || r.PriceCents > top {
				top, found = r.PriceCents, true
			}
		}
		return top, found
	}
	yes, hasYes := best(quote.SideYes)
	no, hasNo := best(quote.SideNo)
	if !hasYes || !hasNo {
		return false
	}
	return quote.Crosses(yes, no)
}

// fitsUnderCapital is H-CAP-1, H-CAP-2 and H-CAP-3 applied to an adopted adding
// order.
//
// The budget is computed against an exposure picture that EXCLUDES every adopted
// adding order and includes everything else -- the held positions and the
// resting reducers. That is the only formulation that answers a useful question:
// `AddingBudget` returns the headroom remaining after the exposures it is given,
// so handing it a picture that already contained the orders under test would
// return the headroom after them, which is zero-or-negative exactly when they
// fit and when they do not.
//
// So: subtract what is committed and cannot be released, then fill the remaining
// headroom with the ranked adding orders and keep the prefix that fits. H-CAP-4
// is satisfied by construction rather than by an argument -- the reducers are
// counted first, so their collateral is never available to an adding order.
//
// # Held positions are valued at settlement, deliberately
//
// `AdoptionFacts` carries `Positions` as quantities and no entry prices, because
// §7.5 step 1 seeds `q` from `GET /portfolio/positions` and that endpoint's
// answer is the position, not what it cost. Rather than invent a mark, this
// values a held contract at `risk.SettlementPrice4` -- $1.00, the most it can
// ever be worth. That OVERSTATES committed collateral, which is the safe
// direction here: it shrinks the adding budget, so the error can only cancel an
// adopted adding order that a truer mark would have kept. The alternative error
// -- understating what the position ties up and keeping an order that H-CAP-1
// does not cover -- is the one that produces an `insufficient_balance` reject,
// and H-CAP-5 makes that a correctness failure and a global WINDING_DOWN rather
// than a market condition.
func (a *adoptionPolicy) fitsUnderCapital(o rest.Order, f lifecycle.AdoptionFacts) bool {
	byTicker := make(map[string]*risk.Exposure)
	get := func(ticker string) *risk.Exposure {
		e, ok := byTicker[ticker]
		if !ok {
			e = &risk.Exposure{Ticker: ticker}
			byTicker[ticker] = e
		}
		return e
	}

	for ticker, q := range f.Positions {
		if q == 0 {
			continue
		}
		get(ticker).Position += risk.SideCost(q, risk.SettlementPrice4)
	}
	for _, r := range f.OwnedResting {
		if r.OrderID == "" || r.Remaining <= 0 || r.Fractional ||
			!quote.ValidPrice(r.PriceCents) {
			continue
		}
		reducing, held := quote.ReducingSide(f.Positions[r.Ticker])
		if !held || r.Side != reducing {
			// An adding order, or an order on a flat market where every order
			// adds. It is a candidate rather than a commitment, so it is left
			// out of the picture the budget is computed from.
			continue
		}
		get(r.Ticker).Reducing += risk.SideCost(r.Remaining, r.Price4)
	}

	ex := make([]risk.Exposure, 0, len(byTicker))
	for _, e := range byTicker {
		ex = append(ex, *e)
	}
	// Deterministic order. `AddingBudget` sums over the slice and picks out one
	// ticker, so the arithmetic does not depend on it -- but `num.Money` is an
	// int64 and a stable input is worth having when a figure has to be
	// reproduced from a log.
	sort.Slice(ex, func(i, j int) bool { return ex[i].Ticker < ex[j].Ticker })

	// `reducerNeed` is zero because every reducer under consideration is already
	// RESTING, and its collateral is therefore already counted above. H-CAP-4's
	// figure is collateral the exits require and do NOT yet hold, and at
	// adoption there is no such thing: nothing has been sized against a budget
	// yet, because nothing has been quoted yet.
	budget := risk.AddingBudget(o.Ticker, ex, 0, a.p)
	if budget <= 0 {
		return false
	}

	var used num.Money
	for _, r := range rank(candidates(f, o.Ticker)) {
		cost := risk.SideCost(r.Remaining, r.Price4)
		if used+cost > budget {
			if r.OrderID == o.OrderID {
				return false
			}
			continue
		}
		used += cost
		if r.OrderID == o.OrderID {
			return true
		}
	}
	return false
}

// candidates is every well-formed adding order on one market, both sides.
//
// Both sides, because the capital cap is per MARKET (H-CAP-2) and a two-sided
// quote commits collateral on each of them -- §10.2's formula is evaluated one
// side at a time and summed, precisely because the two sides carry different
// quantities once the quote is skewed.
func candidates(f lifecycle.AdoptionFacts, ticker string) []rest.Order {
	reducing, held := quote.ReducingSide(f.Positions[ticker])
	out := make([]rest.Order, 0, len(f.OwnedResting))
	for _, r := range f.OwnedResting {
		if r.Ticker != ticker {
			continue
		}
		if r.OrderID == "" || r.Remaining <= 0 || r.Fractional ||
			!quote.ValidPrice(r.PriceCents) {
			continue
		}
		if held && r.Side == reducing {
			continue
		}
		out = append(out, r)
	}
	return out
}

func selected(set []string, ticker string) bool {
	for _, s := range set {
		if s == ticker {
			return true
		}
	}
	return false
}

// confidence: high

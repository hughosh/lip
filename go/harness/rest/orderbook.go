package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"lip/core"
	"lip/harness/num"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// H-CO-3 — the orderbook read, and H-FAIL-6's comparison
// ---------------------------------------------------------------------------
//
// F5 is the only reader of this file: "book silent > 60s while socket healthy
// -> GET /markets/{t}/orderbook, compare prices *and sizes* through the full
// Target Size walk on both sides, including our own expected resting size
// (H-FAIL-6); agree -> reset the staleness clock, keep quoting. Disagree ->
// that market's book is replaced and quarantined; market -> REDUCING."
//
// Two halves, deliberately separated. `Client.Orderbook` does I/O and parsing
// and holds no opinion; `CompareBooks` is pure and holds every opinion. That
// split is what lets the REST read happen on a goroutine that may block, while
// the comparison -- which has to see `core.Rig`'s live book and `risk.Portfolio`
// -- runs on the owner goroutine that alone may touch either.

// BookLevel is one resting level of one side, in that side's OWN cents.
//
// `Cents` and `Size` are the only values any comparison reads, and both are
// derived through `core`'s parsers so that a REST level and a websocket level
// that describe the same resting liquidity are the same two numbers. `Wire` is
// the pair exactly as the exchange sent it, kept so that a book we decide to
// REPLACE is replaced through `core.Book.ApplySnapshot` -- which parses the
// strings itself -- rather than through numbers this file re-formatted.
type BookLevel struct {
	Cents int
	Size  float64
	Wire  [2]string
}

// OrderbookOutcome is whether a cross-check read landed, and how it failed.
type OrderbookOutcome uint8

const (
	// OrderbookUnset is the zero value and licenses nothing. Same argument as
	// `ScheduleUnset` and `WalkUnset`: an unpopulated result describes an empty
	// book on both sides, which is a plausible-looking fact and a catastrophic
	// one to compare against.
	OrderbookUnset OrderbookOutcome = iota
	// OrderbookRead is a complete, validated book. Only this outcome may be
	// compared or installed.
	OrderbookRead
	// OrderbookGranularity is H-CO-3a: a resting price that is not an exact
	// integer cent. It is NOT a failed read -- the exchange answered, and what
	// it said is that the tick size has changed -- and it is not a book that may
	// replace anything, because every price comparison downstream is
	// integer-cent arithmetic.
	OrderbookGranularity
	// OrderbookFailed is transport, status, decode, shape or parse: we do not
	// know what the resting book is. It is never agreement.
	OrderbookFailed
)

func (o OrderbookOutcome) String() string {
	switch o {
	case OrderbookRead:
		return "read"
	case OrderbookGranularity:
		return "granularity"
	case OrderbookFailed:
		return "failed"
	}
	return "unset"
}

// OrderbookResult is one market's resting book as the exchange stated it.
//
// The outcome is embedded rather than returned alongside, for the reason
// `OrdersResult` embeds `Walk`: a caller cannot reach the levels without also
// holding the answer to "did this land", and `(book, error)` has the same
// information in it with none of that property.
type OrderbookResult struct {
	Outcome OrderbookOutcome
	Ticker  string

	// Yes and No are DESCENDING by price -- best first. The endpoint sends them
	// best LAST (H-CO-3) and `Orderbook` reverses them, because every walk in
	// this system runs from the touch downwards and a walk over the wire order
	// would read the far end of the book as the touch.
	Yes, No []BookLevel

	Anomalies []risk.Anomaly
	Err       error
}

// Read reports whether this result may be compared or installed.
func (r OrderbookResult) Read() bool { return r.Outcome == OrderbookRead }

// Snapshot renders the result as the two wire arrays `core.Book.ApplySnapshot`
// takes, in the ORDER THE EXCHANGE SENT THEM.
//
// The order is restored deliberately. `core.Levels` is insertion-ordered and
// `Levels.Sum` is CPython-compatible compensated summation over that order
// (port-spec P2, P24), so a book installed best-first would sum by one ULP
// differently from the same book installed by a websocket snapshot. The
// replacement has to be indistinguishable from the frame it stands in for.
func (r OrderbookResult) Snapshot() (yes, no [][2]string) {
	return wireOf(r.Yes), wireOf(r.No)
}

// wireOf reverses a descending side back to the endpoint's best-last order.
func wireOf(levels []BookLevel) [][2]string {
	out := make([][2]string, 0, len(levels))
	for i := len(levels) - 1; i >= 0; i-- {
		out = append(out, levels[i].Wire)
	}
	return out
}

// Orderbook reads GET /markets/{ticker}/orderbook (H-CO-3).
//
// It is a single object with no cursor, so it does not go through `Walk` -- the
// guarded walk exists to stop a paginated read returning page one as the whole
// answer, and there is no page one here. The register is `Schedule`'s and
// `Balance`'s: a non-200 is an ANSWER and not an error, and every field is
// either accounted for or the read fails.
//
// # What it refuses, and why each refusal is not pedantry
//
//   - an absent `orderbook_fp`, or an absent `yes_dollars` / `no_dollars`. An
//     absent array and an empty one are the same JSON to a lenient decoder and
//     opposite facts to F5: "there is no liquidity on this side" would fail the
//     Target Size walk and send a healthy market to REDUCING, and the reverse
//     shape -- reading absence as agreement -- would bless a book we never saw.
//   - a price that is not an exact integer cent. That is H-CO-3a, and the
//     response is `SEV2 BOOK_PRICE_GRANULARITY` rather than a parse error,
//     because it is a change in the exchange's tick size and not a bad byte.
//   - a side that does not arrive strictly ascending. H-CO-3 states the order
//     ("best level last") and this reverses it; a repeated or out-of-order price
//     means the reversal produced something that is not a book, and walking it
//     would compare the wrong levels against each other.
func (c *Client) Orderbook(ctx context.Context, ticker string) OrderbookResult {
	if err := validateTicker(ticker); err != nil {
		return orderbookFailed(ticker, err)
	}
	// The ticker is interpolated into the path, which is also the string the
	// signature covers. `validateTicker` runs BEFORE the Doer is touched, so a
	// ticker that would re-target the request cannot reach the wire.
	resp, err := c.Doer.Do(ctx, Request{
		Method: "GET", Path: "/markets/" + ticker + "/orderbook",
	})
	if err != nil {
		return orderbookFailed(ticker, fmt.Errorf("orderbook %s: %w", ticker, err))
	}
	if resp.Status == 429 {
		return orderbookFailed(ticker, fmt.Errorf("orderbook %s: %w", ticker, rateLimitError(resp)))
	}
	if resp.Status != 200 {
		return orderbookFailed(ticker, fmt.Errorf("orderbook %s: HTTP %d: %s",
			ticker, resp.Status, snippet(resp.Body)))
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return orderbookFailed(ticker, fmt.Errorf("orderbook %s: undecodable: %w",
			ticker, err))
	}
	rawBook, ok := body["orderbook_fp"]
	if !ok || string(rawBook) == "null" {
		return orderbookFailed(ticker, fmt.Errorf("orderbook %s: no orderbook_fp "+
			"object; H-CO-3 names it as where the levels live, and a response "+
			"without it is a shape this harness has not measured", ticker))
	}
	var sides map[string]json.RawMessage
	if err := json.Unmarshal(rawBook, &sides); err != nil {
		return orderbookFailed(ticker, fmt.Errorf("orderbook %s: orderbook_fp is "+
			"not an object: %w", ticker, err))
	}

	res := OrderbookResult{Outcome: OrderbookRead, Ticker: ticker}
	for _, side := range [...]struct {
		key string
		dst *[]BookLevel
	}{{"yes_dollars", &res.Yes}, {"no_dollars", &res.No}} {

		raw, ok := sides[side.key]
		if !ok || string(raw) == "null" {
			return orderbookFailed(ticker, fmt.Errorf("orderbook %s: "+
				"orderbook_fp has no %s array; an ABSENT side and an empty one "+
				"are the same bytes to a lenient decoder and opposite facts to "+
				"F5", ticker, side.key))
		}
		var pairs [][]string
		if err := json.Unmarshal(raw, &pairs); err != nil {
			return orderbookFailed(ticker, fmt.Errorf("orderbook %s: %s is not "+
				"an array of [price, size] STRING pairs (%w); the `_fp` suffix "+
				"is the exchange's own mark that these are fixed-point strings, "+
				"and a bare number here is a wire change rather than a value to "+
				"coerce", ticker, side.key, err))
		}
		wire := make([][2]string, len(pairs))
		for i, pair := range pairs {
			if len(pair) != 2 {
				return orderbookFailed(ticker, fmt.Errorf("orderbook %s: %s level %d needs exactly two fields", ticker, side.key, i))
			}
			wire[i] = [2]string{pair[0], pair[1]}
		}
		levels, anom, err := parseRESTLevels(ticker, side.key, wire)
		if err != nil {
			return orderbookFailed(ticker, err)
		}
		if anom != nil {
			// H-CO-3a. The read LANDED and what it established is that the
			// resting book is no longer integer-cent. Nothing here is
			// comparable and nothing here may replace a book.
			return OrderbookResult{
				Outcome: OrderbookGranularity, Ticker: ticker,
				Anomalies: []risk.Anomaly{*anom},
				Err: fmt.Errorf("orderbook %s: %s carries a level that is not an "+
					"integer cent", ticker, side.key),
			}
		}
		*side.dst = levels
	}
	return res
}

// parseRESTLevels validates one side and returns it DESCENDING by price.
//
// Prices go through `core.ParsePriceCents` and sizes through `core.ParseSize`,
// because H-CO-3 permits no second parser for either: `core` is what the
// websocket book is built with, and a comparison between two books parsed by
// two decoders is a comparison of the decoders.
//
// `ParsePrice4` is used FIRST and only to enforce H-CO-3a. It is exact integer
// arithmetic end to end, where `ParsePriceCents` rounds -- so it is the only
// thing here that can tell a 58c level from a 58.5c one, and asking
// `ParsePriceCents` that question would get the answer 58 either way. It is not
// a second book parser: the value handed onwards is `core`'s.
func parseRESTLevels(ticker, field string, wire [][2]string) ([]BookLevel,
	*risk.Anomaly, error) {

	out := make([]BookLevel, 0, len(wire))
	for i, lv := range wire {
		p4, err := ParsePrice4(lv[0])
		if err != nil {
			return nil, &risk.Anomaly{
				Class: "BOOK_PRICE_GRANULARITY", Sev: risk.SEV2, Ticker: ticker,
				Text: fmt.Sprintf("REST book price %q in %s did not parse as an "+
					"exact fixed-point price (%v); the level cannot be trusted "+
					"and no book may be replaced from it", lv[0], field, err),
			}, nil
		}
		if _, exact := CentsExact(p4); !exact {
			return nil, &risk.Anomaly{
				Class: "BOOK_PRICE_GRANULARITY", Sev: risk.SEV2, Ticker: ticker,
				Text: fmt.Sprintf("REST book price %q in %s is %d/1e4 dollars, "+
					"which is not an integer cent; H-CO-3a asserts integer-cent "+
					"resting prices rather than assuming them, and tick size is "+
					"the exchange's to change", lv[0], field, p4),
			}, nil
		}
		cents, err := core.ParsePriceCents(lv[0])
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s level %d price %q: %w",
				ticker, field, i, lv[0], err)
		}
		if got, _ := CentsExact(p4); got != cents {
			// Unreachable while both parsers are correctly rounded over an
			// exact integer cent, and written anyway: the two disagreeing is
			// the one condition under which `Cents` would not be the number
			// `core` believes, and a silent disagreement here is a comparison
			// against a book that does not exist.
			return nil, nil, fmt.Errorf("%s %s level %d price %q: core says %dc "+
				"and the exact parser says %dc", ticker, field, i, lv[0], cents, got)
		}
		size, err := core.ParseSize(lv[1])
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s level %d size %q: %w",
				ticker, field, i, lv[1], err)
		}
		if math.IsNaN(size) || math.IsInf(size, 0) || size < 0 {
			return nil, nil, fmt.Errorf("%s %s invalid size %q", ticker, field, lv[1])
		}
		if size <= 0 {
			// `core.parseLevels` filters `if size > 0` (rig.py:165), so a
			// zero-size level is absent from the websocket book. Keeping it here
			// would make an identical book compare as different.
			continue
		}
		out = append(out, BookLevel{Cents: cents, Size: size, Wire: lv})
	}

	// The reversal H-CO-3 asks for: "best level last (ascending by price).
	// Reverse before use."
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	for i := 1; i < len(out); i++ {
		if out[i].Cents >= out[i-1].Cents {
			return nil, nil, fmt.Errorf("%s %s is not strictly ascending on the "+
				"wire (%dc then %dc after reversal); H-CO-3 states the order and "+
				"a reversal of something that is not sorted is not a book",
				ticker, field, out[i-1].Cents, out[i].Cents)
		}
	}
	return out, nil, nil
}

func orderbookFailed(ticker string, err error) OrderbookResult {
	return OrderbookResult{Outcome: OrderbookFailed, Ticker: ticker, Err: err}
}

// ---------------------------------------------------------------------------
// H-FAIL-6 — the comparison
// ---------------------------------------------------------------------------

// OwnResting is our own aggregate resting size at one price, in that side's own
// cents.
//
// "Ours" here means EXCHANGE-CONFIRMED resting, including an order whose cancel
// we have requested and the exchange has not confirmed absent (H-FAIL-3: until
// then the order "is live and fillable and its full quantity remains in the risk
// model"). It deliberately does NOT include a create we have sent and not had
// listed back: an order the exchange has never told us rests is one we cannot
// require either book to show, and demanding it would turn every in-flight
// placement into a wedged-feed disagreement.
type OwnResting struct {
	Cents int
	Size  num.Qty
}

// SideBooks is one side of both books, plus what we believe rests there.
type SideBooks struct {
	// WS and REST are DESCENDING by price, best first.
	WS, REST []BookLevel
	Ours     []OwnResting
}

// BookCheck is one whole F5 cross-check, as data.
type BookCheck struct {
	// Target is the market's LIP Target Size -- the same `core.Book.Target` the
	// qualifying walk uses.
	Target  float64
	Yes, No SideBooks
}

// BookVerdict is what the comparison established. `Why` is empty on agreement
// and is the operator's SEV2 text otherwise.
type BookVerdict struct {
	Agree bool
	Why   string
}

// CompareBooks is H-FAIL-6, and it is pure: no clock, no I/O, no state.
//
// > "F5's REST cross-check walks both sides to Target Size, comparing price and
// > size at every level, and additionally checks that our own resting size
// > appears where we believe it is."
//
// Red-team HR-015 is why size is in that sentence: the websocket shows YES
// 50x1,300 with Target 1,000, it has missed a size delta of -1,290, and the real
// book is YES 50x10. The TOUCH PRICES AGREE. A cross-check that compared prices
// would reset the staleness clock and keep quoting into a market that no longer
// qualifies, with an `external_best` subtraction that is now wrong. Everything
// F5 exists to protect depends on size.
//
// # Disagreement is the answer to every uncertainty
//
// A side that never reaches Target Size, a walk that ran out of levels, a target
// of zero: all of them are disagreement, not agreement. The only thing that
// resets the staleness clock is a complete walk on BOTH sides that matched at
// every level -- because agreement is what licenses quoting to continue into a
// market we already know has gone silent.
func CompareBooks(c BookCheck) BookVerdict {
	if c.Target <= 0 || math.IsNaN(c.Target) || math.IsInf(c.Target, 0) {
		return BookVerdict{Why: fmt.Sprintf("the market has no Target Size "+
			"(%v), so H-FAIL-6's walk cannot be performed and nothing can "+
			"establish that the silent book is right", c.Target)}
	}
	for _, side := range [...]struct {
		name string
		bk   SideBooks
	}{{"yes", c.Yes}, {"no", c.No}} {
		if v := compareSide(side.name, c.Target, side.bk); !v.Agree {
			return v
		}
	}
	return BookVerdict{Agree: true}
}

func compareSide(name string, target float64, s SideBooks) BookVerdict {
	ws, wsReached := targetWalk(target, s.WS)
	rst, restReached := targetWalk(target, s.REST)

	if !wsReached || !restReached {
		return BookVerdict{Why: fmt.Sprintf("the %s side does not reach Target "+
			"Size %v on both books (websocket reached it: %v over %d level(s); "+
			"REST reached it: %v over %d level(s)); H-FAIL-6 requires the full "+
			"walk on both sides and a walk that ran out of depth has not "+
			"compared the liquidity the reward is paid for",
			name, target, wsReached, len(ws), restReached, len(rst))}
	}
	// The COMMON prefix first, so that the reason names the depth the two books
	// actually differ at. Comparing lengths first would report "one walk is
	// longer" for HR-015's case -- where the touch size is wrong, so the REST
	// walk needs an extra level to reach the target -- and the operator would be
	// told about the shape of the walk instead of about the missing 1,290
	// contracts that caused it.
	n := len(ws)
	if len(rst) < n {
		n = len(rst)
	}
	for i := 0; i < n; i++ {
		if ws[i].Cents != rst[i].Cents {
			return BookVerdict{Why: fmt.Sprintf("the %s side disagrees at depth "+
				"%d of the Target Size walk: the websocket says %dc and REST "+
				"says %dc", name, i+1, ws[i].Cents, rst[i].Cents)}
		}
		if !sameBookSize(ws[i].Size, rst[i].Size) {
			return BookVerdict{Why: fmt.Sprintf("the %s side disagrees at depth "+
				"%d of the Target Size walk: %dc is %v on the websocket and %v "+
				"on REST. A price-only check would have called this agreement "+
				"(HR-015) and kept quoting against depth that is not there",
				name, i+1, ws[i].Cents, ws[i].Size, rst[i].Size)}
		}
	}
	// Walks that matched on the grid at every common depth have the same
	// running total at each one, so they reach the target at the same depth and
	// this cannot fire while the walk and the comparison share the grid. It
	// stays so that if they ever part, two lengths are still disagreement and
	// never agreement: agreement must not skip a level from either walk.
	if len(ws) != len(rst) {
		return BookVerdict{Why: fmt.Sprintf("the %s side reaches Target Size %v "+
			"over %d websocket level(s) and %d REST level(s)",
			name, target, len(ws), len(rst))}
	}

	// H-FAIL-6's second clause, and it runs over the WHOLE side rather than the
	// Target Size prefix: our own resting order can be deeper than the walk and
	// is still ours, and "the size we believe is resting is not there" is the
	// discrepancy that costs money whatever depth it happens at.
	owned := make(map[int]num.Qty)
	for _, own := range s.Ours {
		if own.Size < 0 {
			return BookVerdict{Why: "negative expected owned size"}
		}
		if own.Size > num.Qty(math.MaxInt64)-owned[own.Cents] {
			return BookVerdict{Why: "expected owned size overflow"}
		}
		owned[own.Cents] += own.Size
	}
	for cents, size := range owned {
		if size == 0 {
			continue
		}
		own := OwnResting{Cents: cents, Size: size}
		for _, bk := range [...]struct {
			what   string
			levels []BookLevel
		}{{"websocket", s.WS}, {"REST", s.REST}} {

			have, found := sizeAt(bk.levels, own.Cents)
			if !found {
				return BookVerdict{Why: fmt.Sprintf("we believe %s of ours rests "+
					"on the %s side at %dc and the %s book has no level there at "+
					"all", own.Size.Wire(), name, own.Cents, bk.what)}
			}
			if q, ok := bookQty(have); !ok || q < own.Size {
				return BookVerdict{Why: fmt.Sprintf("we believe %s of ours rests "+
					"on the %s side at %dc and the %s book shows only %v there",
					own.Size.Wire(), name, own.Cents, bk.what, have)}
			}
		}
	}
	return BookVerdict{Agree: true}
}

// sameBookSize compares two book sizes on the exchange's 0.01-contract grid,
// H-CO-4a's quantum.
//
// The websocket book adds every delta into a float64 level
// (core.Book.ApplyDelta), so its sizes carry residue that grows with the
// number of deltas a level has absorbed. lip-2w3 forgave one adjacent step and
// production then raised four (lip-2mz, a29: 29.999999999999986 against 30).
// No step count bounds that residue; the grid does. Wire sizes are two-decimal
// fixed point (H-CO-2), so a real difference is at least one quantum, while
// residue stays many orders of magnitude below half of one.
func sameBookSize(a, b float64) bool {
	qa, okA := bookQty(a)
	qb, okB := bookQty(b)
	return okA && okB && qa == qb
}

// maxBookSize bounds the sizes bookQty puts on the grid. Below 2^44 contracts
// a double's spacing is at most 2^-9 of a contract, so a two-decimal size lands
// within about 0.16 of its own quantum after QtyFromFloat's multiply. From 2^45
// adjacent quanta collide -- 35184372088832.02 and .03 become one Qty -- and two
// books a quantum apart would compare as the same. The bound is also far inside
// Qty's int64 range, and orders of magnitude above any resting level.
const maxBookSize = 1e13

// bookQty puts one book size on the grid, or reports that it is not a size the
// grid can hold exactly: not finite, not positive, or not below maxBookSize.
//
// A size below half a quantum is ZERO quanta and still a level. On a
// two-decimal wire the websocket book holds one only as residue that outlived
// ApplyDelta's 1e-9 deletion at a price the exchange has emptied. Inside the
// walk that is a real disagreement -- the live book has a level the exchange
// does not -- and the comparison names it by its price, where refusing the size
// would have reported a walk that "does not reach" the Target.
func bookQty(size float64) (num.Qty, bool) {
	if !validBookSize(size) || size >= maxBookSize {
		return 0, false
	}
	return num.QtyFromFloat(size), true
}

func validBookSize(size float64) bool {
	return size > 0 && !math.IsInf(size, 0) && !math.IsNaN(size)
}

// targetWalk returns the levels through and INCLUDING the first one at which the
// cumulative size reaches `target`, and whether it got there.
//
// The running total is on the grid, like the level comparison (lip-2mz). A
// float64 total can land just under the target where the grid total lands
// exactly on it -- through residue in a websocket level, or through the naive
// sum's own rounding (0.3 + 0.6 < 0.9) -- and two walks over the same resting
// liquidity would then end at different depths, or one would never end.
// `core.Book.Qualifies` still totals the live book naively (rig.py:245,
// port-spec P9/P24), so on such a book it crosses the target one level deeper,
// where any further level covers the shortfall, or not at all, which is its
// gate's fail-closed answer. Neither is a disagreement between the books.
//
// `total.Float()` is exact while the total is below 2^53 quanta, some 9e13
// contracts, which no Target approaches.
func targetWalk(target float64, levels []BookLevel) ([]BookLevel, bool) {
	var total num.Qty
	for i := range levels {
		q, ok := bookQty(levels[i].Size)
		if !ok || q > math.MaxInt64-total {
			return levels[:i+1], false
		}
		total += q
		if total.Float() >= target {
			return levels[:i+1], true
		}
	}
	return levels, false
}

// sizeAt is the size at one price, or false if the side has no such level.
func sizeAt(levels []BookLevel, cents int) (float64, bool) {
	for _, lv := range levels {
		if lv.Cents == cents {
			return lv.Size, true
		}
	}
	return 0, false
}

// confidence: high

// ReducerBook retains REST depth separately from the websocket rig. Snapshot
// returns a fresh core book, so callers cannot mutate the retained source.
// Its timestamp is the request START, never completion of a slow read.
// The owner must also require Gate.RESTReducerActionable and reducing role.
type ReducerBook struct {
	result OrderbookResult
	at     time.Duration
	valid  bool
}

func (b *ReducerBook) Retain(r OrderbookResult, started time.Duration) bool {
	if !r.Read() {
		b.valid = false
		return false
	}
	b.result = r
	b.result.Yes = append([]BookLevel(nil), r.Yes...)
	b.result.No = append([]BookLevel(nil), r.No...)
	b.at, b.valid = started, true
	return true
}

func (b *ReducerBook) Invalidate() { b.valid = false }

func (b *ReducerBook) Snapshot(ticker string, target float64, now, maxAge time.Duration) (*core.Book, bool) {
	if !b.valid || ticker != b.result.Ticker || maxAge <= 0 || now < b.at || now-b.at > maxAge {
		return nil, false
	}
	book := core.NewBook(target)
	yes, no := b.result.Snapshot()
	if err := book.ApplySnapshot(yes, no); err != nil {
		return nil, false
	}
	return book, true
}

// confidence: high

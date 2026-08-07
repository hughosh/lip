package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// Price parsing — exact, and asserted to be integer cents (H-CO-3a)
// ---------------------------------------------------------------------------

// Price4 is a price in the exchange's own quantum: 1e-4 dollars. Every one of
// 79,526 sampled `yes_price_dollars` values carried exactly four decimals.
type Price4 = int64

// ParsePrice4 parses a dollar-denominated fixed-point string exactly.
//
// It does not go through float64 at any point. `strconv.ParseFloat("0.5800")`
// is not exactly 0.58, and multiplying that by 10000 and truncating is how a
// 58c order becomes a 57c order once in a while. The arithmetic here is
// integer throughout, so it either produces the exact value or an error.
//
// The grammar is the same narrow one num.ParseQty enforces, and for the same
// reason: ParseFloat accepts Go literal syntax ("1_000.00", "0x1p+10", "1e3"),
// none of which the wire emits, and each of which turns a string we do not
// understand into a confident price.
func ParsePrice4(s string) (Price4, error) {
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	if s[0] == '-' || s[0] == '+' {
		return 0, fmt.Errorf("price %q is signed; prices are non-negative", s)
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("price %q has more than one decimal point", s)
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("price %q has no digits", s)
	}
	if len(fracPart) > 4 {
		// Truncating here would silently discard precision the exchange chose
		// to send. If the quantum ever gets finer, this must surface as an
		// error and be handled, not rounded away.
		return 0, fmt.Errorf("price %q has %d decimals; the measured quantum "+
			"is 1e-4 dollars and this parser will not round", s, len(fracPart))
	}
	var v int64
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("price %q is not a fixed-point number", s)
		}
		v = v*10 + int64(c-'0')
		if v > 1<<40 {
			return 0, fmt.Errorf("price %q is out of range", s)
		}
	}
	v *= 10000
	scale := int64(1000)
	for _, c := range fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("price %q is not a fixed-point number", s)
		}
		v += int64(c-'0') * scale
		scale /= 10
	}
	return v, nil
}

// CentsExact converts a Price4 to whole cents, reporting whether it was exact.
//
// H-CO-3a: the resting book is integer-cent, and `harness/rest` ASSERTS that
// rather than assuming it. A fractional resting price is not a rounding
// problem, it is a change in the exchange's tick size, and the response is
// `SEV2 BOOK_PRICE_GRANULARITY` plus that market to `REDUCING` -- ten lines
// that turn a measured regularity into a detected anomaly.
func CentsExact(p Price4) (int, bool) {
	if p%100 != 0 {
		return 0, false
	}
	return int(p / 100), true
}

// ---------------------------------------------------------------------------
// Resting orders
// ---------------------------------------------------------------------------

// Order is one order on the account, in BOOK terms.
//
// Reads come back in the yes/no shape even though writes go out as bid/ask.
// `probebot.py:783-789` records this and it is the only verified-in-production
// statement of it in the repository:
//
//	"Reads still come back in the yes/no shape even though writes are
//	 bid/ask, so normalise here. `book_side` is authoritative when present:
//	 ask == a no bid, whatever `side` claims."
type Order struct {
	OrderID       string
	ClientOrderID string
	Ticker        string
	Side          quote.Side
	// Price4 is always populated. PriceCents is populated only when the price
	// is an exact integer cent; Fractional says which.
	Price4     Price4
	PriceCents int
	Fractional bool
	Remaining  num.Qty
	Status     string
	// Ours is true when the coid parses as one of ours (any run). H-ORD-5 step
	// 5 turns a false here into `SEV2 FOREIGN_ORDER`; what that means depends on
	// whether we are still in STARTING, which is the lifecycle's call, not this
	// package's.
	Ours   bool
	Parsed ParsedCoid
}

// OrdersResult embeds the Walk so a caller cannot reach the orders without
// having a Replaces() to check. H-POS-4 replaces the order map wholesale, but
// only on a complete walk: an incomplete response tells us nothing and must not
// retire an order from our model.
type OrdersResult struct {
	Walk
	Orders []Order
}

// Ours returns only the orders bearing a `lipH-` coid.
func (r OrdersResult) Ours() []Order {
	out := make([]Order, 0, len(r.Orders))
	for _, o := range r.Orders {
		if o.Ours {
			out = append(out, o)
		}
	}
	return out
}

// Foreign returns the orders on the account that are not ours.
func (r OrdersResult) Foreign() []Order {
	out := make([]Order, 0)
	for _, o := range r.Orders {
		if !o.Ours {
			out = append(out, o)
		}
	}
	return out
}

// Orders reads every order matching `status` as a complete cursor walk.
//
// `status` MUST be one of the pinned constants. The endpoint does not reject an
// unrecognised value, it answers "nothing" -- so a typo here is an empty result
// that reads exactly like "no orders rest", which is the single most dangerous
// wrong answer this endpoint can give.
func (c *Client) Orders(ctx context.Context, ticker, status string) OrdersResult {
	if err := ValidateStatus(status); err != nil {
		return OrdersResult{Walk: Walk{Outcome: WalkFailed, Err: err}}
	}
	filters := url.Values{}
	if ticker != "" {
		filters.Set("ticker", ticker)
	}
	if status != "" {
		filters.Set("status", status)
	}

	w := c.Walk(ctx, EpOrders, filters)
	if !w.Replaces() {
		return OrdersResult{Walk: w}
	}

	recs := w.Records("orders")
	out := make([]Order, 0, len(recs))
	for i, raw := range recs {
		o, anoms, err := decodeOrder(raw)
		if err != nil {
			// One undecodable order invalidates the whole walk. It cannot be
			// skipped: a resting order we failed to parse is resting size we
			// would then believe is absent, and H-FAIL-3 is explicit that "off"
			// means exchange-confirmed absent -- not "we could not read it".
			return OrdersResult{Walk: Walk{
				Outcome:   WalkFailed,
				Pages:     w.Pages,
				Anomalies: w.Anomalies,
				Err:       fmt.Errorf("orders record %d: %w", i, err),
			}}
		}
		w.Anomalies = append(w.Anomalies, anoms...)
		out = append(out, o)
	}
	return OrdersResult{Walk: w, Orders: out}
}

func decodeOrder(raw json.RawMessage) (Order, []risk.Anomaly, error) {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Order{}, nil, fmt.Errorf("not an object: %w", err)
	}
	var o Order
	o.OrderID = scalar(rec["order_id"])
	o.ClientOrderID = scalar(rec["client_order_id"])
	o.Ticker = scalar(rec["ticker"])
	if o.Ticker == "" {
		o.Ticker = scalar(rec["market_ticker"])
	}
	o.Status = scalar(rec["status"])
	if o.OrderID == "" {
		return Order{}, nil, fmt.Errorf("no order_id")
	}
	o.Parsed, o.Ours = ParseCoid(o.ClientOrderID)

	// --- side: book_side wins ------------------------------------------
	side, err := readSide(rec)
	if err != nil {
		return Order{}, nil, err
	}
	o.Side = side

	// --- price ---------------------------------------------------------
	p4, err := readPrice(rec, side)
	if err != nil {
		return Order{}, nil, err
	}
	o.Price4 = p4
	cents, exact := CentsExact(p4)
	o.PriceCents, o.Fractional = cents, !exact

	var anoms []risk.Anomaly
	if !exact {
		anoms = append(anoms, risk.Anomaly{
			Class: "BOOK_PRICE_GRANULARITY", Sev: risk.SEV2, Ticker: o.Ticker,
			Text: fmt.Sprintf("resting order %s is priced at %d/1e4 dollars, "+
				"which is not an integer cent; H-CO-3a asserts integer-cent "+
				"resting prices rather than assuming them, and tick size is "+
				"the exchange's to change", o.OrderID, p4),
		})
	}

	// --- remaining size ------------------------------------------------
	rem := scalar(rec["remaining_count_fp"])
	if rem == "" {
		rem = scalar(rec["remaining_count"])
	}
	if rem == "" {
		return Order{}, nil, fmt.Errorf("order %s has neither "+
			"remaining_count_fp nor remaining_count", o.OrderID)
	}
	q, err := num.ParseQty(rem)
	if err != nil {
		return Order{}, nil, fmt.Errorf("order %s: %w", o.OrderID, err)
	}
	o.Remaining = q
	return o, anoms, nil
}

// readSide normalises an order record onto a book side.
func readSide(rec map[string]json.RawMessage) (quote.Side, error) {
	if bs := scalar(rec["book_side"]); bs != "" {
		// H-CO-1 backwards. `ask` is a NO bid whatever `side` claims, which is
		// the whole reason book_side is authoritative: a reconciliation that
		// mistook a NO bid at 42c for a YES bid at 58c would adopt the position
		// with the wrong sign.
		switch BookSide(bs) {
		case Bid:
			return quote.SideYes, nil
		case Ask:
			return quote.SideNo, nil
		default:
			return 0, fmt.Errorf("unknown book_side %q", bs)
		}
	}
	s := scalar(rec["side"])
	if s == "" {
		s = scalar(rec["outcome_side"])
	}
	switch s {
	case "yes":
		return quote.SideYes, nil
	case "no":
		return quote.SideNo, nil
	}
	return 0, fmt.Errorf("no usable side: book_side absent and side is %q", s)
}

// readPrice takes the side's own price field, and cross-checks it against the
// opposite one when both are present.
func readPrice(rec map[string]json.RawMessage, side quote.Side) (Price4, error) {
	yesRaw, noRaw := scalar(rec["yes_price_dollars"]), scalar(rec["no_price_dollars"])

	var yes4, no4 Price4
	var haveYes, haveNo bool
	if yesRaw != "" {
		v, err := ParsePrice4(yesRaw)
		if err != nil {
			return 0, fmt.Errorf("yes_price_dollars: %w", err)
		}
		yes4, haveYes = v, true
	}
	if noRaw != "" {
		v, err := ParsePrice4(noRaw)
		if err != nil {
			return 0, fmt.Errorf("no_price_dollars: %w", err)
		}
		no4, haveNo = v, true
	}

	// A complementary pair must sum to $1.00 exactly. This is integer
	// arithmetic on the exchange's own quantum, so it is an exact test, and it
	// is the cheapest possible detector for the read half of H-CO-1 going
	// wrong: if the two fields ever stop being complements, every price this
	// package returns is suspect and we should say so rather than pick one.
	if haveYes && haveNo && yes4+no4 != 10000 {
		return 0, fmt.Errorf("yes %d/1e4 + no %d/1e4 = %d/1e4, not 10000: the "+
			"two price fields are not complements and H-CO-1 cannot be applied",
			yes4, no4, yes4+no4)
	}

	switch side {
	case quote.SideYes:
		if haveYes {
			return yes4, nil
		}
		if haveNo {
			return 10000 - no4, nil
		}
	default:
		if haveNo {
			return no4, nil
		}
		if haveYes {
			return 10000 - yes4, nil
		}
	}
	return 0, fmt.Errorf("no price field present")
}

// ---------------------------------------------------------------------------
// Positions
// ---------------------------------------------------------------------------

// PositionsResult is q per market, YES-positive (§8.1).
type PositionsResult struct {
	Walk
	ByTicker map[string]num.Qty
}

// Positions seeds q for every market with a non-zero position, INCLUDING
// markets not in the selection set (H-ORD-5 step 1, H-ORD-5b).
//
// It takes no ticker filter. The managed set is the union of selected markets
// and every market we hold, so a filtered read is structurally incapable of
// answering the question this endpoint is asked.
func (c *Client) Positions(ctx context.Context) PositionsResult {
	w := c.Walk(ctx, EpPositions, nil)
	if !w.Replaces() {
		return PositionsResult{Walk: w}
	}
	recs := w.Records("market_positions")
	byTicker := make(map[string]num.Qty, len(recs))
	for i, raw := range recs {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rec); err != nil {
			return PositionsResult{Walk: Walk{Outcome: WalkFailed, Pages: w.Pages,
				Err: fmt.Errorf("market_positions record %d: %w", i, err)}}
		}
		ticker := scalar(rec["ticker"])
		if ticker == "" {
			return PositionsResult{Walk: Walk{Outcome: WalkFailed, Pages: w.Pages,
				Err: fmt.Errorf("market_positions record %d has no ticker", i)}}
		}
		// `position_fp` is the net YES-equivalent, and is the field
		// probebot.py:805 reads. H-CO-4: counts are fixed-point, not integers.
		val := scalar(rec["position_fp"])
		if val == "" {
			val = scalar(rec["position"])
		}
		if val == "" {
			return PositionsResult{Walk: Walk{Outcome: WalkFailed, Pages: w.Pages,
				Err: fmt.Errorf("%s: no position_fp or position field", ticker)}}
		}
		q, err := num.ParseQty(val)
		if err != nil {
			// Refusing the parse leaves an error to escalate. Defaulting to
			// zero would report a held market as flat, and flat is the answer
			// that lets REDUCING reach IDLE and lets the process exit.
			return PositionsResult{Walk: Walk{Outcome: WalkFailed, Pages: w.Pages,
				Err: fmt.Errorf("%s position: %w", ticker, err)}}
		}
		byTicker[ticker] = q
	}
	return PositionsResult{Walk: w, ByTicker: byTicker}
}

// ---------------------------------------------------------------------------
// Fills
// ---------------------------------------------------------------------------

// Fill is one of our fills. `trade_id` is the join key against `rig.db`
// (H-ORD-6); `fill_id` is a second, distinct identity used to page safely.
type Fill struct {
	FillID   string
	TradeID  string
	OrderID  string
	Ticker   string
	Side     quote.Side
	Price4   Price4
	Count    num.Qty
	IsTaker  bool
	FeeCost  string
	TsMillis int64
}

// FillsResult is a complete fills walk.
type FillsResult struct {
	Walk
	Fills []Fill
}

// Takers returns any fill with `is_taker` true.
//
// H-ORD-8: this must be empty forever. One such fill means H-Q-3 has been
// violated by something -- a post_only that did not take effect, a marketable
// price, an API change -- and it is `SEV1` plus global `WINDING_DOWN`. It is
// the cheapest possible detector for the most expensive possible bug, and
// H-PAGE-1a records that it silently stops working the moment our fills exceed
// one page, which is why this rides on the complete walk and nothing else.
func (r FillsResult) Takers() []Fill {
	out := make([]Fill, 0)
	for _, f := range r.Fills {
		if f.IsTaker {
			out = append(out, f)
		}
	}
	return out
}

// Fills reads our fills as a complete cursor walk, then filters client-side to
// those at or after `since`.
//
// The time filter is applied AFTER the walk, deliberately. Stopping the walk
// early on a timestamp would make it an incomplete walk under H-PAGE-1 clause
// 2, and "complete" is the only thing that licenses replacing state. A zero
// `since` disables the filter.
func (c *Client) Fills(ctx context.Context, ticker string, since time.Time) FillsResult {
	filters := url.Values{}
	if ticker != "" {
		filters.Set("ticker", ticker)
	}
	w := c.Walk(ctx, EpFills, filters)
	if !w.Replaces() {
		return FillsResult{Walk: w}
	}
	recs := w.Records("fills")
	out := make([]Fill, 0, len(recs))
	for i, raw := range recs {
		f, err := decodeFill(raw)
		if err != nil {
			return FillsResult{Walk: Walk{Outcome: WalkFailed, Pages: w.Pages,
				Anomalies: w.Anomalies,
				Err:       fmt.Errorf("fills record %d: %w", i, err)}}
		}
		if !since.IsZero() && f.TsMillis != 0 &&
			f.TsMillis < since.UnixMilli() {
			continue
		}
		out = append(out, f)
	}
	return FillsResult{Walk: w, Fills: out}
}

func decodeFill(raw json.RawMessage) (Fill, error) {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Fill{}, fmt.Errorf("not an object: %w", err)
	}
	var f Fill
	f.FillID = scalar(rec["fill_id"])
	f.TradeID = scalar(rec["trade_id"])
	f.OrderID = scalar(rec["order_id"])
	f.Ticker = scalar(rec["ticker"])
	if f.Ticker == "" {
		f.Ticker = scalar(rec["market_ticker"])
	}
	f.FeeCost = scalar(rec["fee_cost"])
	if f.TradeID == "" {
		// H-ORD-6 makes trade_id our_fill's primary key and the join against
		// rig.db. A fill without one cannot be attributed at all.
		return Fill{}, fmt.Errorf("fill %q has no trade_id", f.FillID)
	}

	v, ok := rec["is_taker"]
	if !ok {
		return Fill{}, fmt.Errorf("fill %s has no is_taker field; H-ORD-8's "+
			"taker detector cannot be evaluated without it", f.TradeID)
	}
	// Decoded through a POINTER so that JSON null is distinguishable from
	// false. `json.Unmarshal([]byte("null"), &someBool)` succeeds and leaves
	// the bool at false — "unmarshaling a JSON null into any other Go type has
	// no effect on the value and produces no error". H-ORD-8's detector is the
	// cheapest detector for the most expensive bug in the system, and a null
	// that read as `is_taker: false` would hide continued taker execution until
	// the account was exhausted.
	var isTaker *bool
	if err := json.Unmarshal(v, &isTaker); err != nil {
		return Fill{}, fmt.Errorf("fill %s: is_taker is not a bool: %w",
			f.TradeID, err)
	}
	if isTaker == nil {
		return Fill{}, fmt.Errorf("fill %s has a null is_taker; H-ORD-8 must "+
			"never fail open, and null is not false", f.TradeID)
	}
	f.IsTaker = *isTaker

	side, err := readSide(rec)
	if err != nil {
		return Fill{}, fmt.Errorf("fill %s: %w", f.TradeID, err)
	}
	f.Side = side
	p4, err := readPrice(rec, side)
	if err != nil {
		return Fill{}, fmt.Errorf("fill %s: %w", f.TradeID, err)
	}
	f.Price4 = p4

	cnt := scalar(rec["count_fp"])
	if cnt == "" {
		cnt = scalar(rec["count"])
	}
	if cnt == "" {
		return Fill{}, fmt.Errorf("fill %s has no count_fp or count", f.TradeID)
	}
	q, err := num.ParseQty(cnt)
	if err != nil {
		return Fill{}, fmt.Errorf("fill %s: %w", f.TradeID, err)
	}
	f.Count = q

	if v := scalar(rec["ts"]); v != "" {
		f.TsMillis = parseTsMillis(v)
	}
	return f, nil
}

// parseTsMillis reads a `ts` field that may be seconds or milliseconds. It
// returns 0 when it cannot tell, and a 0 disables the client-side time filter
// for that record rather than silently excluding it.
func parseTsMillis(s string) int64 {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
	}
	if v == 0 {
		return 0
	}
	// Seconds since epoch are ~1.7e9; milliseconds are ~1.7e12.
	if v < 1e11 {
		return v * 1000
	}
	return v
}

// ---------------------------------------------------------------------------
// Balance
// ---------------------------------------------------------------------------

// Balance is the account's cash, in cents. It seeds the capital model
// (H-ORD-5 step 4).
type Balance struct {
	Cents int64
}

// Balance reads GET /portfolio/balance. It is not a list and has no cursor.
func (c *Client) Balance(ctx context.Context) (Balance, error) {
	resp, err := c.Doer.Do(ctx, Request{Method: "GET", Path: "/portfolio/balance"})
	if err != nil {
		return Balance{}, err
	}
	if resp.Status != 200 {
		return Balance{}, fmt.Errorf("balance: HTTP %d: %s",
			resp.Status, snippet(resp.Body))
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &rec); err != nil {
		return Balance{}, fmt.Errorf("balance: undecodable: %w", err)
	}
	v, ok := rec["balance"]
	if !ok {
		return Balance{}, fmt.Errorf("balance response has no balance field")
	}
	// Through a pointer, for the same reason as is_taker: a JSON null decodes
	// into an int64 as 0 with no error. A balance that read as $0.00 would
	// leave a held position without a funded reducer (H-CAP-8) for as long as
	// the mistake persisted.
	var cents *int64
	if err := json.Unmarshal(v, &cents); err != nil {
		return Balance{}, fmt.Errorf("balance is not an integer number of "+
			"cents: %w", err)
	}
	if cents == nil {
		return Balance{}, fmt.Errorf("balance is null; null is not zero, and " +
			"a zero balance would leave a held position with no funded reducer")
	}
	return Balance{Cents: *cents}, nil
}

// ---------------------------------------------------------------------------

// scalar reads a JSON value as its string form, whether the wire sent a string
// or a bare number. The exchange sends fixed-point quantities as strings
// (H-CO-2) but not uniformly across every field and every endpoint, and a
// number that arrives as `1.00` must parse the same way as `"1.00"`.
//
// It returns "" for absent and for JSON null, which every caller treats as
// "field not present" rather than as a zero.
func scalar(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	if r[0] == '"' {
		var s string
		if err := json.Unmarshal(r, &s); err != nil {
			return ""
		}
		return s
	}
	if string(r) == "null" {
		return ""
	}
	return string(r)
}

// confidence: high

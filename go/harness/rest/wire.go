// Package rest is the harness's exchange write and read surface: the H-CO-1
// coordinate transform, the exact V2 wire payloads, deterministic client order
// ids, and the guarded pagination of notes/pagecontract.md.
//
// This file is the pure half. It builds and parses payloads and does no I/O, so
// V1.1 and V1.2 are table tests over literals with no fixtures and no network.
package rest

import (
	"fmt"

	"lip/harness/num"
	"lip/harness/quote"
)

// ---------------------------------------------------------------------------
// §4 H-CO-1 — the transform, pinned
// ---------------------------------------------------------------------------
//
//	book (side="yes", p cents)  ->  wire {"side":"bid", "price": p/100}
//	book (side="no",  p cents)  ->  wire {"side":"ask", "price": (100-p)/100}
//
// From the OpenAPI schema, verified 2026-07-25: "`bid` means buy YES, `ask`
// means sell YES. (Selling YES is economically equivalent to buying NO at
// `1 - price`...)" So a NO bid at 42c IS a YES ask at 58c.
//
// There are THREE representations in play and they are not interchangeable
// (§4). Book terms are what LIP scoring, `core`, `probescore.py` and the whole
// spec are written in; wire terms are the V2 order API's; position terms are
// one signed number per market, YES-positive. This transform is the only bridge
// between the first two, and it exists in exactly one place so that exactly one
// place has to be right.

// BookSide is the wire's `side` field. It describes the YES leg only.
type BookSide string

const (
	Bid BookSide = "bid" // buy YES
	Ask BookSide = "ask" // sell YES, which is buying NO at 1 − price
)

// ToWire applies H-CO-1: a bid on `side` at `priceCents` in that side's own
// cents becomes a wire side and a YES-denominated cent price.
func ToWire(side quote.Side, priceCents int) (BookSide, int, error) {
	if !quote.ValidPrice(priceCents) {
		return "", 0, fmt.Errorf("price %dc is outside the tradable range %d..%d",
			priceCents, quote.MinPrice, quote.MaxPrice)
	}
	if side == quote.SideNo {
		return Ask, 100 - priceCents, nil
	}
	return Bid, priceCents, nil
}

// FromWire is H-CO-1 run backwards, for reading resting orders back off the
// exchange. It is not decoration: §7.5's startup adoption reads orders it did
// not place in this process, and a reconciliation that mistook a NO bid at 42c
// for a YES bid at 58c would adopt the position with the wrong sign.
func FromWire(ws BookSide, yesCents int) (quote.Side, int, error) {
	if yesCents < quote.MinPrice || yesCents > quote.MaxPrice {
		return 0, 0, fmt.Errorf("yes price %dc is outside %d..%d",
			yesCents, quote.MinPrice, quote.MaxPrice)
	}
	switch ws {
	case Bid:
		return quote.SideYes, yesCents, nil
	case Ask:
		return quote.SideNo, 100 - yesCents, nil
	}
	return 0, 0, fmt.Errorf("unknown wire side %q", ws)
}

// ---------------------------------------------------------------------------
// H-CO-2 — wire encoding
// ---------------------------------------------------------------------------

// CreateOrder is a validated statement of INTENT to place one order. It is NOT
// the wire body, and `Client.Create` never marshals it -- it marshals the
// private `createOrderWire` derived from it. (`json.Marshal` applied to this
// type does not fail; it produces `{}`, because every field is unexported.
// That is a useless body, not a protection, which is why the protection is that
// nothing on the send path marshals this type.)
//
// **Every field is private and there are no setters.** The public type carries
// no `post_only`, no self-trade-prevention, no time-in-force and no
// wire-formatted value, so there is nothing on it for a caller — or a future
// refactor — to set wrongly. Those are wire policy, they are decided in exactly
// one place (`wire()`), and they are hard-coded there.
//
// This shape was arrived at adversarially. The previous version was an exported
// struct with exported fields carrying `PostOnly bool`, and its doc comment
// claimed `post_only` was "a constant of this struct's construction" with "no
// caller-reachable switch at all". That claim was false at the send path:
// `Create` received the struct by value and marshalled it, so
// `body.PostOnly = false` compiled and survived the entire test suite. H-Q-3
// requires "no exception, no flag, and no code path that sets it false", and a
// comment asserting a guarantee the compiler does not enforce is worse than no
// comment, because it stops the next reader looking.
//
// The structural property now comes from the ABSENCE of wire-policy fields on
// the public type. The completed private wire value is independently
// revalidated immediately before dispatch, because absence alone does not
// protect against a zero value or against corruption inside this package.
//
// A zero `CreateOrder` exists — Go permits `var o CreateOrder` — and is NOT
// dispatchable: `validate` rejects it, so it can never reach the exchange.
type CreateOrder struct {
	ticker        string
	side          quote.Side
	priceCents    int
	count         num.Qty
	derivedFrom   num.Qty
	clientOrderID string
}

// Ticker is the market this order is for.
func (o CreateOrder) Ticker() string { return o.ticker }

// Side is the book side, in book terms (§4), never the wire's bid/ask.
func (o CreateOrder) Side() quote.Side { return o.side }

// PriceCents is the price in that side's own cents.
func (o CreateOrder) PriceCents() int { return o.priceCents }

// Count is the contract count.
func (o CreateOrder) Count() num.Qty { return o.count }

// ClientOrderID is §7.1's deterministic coid.
func (o CreateOrder) ClientOrderID() string { return o.clientOrderID }

// createOrderWire is the exact body of POST /trade-api/v2/portfolio/events/orders.
//
// Private, and the only thing in this package that is ever marshalled to the
// exchange. Field order matches `kalshi.py:create_order`'s pinned body so the
// two can be diffed by eye.
//
// **`count` and `price` are STRINGS.** Sending JSON numbers is a schema
// violation (H-CO-2). The struct tags carry no `,string` option and the fields
// are typed `string` rather than being formatted at the edge, so a future
// change cannot make them numbers by accident.
type createOrderWire struct {
	Ticker string   `json:"ticker"`
	Side   BookSide `json:"side"`
	// Count is the contract count as a two-decimal fixed-point string.
	Count string `json:"count"`
	// Price is the YES-denominated price as a four-decimal dollar string.
	Price       string `json:"price"`
	TimeInForce string `json:"time_in_force"`
	// SelfTradePrevention is H-CO-5's `taker_at_cross`: it cancels the INCOMING
	// order on a self-match and leaves our resting maker alive. `maker` would do
	// the opposite and silently destroy the scoring presence we are paid for.
	SelfTradePrevention string `json:"self_trade_prevention_type"`
	// PostOnly is H-Q-3. Always true, hard-coded in wire(), revalidated before
	// dispatch, and absent from the public type entirely.
	PostOnly bool `json:"post_only"`
	// ClientOrderID is §7.1's deterministic coid. H-ORD-2b's same-coid recovery
	// depends on it being reproducible across a process restart.
	ClientOrderID string `json:"client_order_id"`
}

const (
	timeInForce         = "good_till_canceled"
	selfTradePrevention = "taker_at_cross"
)

// NewCreateOrder builds a validated intent to bid on `side` at `priceCents` in
// that side's own cents, for `count` contracts.
//
// H-CO-4b requires a dispatched count to be strictly positive and no greater
// than the quantized bound it derives from, and "0.00" is never sent;
// `derivedFrom` is that bound -- |q| for a reducer (H-Q-5a), size_A for an
// adding quote -- and 0 means "no bound to check".
func NewCreateOrder(ticker string, side quote.Side, priceCents int,
	count, derivedFrom num.Qty, coid string) (CreateOrder, error) {

	o := CreateOrder{
		ticker:        ticker,
		side:          side,
		priceCents:    priceCents,
		count:         count,
		derivedFrom:   derivedFrom,
		clientOrderID: coid,
	}
	if err := o.validate(); err != nil {
		return CreateOrder{}, err
	}
	return o, nil
}

// validate checks every semantic field. It runs at construction AND again
// immediately before dispatch, because a value can be copied, zeroed, or
// corrupted between the two.
func (o CreateOrder) validate() error {
	if o.ticker == "" {
		return fmt.Errorf("empty ticker")
	}
	if o.side != quote.SideYes && o.side != quote.SideNo {
		return fmt.Errorf("side %d is neither yes nor no; a zero or corrupt "+
			"side would place the order on the wrong leg of the book", o.side)
	}
	if !quote.ValidPrice(o.priceCents) {
		return fmt.Errorf("price %dc is outside the tradable range %d..%d",
			o.priceCents, quote.MinPrice, quote.MaxPrice)
	}
	if o.clientOrderID == "" {
		return fmt.Errorf("empty client_order_id: §7.1 makes the coid " +
			"deterministic precisely so an ambiguous create can be identified " +
			"afterwards (H-ORD-2b), and an absent one cannot")
	}
	// The coid must be one of ours AND must round-trip exactly. A coid that
	// parses but does not reproduce is a coid H-ORD-2b's recovery cannot
	// reconstruct after a restart, which is the whole point of it being
	// deterministic.
	parsed, ok := ParseCoid(o.clientOrderID)
	if !ok {
		return fmt.Errorf("client_order_id %q is not one of ours; §7.5's "+
			"adoption identifies our orders by this prefix and a foreign one "+
			"would be unrecoverable", o.clientOrderID)
	}
	rebuilt, err := Coid(parsed.RunID, parsed.MarketIdx, parsed.Side, parsed.Seq)
	if err != nil {
		return fmt.Errorf("client_order_id %q does not rebuild: %w",
			o.clientOrderID, err)
	}
	if rebuilt != o.clientOrderID {
		return fmt.Errorf("client_order_id %q does not round-trip (rebuilt as "+
			"%q); H-ORD-2b's same-coid retry depends on exact reproducibility",
			o.clientOrderID, rebuilt)
	}
	if parsed.Side != o.side {
		return fmt.Errorf("client_order_id %q names side %v but the order is "+
			"%v; a mismatch makes the ambiguous-create recovery look up the "+
			"wrong side", o.clientOrderID, parsed.Side, o.side)
	}
	// Market index cannot be checked against the ticker here: this package
	// holds no index-to-ticker map, and inventing one would be guessing.
	return num.ValidateCount(o.count, o.derivedFrom)
}

// wire builds the completed wire body. This is the ONE place `post_only`,
// self-trade prevention and time-in-force are decided, and each is a literal.
func (o CreateOrder) wire() (createOrderWire, error) {
	ws, yesCents, err := ToWire(o.side, o.priceCents)
	if err != nil {
		return createOrderWire{}, err
	}
	return createOrderWire{
		Ticker:              o.ticker,
		Side:                ws,
		Count:               o.count.Wire(),
		Price:               PriceWire(yesCents),
		TimeInForce:         timeInForce,
		SelfTradePrevention: selfTradePrevention,
		PostOnly:            true,
		ClientOrderID:       o.clientOrderID,
	}, nil
}

// validateWire re-checks the completed wire value against the intent that
// produced it, immediately before it is frozen into the retry payload.
//
// This is the independent half of the protection. Sealing the public type stops
// a caller expressing a bad order; this stops a bad body leaving the process
// however it was produced.
func validateWire(w createOrderWire, o CreateOrder) error {
	if !w.PostOnly {
		return fmt.Errorf("post_only is false: H-Q-3 forbids taking with no " +
			"exception, no flag, and no code path that sets it false, and A1 " +
			"asserts it over every order ever sent")
	}
	if w.SelfTradePrevention != selfTradePrevention {
		return fmt.Errorf("self_trade_prevention_type is %q, not %q: `maker` "+
			"cancels our RESTING order on a self-match and would silently "+
			"destroy the scoring presence we are paid for (H-CO-5)",
			w.SelfTradePrevention, selfTradePrevention)
	}
	if w.TimeInForce != timeInForce {
		return fmt.Errorf("time_in_force is %q, not %q", w.TimeInForce, timeInForce)
	}
	if w.Ticker != o.ticker {
		return fmt.Errorf("wire ticker %q does not match intent %q", w.Ticker, o.ticker)
	}
	if w.ClientOrderID != o.clientOrderID {
		return fmt.Errorf("wire coid %q does not match intent %q",
			w.ClientOrderID, o.clientOrderID)
	}
	if w.Count != o.count.Wire() {
		return fmt.Errorf("wire count %q does not match intent %q",
			w.Count, o.count.Wire())
	}
	wantSide, wantCents, err := ToWire(o.side, o.priceCents)
	if err != nil {
		return err
	}
	if w.Side != wantSide {
		return fmt.Errorf("wire side %q does not match intent %v", w.Side, o.side)
	}
	if w.Price != PriceWire(wantCents) {
		return fmt.Errorf("wire price %q does not match intent %dc",
			w.Price, o.priceCents)
	}
	return nil
}

// PriceWire formats a YES-denominated cent price as the exchange's four-decimal
// dollar string: 58c -> "0.5800".
//
// Four decimals, not two, because 1e-4 USD is the exchange's own price quantum
// -- every one of 79,526 sampled `yes_price_dollars` values carried exactly
// four decimals. Formatting to two would be lossless for the integer-cent book
// prices the quoting path reads (H-CO-3a) and lossy for anything else, which is
// a distinction the wire format should not be asked to remember.
func PriceWire(yesCents int) string {
	return fmt.Sprintf("%.4f", float64(yesCents)/100)
}

// confidence: high

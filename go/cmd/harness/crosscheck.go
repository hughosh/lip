package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"lip/core"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// A cross-check is a bounded, independent read. Only the owner compares it
// with its websocket book and portfolio model.
type crossCheckResult struct {
	request wsx.CrossCheckRequest
	started time.Duration
	book    rest.OrderbookResult
}

func (r *rig) crossCheckLoop(ctx context.Context, requests <-chan wsx.CrossCheckRequest, results chan<- crossCheckResult) {
	var backoff readBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-requests:
			book, started := backoff.orderbookAt(ctx, r.api, req.Token.Ticker(), r.ex.Mono)
			result := crossCheckResult{request: req, started: started, book: book}
			select {
			case results <- result:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Failed admission is an explicit unavailable answer, so the gate retries.
func (o *owner) requestCrossChecks(requests []wsx.CrossCheckRequest) {
	for _, req := range requests {
		if o.crossChecks == nil {
			o.applyCrossCheck(crossCheckResult{request: req})
			continue
		}
		o.crossCheckPending = append(o.crossCheckPending, req)
	}
	o.flushCrossChecks()
}
func (o *owner) flushCrossChecks() {
	for len(o.crossCheckPending) > 0 {
		select {
		case o.crossChecks <- o.crossCheckPending[0]:
			o.crossCheckPending = o.crossCheckPending[1:]
		default:
			return
		}
	}
}

func (o *owner) applyCrossCheck(result crossCheckResult) {
	restore := o.marketContext(result.request.Token.Ticker())
	defer restore()
	out, why := o.judgeCrossCheck(result)
	eff := o.r.gate.NoteCrossCheck(result.request.Token, out, why, o.r.ex.Clock.Now())
	if !eff.Accepted {
		return
	}
	o.r.anom.raiseAll(eff.Anomalies)
	o.noteReduce(eff.Reduce)
	if eff.RetainRESTBook {
		if !o.restReducer.Retain(result.book, result.started) {
			o.restReducer.Invalidate()
		}
	} else if result.request.Refresh {
		o.restReducer.Invalidate()
	}
	if eff.ReplaceBook {
		o.replaceBook(result.book)
	}
	if eff.Resnapshot {
		o.requestF5Resnapshot()
		o.offerToken(o.reconcileOut, o.reconcileToken)
	}
}

func (o *owner) judgeCrossCheck(result crossCheckResult) (wsx.CrossCheckOutcome, string) {
	r := result.book
	switch r.Outcome {
	case rest.OrderbookGranularity:
		return wsx.CrossCheckGranularity, "REST orderbook contains a non-cent price"
	case rest.OrderbookRead:
		if r.Ticker != result.request.Token.Ticker() {
			return wsx.CrossCheckUnavailable, "orderbook ticker mismatch"
		}
		if result.request.Refresh {
			return wsx.CrossCheckAgree, ""
		}
		book := o.r.book.Book(r.Ticker)
		if book == nil {
			return wsx.CrossCheckUnavailable, "no websocket book to compare"
		}
		verdict := rest.CompareBooks(rest.BookCheck{Target: book.Target,
			Yes: rest.SideBooks{WS: wsLevels(book.Yes()), REST: r.Yes, Ours: o.ownResting(quote.SideYes)},
			No:  rest.SideBooks{WS: wsLevels(book.No()), REST: r.No, Ours: o.ownResting(quote.SideNo)}})
		if verdict.Agree {
			return wsx.CrossCheckAgree, ""
		}
		return wsx.CrossCheckDisagree, verdict.Why
	default:
		if r.Err != nil {
			var throttle *rest.RateLimitError
			if errors.As(r.Err, &throttle) {
				o.noteRESTThrottle(throttle)
			}
			return wsx.CrossCheckUnavailable, r.Err.Error()
		}
		return wsx.CrossCheckUnavailable, "REST orderbook read was not dispatched"
	}
}

func wsLevels(levels *core.Levels) []rest.BookLevel {
	prices := levels.SortedDesc()
	out := make([]rest.BookLevel, 0, len(prices))
	for _, p := range prices {
		if size, ok := levels.Get(p); ok {
			out = append(out, rest.BookLevel{Cents: p, Size: size})
		}
	}
	return out
}

func (o *owner) ownResting(side quote.Side) []rest.OwnResting {
	byPrice := make(map[int]num.Qty)
	for _, lo := range o.r.pf.LiveOrders() {
		if lo.Ticker != o.ticker() || lo.Side != side {
			continue
		}
		if _, absent := o.absentOrders[lo.OrderID]; absent {
			continue
		}
		price, exact := rest.CentsExact(lo.Price4)
		if !exact {
			continue
		}
		byPrice[price] += lo.Remaining
	}
	out := make([]rest.OwnResting, 0, len(byPrice))
	for price, size := range byPrice {
		if size > 0 {
			out = append(out, rest.OwnResting{Cents: price, Size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cents > out[j].Cents })
	return out
}

func (o *owner) replaceBook(r rest.OrderbookResult) {
	book := o.r.book.Book(r.Ticker)
	if book == nil {
		o.r.anom.raise(risk.Anomaly{Class: "BOOK_REPLACE_FAILED", Sev: risk.SEV2, Ticker: r.Ticker, Text: "no local book"})
		return
	}
	yes, no := r.Snapshot()
	if err := book.ApplySnapshot(yes, no); err != nil {
		o.r.anom.raise(risk.Anomaly{Class: "BOOK_REPLACE_FAILED", Sev: risk.SEV2, Ticker: r.Ticker, Text: fmt.Sprintf("REST snapshot rejected: %v", err)})
		return
	}
	book.ClearHistory()
	book.Checkpoint(float64(o.r.book.Watermark()) / 1000)
}

// The websocket book is never a fallback while F5 remains active.
func (o *owner) pricingBook(role quote.Role) (*core.Book, bool) {
	ticker := o.ticker()
	stamp := o.r.ex.Clock.Now()
	if o.r.gate.F5Quarantined(ticker) {
		if role != quote.RoleReducing || !o.r.gate.RESTReducerActionable(ticker, stamp) {
			return nil, false
		}
		ws := o.r.book.Book(ticker)
		if ws == nil {
			return nil, false
		}
		return o.restReducer.Snapshot(ticker, ws.Target, stamp.Mono, o.p.Quiet)
	}
	if !o.r.gate.Actionable(ticker, stamp) {
		return nil, false
	}
	book := o.r.book.Book(ticker)
	return book, book != nil
}

func (o *owner) f5FundedReducer(q num.Qty) num.Qty {
	if !o.r.gate.F5Quarantined(o.ticker()) {
		return o.fundedReducer(q)
	}
	book, ok := o.pricingBook(quote.RoleReducing)
	if !ok {
		return 0
	}
	return o.fundedReducerOn(book, q)
}

// Recompute the order's price from the same source at dispatch.
func (o *owner) f5TargetPrice(side quote.Side, role quote.Role) (int, bool) {
	if !o.r.gate.F5Quarantined(o.ticker()) {
		return o.targetPrice(side)
	}
	book, ok := o.pricingBook(role)
	if !ok {
		return 0, false
	}
	return o.targetPriceOn(book, side)
}

func (o *owner) expireRESTIfRecovered() {
	if !o.r.gate.F5Quarantined(o.ticker()) {
		o.restReducer.Invalidate()
	}
}

// A full command channel is retried on the next owner tick; the request is
// never silently lost behind an unrelated queued resnapshot.
func (o *owner) requestF5Resnapshot() {
	o.f5ResnapshotPending = true
	o.retryF5Resnapshot()
}

func (o *owner) retryF5Resnapshot() {
	if !o.f5ResnapshotPending || !o.r.gate.Connected() || o.wsCommands == nil || o.bookSID <= 0 {
		return
	}
	cmd := wsx.Command{Kind: wsx.CmdResnapshot, Sids: []int64{o.bookSID}, Tickers: []string{o.ticker()}}
	select {
	case o.wsCommands <- cmd:
		o.f5ResnapshotPending = false
	default:
	}
}

func (o *owner) f5GateFailBlocksPlacement(now time.Duration, in quote.Intent) bool {
	if !o.r.gate.F5Quarantined(in.Market) || in.Role != quote.RoleReducing {
		return o.gateFailBlocksPlacement(now, in)
	}
	_, ok := o.pricingBook(in.Role)
	return !ok
}

// confidence: high

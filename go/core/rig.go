package core

import (
	"encoding/json"
	"fmt"
)

// Rig is the frame handler: book reconstruction, sequence-gap quarantine, and
// the two row types. Direct port of rig.py:262-483, excluding `run` and
// `resolve_mids`, which need a socket and a clock and therefore live outside
// this package.
//
// WHAT trade_through IS, AND WHAT IT IS NOT
//
// Kalshi matches price-time. Seeing a print at your price does NOT mean your
// order filled — you may have been behind the queue at that level, and public
// data cannot see queue position. When an aggressor trades THROUGH a level to a
// worse price, that is strong evidence the level was exhausted.
//
// It is NOT proof, and an earlier version of the Python wrongly claimed it was:
//
//   - Kalshi's maker-side self-trade prevention CANCELS the resting maker and
//     continues matching at worse prices. A print below the touch can therefore
//     occur without the touch order ever filling.
//   - A cancel or amend landing microseconds before the sweep produces the same
//     observation, with its delta arriving afterwards.
//
// So `trade_through` selects a population that is heavily enriched for genuine
// fills but is not causally clean. Public aggregate data cannot separate
// "filled" from "cancelled just in time".
//
// Note also what the flag is evidence ABOUT. If the touch is 48 and the print
// is at 47, the row describes a real fill at 47, but the trade-through fact
// concerns an order resting at 48. Analysis of "would my quote at the touch
// have filled" must use the touch price (PreBestYes / PreBestNo), not the print
// price.
//
// This retraction is load-bearing and is carried across from rig.py:19-47
// deliberately. Do not delete it as prose. See port-spec.md P20.

// Forward horizons for the markout estimator, in seconds. A slice, not a map:
// Python iterates a dict literal, so the order is fixed at 1m, 5m, 30m.
var Horizons = []struct {
	Name string
	Secs int64
}{
	{"1m", 60},
	{"5m", 300},
	{"30m", 1800},
}

type FillRow struct {
	TradeID      string
	TsMs         int64
	Ticker       string
	RestingSide  string
	Price        int
	Size         float64
	TakerSide    string
	TradeThrough int
	PreBestYes   *int
	PreBestNo    *int
	PreYesSize   float64
	PreNoSize    float64
	PreMid       *float64
	PreSpread    *int
	DepthAtPrice float64
	BookLagMs    *int64
}

type ReferenceRow struct {
	TsMs     int64
	Ticker   string
	RefYes   *int
	RefNo    *int
	Gate     int
	YesDepth float64
	NoDepth  float64
	Target   float64
}

// Sink receives rows. `store` implements it against SQLite; tests use an
// in-memory recorder. Keeping it an interface is what lets the whole handler be
// exercised with no I/O.
type Sink interface {
	Fill(FillRow)
	PendingMid(tradeID, ticker, horizon string, dueMS int64)
	Reference(ReferenceRow)
}

type Stats struct {
	Trades      int64
	Fills       int64
	Through     int64
	Gaps        int64
	Refs        int64
	Quarantined int64
}

type Rig struct {
	sink  Sink
	books map[string]*Book
	seq   map[int64]int64

	// Insertion order of seq's keys, so Sids() reproduces Python's
	// `list(self.seq)` at rig.py:529. Range order over a Go map is randomized.
	seqOrder []int64

	Stats Stats

	// Single clock domain. Mixing exchange ts_ms with local wall time shifts
	// every checkpoint by the clock offset plus delivery latency, which
	// silently mis-attributes trades to post-trade book states.
	exTs int64

	// Markets whose book is not currently trustworthy: awaiting a first
	// snapshot, or downstream of a dropped delta. Trades are not recorded for
	// these until a fresh snapshot arrives.
	stale           map[string]struct{}
	NeedsResnapshot bool

	// Optional; nil in tests. Diagnostics only — never affects rows.
	Log func(string)
}

func NewRig(sink Sink, universe map[string]float64) *Rig {
	r := &Rig{
		sink:  sink,
		books: make(map[string]*Book, len(universe)),
		seq:   map[int64]int64{},
		stale: make(map[string]struct{}, len(universe)),
	}
	for t, target := range universe {
		r.books[t] = NewBook(target)
		r.stale[t] = struct{}{}
	}
	return r
}

func (r *Rig) Tickers() []string {
	out := make([]string, 0, len(r.books))
	for t := range r.books {
		out = append(out, t)
	}
	return out
}

func (r *Rig) Book(ticker string) *Book { return r.books[ticker] }
func (r *Rig) StaleCount() int          { return len(r.stale) }

// Sids returns the subscription ids seen so far, in first-seen order. It feeds
// the `sids` field of the resnapshot request, where Python passes
// `list(self.seq)` — dict key order, which is insertion order.
func (r *Rig) Sids() []int64 {
	out := make([]int64, len(r.seqOrder))
	copy(out, r.seqOrder)
	return out
}

// ResetOnReconnect discards everything the previous connection established.
// rig.py:554-564. See port-spec.md P25.
//
// The books are discarded outright, not merely re-historied: any level carried
// across the gap may already be wrong, and a trade arriving before the
// replacement snapshot would be attributed to it. Five pieces of state have to
// go together, and dropping any one of them fails silently —
//
//   - the levels, or a post-gap trade is attributed to a pre-gap book;
//   - the history, for the same reason one checkpoint later;
//   - RefYes/RefNo/Gate, or change detection compares the first post-reconnect
//     book against a reference row that is no longer true of anything, and
//     suppresses it;
//   - the stale set, which is what withholds trades until each market resnaps;
//   - the seq map, whose sids the new connection is free to renumber.
func (r *Rig) ResetOnReconnect() {
	for t, b := range r.books {
		b.yes = NewLevels()
		b.no = NewLevels()
		b.ClearHistory()
		b.RefYes, b.RefNo, b.Gate = nil, nil, nil
		r.stale[t] = struct{}{}
	}
	r.seq = map[int64]int64{}
	r.seqOrder = nil
	r.NeedsResnapshot = false
}

// Watermark exposes the current exchange-time watermark. Read-only; it is the
// clock every row is stamped with, so it is worth being able to observe both in
// tests and in live diagnostics.
func (r *Rig) Watermark() int64 { return r.exTs }

func (r *Rig) logf(format string, a ...any) {
	if r.Log != nil {
		r.Log(fmt.Sprintf(format, a...))
	}
}

// watermark is a monotone exchange-time watermark; it never goes backwards.
//
// Python guards with `if ts_ms:` — a FALSY test, so a missing timestamp, a JSON
// null and a literal zero ALL skip the update. See port-spec.md P8.
//
// That is exactly why this takes a plain int64 rather than a *int64: absent and
// null both decode to zero, and zero already skips, so the pointer carried no
// information. It cost one heap allocation per snapshot and per delta frame —
// reflect.unsafe_New was the single largest allocation site at 23.8%.
//
// The trade branch is NOT the same and keeps its pointer: there, absent must
// raise where a present zero is a legitimate ts_ms of 0.
func (r *Rig) watermark(tsMs int64) int64 {
	if tsMs != 0 {
		if tsMs > r.exTs {
			r.exTs = tsMs
		}
	}
	return r.exTs
}

// --- wire types ----------------------------------------------------------
//
// Fields Python reads with `[]` (and would raise KeyError on) are pointers, so
// their absence is detectable and produces an error rather than a silent zero.
// Fields Python reads with `.get()` are plain values.

type envelope struct {
	Type string          `json:"type"`
	Sid  *int64          `json:"sid"`
	Seq  *int64          `json:"seq"`
	Msg  json.RawMessage `json:"msg"`
}

type snapshotMsg struct {
	MarketTicker string      `json:"market_ticker"`
	TsMs         int64       `json:"ts_ms"`
	YesLevels    [][2]string `json:"yes_dollars_fp"`
	NoLevels     [][2]string `json:"no_dollars_fp"`
}

type deltaMsg struct {
	MarketTicker string  `json:"market_ticker"`
	TsMs         int64   `json:"ts_ms"`
	Side         *string `json:"side"`
	PriceDollars *string `json:"price_dollars"`
	DeltaFP      *string `json:"delta_fp"`
}

type tradeMsg struct {
	TradeID         *string `json:"trade_id"`
	MarketTicker    *string `json:"market_ticker"`
	TsMs            *int64  `json:"ts_ms"`
	CountFP         *string `json:"count_fp"`
	YesPriceDollars *string `json:"yes_price_dollars"`
	NoPriceDollars  *string `json:"no_price_dollars"`
	TakerSide       string  `json:"taker_side"`
}

// Handle processes one raw websocket frame. rig.py:423-483.
func (r *Rig) Handle(frame []byte) error {
	var env envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}

	// Sequence is subscription-wide, not per market: a gap means SOME market
	// lost a delta and we cannot tell which. Every book on that subscription is
	// therefore suspect and must be resnapshotted. Checked BEFORE mutating, so
	// a known-bad delta is never applied — and the whole frame is dropped, its
	// book update along with it.
	//
	// Note the assignment happens before the test, so the next frame is judged
	// against the post-gap value. See port-spec.md P13.
	if env.Sid != nil && env.Seq != nil {
		prev, hadPrev := r.seq[*env.Sid]
		if !hadPrev {
			r.seqOrder = append(r.seqOrder, *env.Sid)
		}
		r.seq[*env.Sid] = *env.Seq
		if hadPrev && *env.Seq != prev+1 {
			r.Stats.Gaps++
			for t := range r.books {
				r.stale[t] = struct{}{}
			}
			r.NeedsResnapshot = true
			r.logf("[seq gap on sid %d: %d -> %d] quarantining %d books",
				*env.Sid, prev, *env.Seq, len(r.books))
			return nil
		}
	}

	switch env.Type {
	case "orderbook_snapshot":
		return r.handleSnapshot(env.Msg)
	case "orderbook_delta":
		return r.handleDelta(env.Msg)
	case "trade":
		return r.handleTrade(env.Msg)
	case "error":
		r.logf("[ws error] %s", frame)
	}
	return nil
}

func (r *Rig) handleSnapshot(raw json.RawMessage) error {
	var m snapshotMsg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("decode snapshot: %w", err)
		}
	}
	book := r.books[m.MarketTicker]
	if book == nil {
		return nil
	}
	if err := book.ApplySnapshot(m.YesLevels, m.NoLevels); err != nil {
		return err
	}
	// Snapshots carry no exchange timestamp on the wire. Stamping them with
	// local time would inject a second clock, so they adopt the current
	// exchange watermark instead.
	tsMs := r.watermark(m.TsMs)
	book.ClearHistory()
	book.Checkpoint(float64(tsMs) / 1000.0)
	delete(r.stale, m.MarketTicker)
	r.recordReference(m.MarketTicker, book, tsMs)
	return nil
}

func (r *Rig) handleDelta(raw json.RawMessage) error {
	var m deltaMsg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("decode delta: %w", err)
		}
	}
	book := r.books[m.MarketTicker]
	if book == nil {
		return nil
	}
	if _, quarantined := r.stale[m.MarketTicker]; quarantined {
		return nil
	}
	// The watermark is advanced only AFTER the stale check here, but BEFORE it
	// in the trade branch. That asymmetry is in the Python and is observable in
	// the ts_ms of every subsequent row. See port-spec.md P15.
	//
	// It also advances BEFORE the required fields are read, matching the
	// Python, where a malformed delta raises only after the watermark has moved.
	tsMs := r.watermark(m.TsMs)
	if m.Side == nil || m.PriceDollars == nil || m.DeltaFP == nil {
		return fmt.Errorf("delta missing side/price_dollars/delta_fp")
	}
	price, err := ParsePriceCents(*m.PriceDollars)
	if err != nil {
		return err
	}
	delta, err := ParseSize(*m.DeltaFP)
	if err != nil {
		return err
	}
	book.ApplyDelta(*m.Side, price, delta)
	book.Checkpoint(float64(tsMs) / 1000.0)
	r.recordReference(m.MarketTicker, book, tsMs)
	return nil
}

func (r *Rig) handleTrade(raw json.RawMessage) error {
	var m tradeMsg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("decode trade: %w", err)
		}
	}
	// Still a pointer here: unlike the snapshot and delta branches, a trade
	// distinguishes an absent ts_ms (an error) from a present zero (a valid row).
	var tradeTs int64
	if m.TsMs != nil {
		tradeTs = *m.TsMs
	}
	r.watermark(tradeTs)

	ticker := ""
	if m.MarketTicker != nil {
		ticker = *m.MarketTicker
	}
	if _, quarantined := r.stale[ticker]; quarantined {
		r.Stats.Quarantined++
		return nil
	}
	return r.recordTrade(&m)
}

// recordReference writes a row only when the reference price or the gate
// actually moves. rig.py:308-321.
func (r *Rig) recordReference(ticker string, book *Book, tsMs int64) {
	var ry, rn *int
	if p, _, ok := book.BestYes(); ok {
		ry = &p
	}
	if p, _, ok := book.BestNo(); ok {
		rn = &p
	}
	gate := book.Qualifies()

	if eqIntPtr(ry, book.RefYes) && eqIntPtr(rn, book.RefNo) &&
		book.Gate != nil && *book.Gate == gate {
		return
	}
	book.RefYes, book.RefNo = ry, rn
	g := gate
	book.Gate = &g

	r.sink.Reference(ReferenceRow{
		TsMs:   tsMs,
		Ticker: ticker,
		RefYes: ry,
		RefNo:  rn,
		Gate:   gate,
		// Insertion-ordered sums; see port-spec.md P2.
		YesDepth: book.Yes().Sum(),
		NoDepth:  book.No().Sum(),
		Target:   book.Target,
	})
	r.Stats.Refs++
}

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (r *Rig) recordTrade(m *tradeMsg) error {
	if m.MarketTicker == nil {
		return fmt.Errorf("trade missing market_ticker")
	}
	ticker := *m.MarketTicker
	book := r.books[ticker]
	if book == nil {
		// Returns before any stats increment: the `trade` channel carries the
		// whole exchange, and 98.6% of decoded trade frames land here.
		return nil
	}
	if m.TsMs == nil || m.CountFP == nil || m.YesPriceDollars == nil ||
		m.NoPriceDollars == nil || m.TradeID == nil {
		return fmt.Errorf("trade missing a required field")
	}

	tsMs := *m.TsMs
	size, err := ParseSize(*m.CountFP)
	if err != nil {
		return err
	}
	yesPrice, err := ParsePriceCents(*m.YesPriceDollars)
	if err != nil {
		return err
	}
	noPrice, err := ParsePriceCents(*m.NoPriceDollars)
	if err != nil {
		return err
	}
	taker := m.TakerSide

	// Taker bought yes -> lifted a resting NO bid  -> resting side is 'no'.
	// Taker bought no  -> hit a resting YES bid    -> resting side is 'yes'.
	// Anything that is not exactly "yes", including absent, yields 'yes'.
	// These semantics are CONFIRMED CORRECT and must not be "fixed" (P17).
	restingSide := "yes"
	price := yesPrice
	if taker == "yes" {
		restingSide = "no"
		price = noPrice
	}

	preYes, preNo, lag := book.StateBefore(float64(tsMs) / 1000.0)

	var bestYes, bestNo *int
	if p, _, ok := preYes.Max(); ok {
		bestYes = &p
	}
	if p, _, ok := preNo.Max(); ok {
		bestNo = &p
	}
	restingPre, bestResting := preYes, bestYes
	if restingSide == "no" {
		restingPre, bestResting = preNo, bestNo
	}

	// The aggressor reached a worse price than the touch, so the touch level
	// was exhausted — strong evidence, not proof. See the package comment.
	tradeThrough := 0
	if bestResting != nil && price < *bestResting {
		tradeThrough = 1
	}

	var preYesSize, preNoSize float64
	if bestYes != nil {
		preYesSize = preYes.GetOr(*bestYes, 0.0)
	}
	if bestNo != nil {
		preNoSize = preNo.GetOr(*bestNo, 0.0)
	}

	var preSpread *int
	if bestYes != nil && bestNo != nil {
		s := 100 - *bestYes - *bestNo
		preSpread = &s
	}

	// Truncates toward zero, matching Python's int(); the lag is legitimately
	// negative when a trade's deltas were processed first (P5, P7).
	var bookLagMs *int64
	if lag != nil {
		v := int64(*lag * 1000)
		bookLagMs = &v
	}

	r.sink.Fill(FillRow{
		TradeID:      *m.TradeID,
		TsMs:         tsMs,
		Ticker:       ticker,
		RestingSide:  restingSide,
		Price:        price,
		Size:         size,
		TakerSide:    taker,
		TradeThrough: tradeThrough,
		PreBestYes:   bestYes,
		PreBestNo:    bestNo,
		PreYesSize:   preYesSize,
		PreNoSize:    preNoSize,
		PreMid:       MidOf(preYes, preNo),
		PreSpread:    preSpread,
		DepthAtPrice: restingPre.GetOr(price, 0.0),
		BookLagMs:    bookLagMs,
	})
	for _, h := range Horizons {
		r.sink.PendingMid(*m.TradeID, ticker, h.Name, tsMs+h.Secs*1000)
	}

	r.Stats.Trades++
	r.Stats.Fills++
	r.Stats.Through += int64(tradeThrough)
	return nil
}

// confidence: high

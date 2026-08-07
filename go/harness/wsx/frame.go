package wsx

import (
	"encoding/json"
	"fmt"

	"lip/harness/rest"
	"lip/harness/risk"
)

// FrameKind is what an inbound frame turned out to be.
type FrameKind uint8

const (
	// FrameUnknown is the zero value and is never a classification this
	// package produces for a frame it understood. It exists so that a
	// zero-valued FrameInfo cannot be mistaken for a deliverable book frame.
	FrameUnknown FrameKind = iota
	FrameSnapshot
	FrameDelta
	FrameTrade
	FrameError
	// FrameOther is a frame we decoded and do not act on -- a subscription
	// acknowledgement, a heartbeat, anything the exchange adds later. It is
	// still delivered to core, which ignores what it does not recognise.
	FrameOther
)

// isBookFrame reports whether a frame kind carries resting book state, and so
// whether core refusing it leaves a book that needs replacing. A refused trade
// print costs us a row; a refused snapshot or delta costs us the book.
func isBookFrame(k FrameKind) bool {
	return k == FrameSnapshot || k == FrameDelta
}

func (k FrameKind) String() string {
	switch k {
	case FrameSnapshot:
		return "orderbook_snapshot"
	case FrameDelta:
		return "orderbook_delta"
	case FrameTrade:
		return "trade"
	case FrameError:
		return "error"
	case FrameOther:
		return "other"
	}
	return "unknown"
}

// FrameInfo is InspectFrame's verdict on one frame.
type FrameInfo struct {
	Kind   FrameKind
	Ticker string

	// Deliver is whether the frame may be passed to core.Rig.Handle. It is
	// false ONLY when delivering it would corrupt a book: a fractional book
	// price, or a frame we could not decode well enough to know what it is.
	Deliver bool

	// Granularity is H-CO-3a's violation: a resting book price that is not an
	// exact integer cent. It is not a rounding problem. It is the exchange
	// changing its tick size, and every price comparison, cap and requote
	// decision in the harness assumes integer cents.
	Granularity bool

	Anomalies []risk.Anomaly
}

// InspectFrame classifies one raw frame and validates BOOK prices only.
//
// # Why only book prices
//
// The public `trade` stream carries fractional-cent prints and always has:
// 17.08% of prints are fractional cents (H-CO-3a), and `core.ParsePriceCents`
// exists to reproduce CPython's banker's rounding over exactly those. Rejecting
// them here would throw away most of the tape and would be rejecting a
// measured, normal fact about the exchange.
//
// The RESTING book is different. Every level the exchange has ever published
// has been an integer cent, and the harness's prices, caps and requote
// decisions are all integer-cent arithmetic. H-CO-3a's instruction is to
// ASSERT that rather than assume it, and this is where the assertion lives --
// before core, because once a fractional level is in a Book the touch it
// implies is already wrong.
//
// # Why the parse is exact
//
// `rest.ParsePrice4` is integer arithmetic end to end. Going through float64
// would make "0.5800" not exactly 0.58, and a whole-cent test on the result
// would then reject a perfectly ordinary 58c level some of the time -- turning
// H-CO-3a's detector into a random generator of SEV2s.
func InspectFrame(frame []byte) FrameInfo {
	var env envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		// We cannot tell whether this is a book frame, so we cannot certify
		// that delivering it is safe. Undeliverable and loud, rather than
		// passed through on the assumption that core will cope.
		return FrameInfo{
			Kind:    FrameUnknown,
			Deliver: false,
			Anomalies: []risk.Anomaly{{
				Class: "FRAME_UNDECODABLE", Sev: risk.SEV2,
				Text: fmt.Sprintf("a websocket frame did not decode as an "+
					"envelope (%v); it is not delivered, because a frame we "+
					"cannot classify is one we cannot certify as book-safe",
					err),
			}},
		}
	}

	switch env.Type {
	case "orderbook_snapshot":
		return inspectSnapshot(env.Msg)
	case "orderbook_delta":
		return inspectDelta(env.Msg)
	case "trade":
		return inspectTrade(env.Msg)
	case "error":
		return FrameInfo{Kind: FrameError, Deliver: true,
			Anomalies: []risk.Anomaly{{
				Class: "WS_ERROR_FRAME", Sev: risk.SEV2,
				Text: "the exchange sent an error frame: " + snippet(frame),
			}}}
	}
	return FrameInfo{Kind: FrameOther, Deliver: true}
}

func inspectSnapshot(raw json.RawMessage) FrameInfo {
	info := FrameInfo{Kind: FrameSnapshot, Deliver: true}
	var m snapshotMsg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return undecodableMsg("orderbook_snapshot", err)
		}
	}
	info.Ticker = m.MarketTicker
	for _, side := range [...]struct {
		name   string
		levels [][2]string
	}{{"yes_dollars_fp", m.YesLevels}, {"no_dollars_fp", m.NoLevels}} {
		for _, lv := range side.levels {
			if bad, anom := checkCent(m.MarketTicker, side.name, lv[0]); bad {
				info.Granularity = true
				info.Deliver = false
				info.Anomalies = append(info.Anomalies, anom)
				return info
			}
		}
	}
	return info
}

func inspectDelta(raw json.RawMessage) FrameInfo {
	info := FrameInfo{Kind: FrameDelta, Deliver: true}
	var m deltaMsg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return undecodableMsg("orderbook_delta", err)
		}
	}
	info.Ticker = m.MarketTicker
	if m.PriceDollars == nil {
		// core rejects this itself; there is nothing to validate and nothing
		// to protect the book from, so it is delivered and core decides.
		return info
	}
	if bad, anom := checkCent(m.MarketTicker, "price_dollars", *m.PriceDollars); bad {
		info.Granularity = true
		info.Deliver = false
		info.Anomalies = append(info.Anomalies, anom)
	}
	return info
}

// inspectTrade classifies only. A fractional PRINT is legal and normal.
func inspectTrade(raw json.RawMessage) FrameInfo {
	info := FrameInfo{Kind: FrameTrade, Deliver: true}
	var m tradeMsg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return undecodableMsg("trade", err)
		}
	}
	if m.MarketTicker != nil {
		info.Ticker = *m.MarketTicker
	}
	return info
}

func undecodableMsg(kind string, err error) FrameInfo {
	return FrameInfo{
		Kind:    FrameUnknown,
		Deliver: false,
		Anomalies: []risk.Anomaly{{
			Class: "FRAME_UNDECODABLE", Sev: risk.SEV2,
			Text: fmt.Sprintf("a %s frame's msg did not decode (%v)", kind, err),
		}},
	}
}

// checkCent is H-CO-3a's assertion on one book price string.
func checkCent(ticker, field, s string) (bool, risk.Anomaly) {
	p4, err := rest.ParsePrice4(s)
	if err != nil {
		return true, risk.Anomaly{
			Class: "BOOK_PRICE_GRANULARITY", Sev: risk.SEV2, Ticker: ticker,
			Text: fmt.Sprintf("book price %q in %s did not parse as an exact "+
				"fixed-point price (%v); the level cannot be trusted and the "+
				"frame is not delivered", s, field, err),
		}
	}
	if _, exact := rest.CentsExact(p4); !exact {
		return true, risk.Anomaly{
			Class: "BOOK_PRICE_GRANULARITY", Sev: risk.SEV2, Ticker: ticker,
			Text: fmt.Sprintf("book price %q in %s is %d/1e4 dollars, which is "+
				"not an integer cent; H-CO-3a asserts integer-cent resting "+
				"prices rather than assuming them, and tick size is the "+
				"exchange's to change", s, field, p4),
		}
	}
	return false, risk.Anomaly{}
}

func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// confidence: high

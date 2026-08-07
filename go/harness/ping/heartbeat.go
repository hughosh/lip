package ping

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
)

// ---------------------------------------------------------------------------
// Optional metrics
// ---------------------------------------------------------------------------
//
// Every optional metric is a pair, and the zero value is UNKNOWN. §13.3's
// heartbeat is the only regular evidence an unattended operator has, and a
// metric that renders as `0` when it is merely unavailable is the worst
// possible rendering: `capital: $0.00` and `integrated: 0.000` are both
// alarming, both actionable, and both wrong in a way that costs a night. So an
// unset value prints the word `unknown` and cannot be mistaken for a reading.

// OptInt is an optional integer metric.
type OptInt struct {
	V     int64
	Known bool
}

// KnownInt marks an integer metric present.
func KnownInt(v int64) OptInt { return OptInt{V: v, Known: true} }

func (o OptInt) String() string {
	if !o.Known {
		return "unknown"
	}
	return fmt.Sprintf("%d", o.V)
}

// OptMoney is an optional money metric.
type OptMoney struct {
	V     num.Money
	Known bool
}

// KnownMoney marks a money metric present.
func KnownMoney(v num.Money) OptMoney { return OptMoney{V: v, Known: true} }

func (o OptMoney) String() string {
	if !o.Known {
		return "unknown"
	}
	return o.V.String()
}

// OptFloat is an optional ratio metric.
type OptFloat struct {
	V     float64
	Known bool
}

// KnownFloat marks a ratio metric present.
func KnownFloat(v float64) OptFloat { return OptFloat{V: v, Known: true} }

func (o OptFloat) String() string {
	if !o.Known {
		return "unknown"
	}
	return fmt.Sprintf("%.3f", o.V)
}

// OptDuration is an optional duration metric.
type OptDuration struct {
	V     time.Duration
	Known bool
}

// KnownDuration marks a duration metric present.
func KnownDuration(v time.Duration) OptDuration {
	return OptDuration{V: v, Known: true}
}

func (o OptDuration) String() string {
	if !o.Known {
		return "unknown"
	}
	return o.V.Round(time.Second).String()
}

// OptQty is an optional contract-count metric.
type OptQty struct {
	V     num.Qty
	Known bool
}

// KnownQty marks a quantity metric present.
func KnownQty(v num.Qty) OptQty { return OptQty{V: v, Known: true} }

func (o OptQty) String() string {
	if !o.Known {
		return "unknown"
	}
	return o.V.Wire()
}

// OptBool is an optional flag, with distinct renderings for the two values and
// for not knowing.
type OptBool struct {
	V     bool
	Known bool
}

// KnownBool marks a flag present.
func KnownBool(v bool) OptBool { return OptBool{V: v, Known: true} }

func (o OptBool) render(yes, no string) string {
	if !o.Known {
		return "unknown"
	}
	if o.V {
		return yes
	}
	return no
}

// ---------------------------------------------------------------------------
// The heartbeat
// ---------------------------------------------------------------------------

// MarketLine is one market's contribution to §13.3.
type MarketLine struct {
	Ticker string
	State  quote.MarketState
	Q      OptQty
}

// Heartbeat is §13.3's content.
//
// It carries the global state, every managed market's state and q, deployed
// capital, the integrated presence share, uptime, whether the source is stale,
// how many alerts are undelivered, and the store's health. The last two are the
// self-report: a heartbeat that could not say "I have four alerts I have failed
// to send you" would let a delivery outage look exactly like a quiet night.
type Heartbeat struct {
	Global      quote.GlobalState
	Markets     []MarketLine
	Capital     OptMoney
	Integrated  OptFloat
	Uptime      OptDuration
	SourceStale OptBool
	Undelivered OptInt

	// StoreHealthy and StoreDetail are filled in by Service.Step from the live
	// store health; a caller does not set them. H-STORE-3's revocation of
	// adding authority is invisible from outside the process unless the
	// heartbeat says so.
	StoreHealthy bool
	StoreDetail  string
}

// Render produces the notification title and body.
//
// The title carries the global state and the store's health, because a phone
// notification is often read without being opened.
func (h Heartbeat) Render(extra []string) (string, string) {
	store := "healthy"
	if !h.StoreHealthy {
		store = "STORE UNHEALTHY"
	}
	title := fmt.Sprintf("lip %s | store %s", h.Global, strings.ToLower(store))

	var b strings.Builder
	fmt.Fprintf(&b, "global: %s\n", h.Global)
	fmt.Fprintf(&b, "capital: %s\n", h.Capital)
	fmt.Fprintf(&b, "integrated: %s\n", h.Integrated)
	fmt.Fprintf(&b, "uptime: %s\n", h.Uptime)
	fmt.Fprintf(&b, "source: %s\n", h.SourceStale.render("STALE", "fresh"))
	fmt.Fprintf(&b, "undelivered: %s\n", h.Undelivered)
	if h.StoreHealthy {
		b.WriteString("store: healthy\n")
	} else {
		detail := h.StoreDetail
		if detail == "" {
			detail = "no detail reported"
		}
		// H-STORE-3, said out loud. Adding is revoked; reducing, monitoring
		// and this heartbeat are not.
		fmt.Fprintf(&b, "store: UNHEALTHY -- persistence is unavailable, so "+
			"ADDING authority is revoked; reducing, monitoring and alerting "+
			"continue (%s)\n", detail)
	}

	markets := append([]MarketLine(nil), h.Markets...)
	sort.Slice(markets, func(i, j int) bool {
		return markets[i].Ticker < markets[j].Ticker
	})
	if len(markets) == 0 {
		b.WriteString("markets: none\n")
	} else {
		b.WriteString("markets:\n")
		for _, m := range markets {
			fmt.Fprintf(&b, "  %s  %s  q=%s\n", m.Ticker, m.State, m.Q)
		}
	}
	for _, line := range extra {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return title, b.String()
}

// confidence: high

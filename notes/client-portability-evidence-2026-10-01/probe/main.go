// measure is a READ-ONLY latency probe for the lip Kalshi client.
//
// It issues signed GETs through the production rest.HTTPDoer wrapped in a
// rest.WriteGuard with a ZERO WriteArm (read-only by construction: every
// non-GET is refused before the network), and opens the production
// wsx.Supervisor websocket session (orderbook_delta for the given tickers plus
// the exchange-wide trade tape). It never places, amends or cancels anything.
//
// Output: a JSON document (-out) plus a human summary on stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lip/feed"
	"lip/harness/cfg"
	"lip/harness/rest"
	"lip/harness/wsx"
)

// ---------------------------------------------------------------------------
// stats helpers
// ---------------------------------------------------------------------------

type dist struct {
	N                  int     `json:"n"`
	MinMs, P50Ms, P90Ms float64 `json:"-"`
	MaxMs              float64 `json:"-"`
	Min, P50, P90, Max string  `json:"min,omitempty"`
}

type distOut struct {
	N   int     `json:"n"`
	Min float64 `json:"min_ms"`
	P50 float64 `json:"p50_ms"`
	P90 float64 `json:"p90_ms"`
	Max float64 `json:"max_ms"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func summarize(ds []time.Duration) distOut {
	if len(ds) == 0 {
		return distOut{}
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) time.Duration { return s[int(float64(len(s)-1)*p+0.5)] }
	return distOut{N: len(s), Min: ms(s[0]), P50: ms(at(0.5)), P90: ms(at(0.9)), Max: ms(s[len(s)-1])}
}

func summarizeMs(vs []float64) distOut {
	if len(vs) == 0 {
		return distOut{}
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	at := func(p float64) float64 { return s[int(float64(len(s)-1)*p+0.5)] }
	return distOut{N: len(s), Min: s[0], P50: at(0.5), P90: at(0.9), Max: s[len(s)-1]}
}

// ---------------------------------------------------------------------------
// REST probes
// ---------------------------------------------------------------------------

type restProbe struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	Query    string         `json:"query,omitempty"`
	FirstMs  float64        `json:"first_ms"`
	Warm     distOut        `json:"warm"`
	Statuses map[string]int `json:"statuses"`
	Bytes    int            `json:"last_body_bytes"`
	Errors   []string       `json:"errors,omitempty"`
}

func probeREST(ctx context.Context, d rest.Doer, name, path string, q url.Values, n int, gap time.Duration) restProbe {
	p := restProbe{Name: name, Path: path, Query: q.Encode(), Statuses: map[string]int{}}
	var warm []time.Duration
	for i := 0; i < n; i++ {
		t0 := time.Now()
		resp, err := d.Do(ctx, rest.Request{Method: "GET", Path: path, Query: q})
		dur := time.Since(t0)
		if err != nil {
			p.Errors = append(p.Errors, err.Error())
		} else {
			p.Statuses[strconv.Itoa(resp.Status)]++
			p.Bytes = len(resp.Body)
		}
		if i == 0 {
			p.FirstMs = ms(dur)
		} else {
			warm = append(warm, dur)
		}
		time.Sleep(gap)
	}
	p.Warm = summarize(warm)
	return p
}

type typedProbe struct {
	Name    string  `json:"name"`
	Ms      float64 `json:"ms"`
	Outcome string  `json:"outcome"`
}

type concProbe struct {
	Round     int       `json:"round"`
	WallMs    float64   `json:"wall_ms"`
	Each      distOut   `json:"each"`
	Statuses  []int     `json:"statuses"`
	Tickers   []string  `json:"tickers"`
	IndivMs   []float64 `json:"individual_ms"`
}

func probeConcurrent(ctx context.Context, d rest.Doer, tickers []string, round int) concProbe {
	out := concProbe{Round: round, Tickers: tickers}
	var mu sync.Mutex
	var wg sync.WaitGroup
	durs := make([]time.Duration, len(tickers))
	stat := make([]int, len(tickers))
	t0 := time.Now()
	for i, t := range tickers {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			s := time.Now()
			resp, err := d.Do(ctx, rest.Request{Method: "GET", Path: "/markets/" + t + "/orderbook"})
			dd := time.Since(s)
			mu.Lock()
			durs[i] = dd
			if err == nil {
				stat[i] = resp.Status
			}
			mu.Unlock()
		}(i, t)
	}
	wg.Wait()
	out.WallMs = ms(time.Since(t0))
	out.Each = summarize(durs)
	out.Statuses = stat
	for _, dd := range durs {
		out.IndivMs = append(out.IndivMs, ms(dd))
	}
	return out
}

// ---------------------------------------------------------------------------
// websocket probe (production wsx.Supervisor)
// ---------------------------------------------------------------------------

type wsProbe struct {
	Tickers            []string           `json:"tickers"`
	DurationS          float64            `json:"duration_s"`
	ConnectMs          float64            `json:"first_connect_ms"`
	FirstSnapshotMs    float64            `json:"first_snapshot_after_connect_ms"`
	FirstDeltaMs       float64            `json:"first_delta_after_connect_ms"`
	Connects           int                `json:"connects"`
	Disconnects        int                `json:"disconnects"`
	DisconnectCauses   []string           `json:"disconnect_causes,omitempty"`
	Frames             map[string]int     `json:"frames_by_kind"`
	DeltasByTicker     map[string]int     `json:"deltas_by_ticker"`
	TradesByTicker     map[string]int     `json:"trades_by_subscribed_ticker"`
	TradeTickersSeen   int                `json:"distinct_trade_tickers_seen"`
	TopTradeTickers    []string           `json:"top_trade_tickers"`
	InterFrameGap      distOut            `json:"inter_frame_gap_ms_all"`
	InterDeltaGap      distOut            `json:"inter_frame_gap_ms_subscribed_deltas"`
	TradeLag           distOut            `json:"trade_exchange_ts_to_local_ms"`
	DeltaLag           distOut            `json:"delta_exchange_ts_to_local_ms"`
	TradeLagField      string             `json:"trade_lag_field"`
	DeltaLagField      string             `json:"delta_lag_field"`
	SampleTradeFrame   string             `json:"sample_trade_frame"`
	SampleDeltaFrame   string             `json:"sample_delta_frame"`
	SampleOtherFrames  []string           `json:"sample_other_frames"`
	TradeRatePerS      float64            `json:"exchange_wide_trade_frames_per_s"`
}

type tsMsg struct {
	Msg struct {
		TsMs *int64          `json:"ts_ms"`
		Ts   json.RawMessage `json:"ts"`
	} `json:"msg"`
}

// exchangeTs returns the exchange timestamp in ms and which field carried it.
func exchangeTs(frame []byte) (int64, string, bool) {
	var m tsMsg
	if json.Unmarshal(frame, &m) != nil {
		return 0, "", false
	}
	if m.Msg.TsMs != nil {
		return *m.Msg.TsMs, "ts_ms", true
	}
	raw := strings.TrimSpace(string(m.Msg.Ts))
	if raw == "" || raw == "null" {
		return 0, "", false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n < 1e12 { // seconds
			return n * 1000, "ts(s)", true
		}
		return n, "ts(ms)", true
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		if f < 1e12 {
			return int64(f * 1000), "ts(s,float)", true
		}
		return int64(f), "ts(ms,float)", true
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.Trim(raw, `"`)); err == nil {
		return t.UnixMilli(), "ts(rfc3339)", true
	}
	return 0, "", false
}

func trunc(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

func probeWS(ctx context.Context, signer *feed.Signer, tickers []string, dur time.Duration) (wsProbe, error) {
	out := wsProbe{Tickers: tickers, DurationS: dur.Seconds(),
		Frames: map[string]int{}, DeltasByTicker: map[string]int{}, TradesByTicker: map[string]int{}}
	subscribed := map[string]bool{}
	for _, t := range tickers {
		subscribed[t] = true
	}
	sup, err := wsx.NewSupervisor(signer, wsx.NewLiveDialer(), wsx.NewSystemClock(), cfg.Default(), tickers)
	if err != nil {
		return out, err
	}
	events := make(chan wsx.Event, 4096)
	cmds := make(chan wsx.Command)
	wctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	errc := make(chan error, 1)
	start := time.Now()
	go func() { errc <- sup.Run(wctx, cmds, events) }()

	var (
		lastMono, lastDeltaMono time.Duration
		haveLast, haveLastDelta bool
		gaps, deltaGaps         []time.Duration
		tradeLag, deltaLag      []float64
		connectAt               time.Duration
		seenSnapshot, seenDelta bool
		tradeTickers            = map[string]int{}
		firstFrameMono          time.Duration
		lastFrameMono           time.Duration
	)
loop:
	for {
		select {
		case <-wctx.Done():
			break loop
		case e := <-events:
			switch e.Kind {
			case wsx.EventConnected:
				out.Connects++
				if out.Connects == 1 {
					out.ConnectMs = ms(time.Since(start))
				}
				connectAt = e.At.Mono
				seenSnapshot, seenDelta = false, false
			case wsx.EventDisconnected:
				out.Disconnects++
				if e.Cause != nil && len(out.DisconnectCauses) < 10 {
					out.DisconnectCauses = append(out.DisconnectCauses, e.Cause.Error())
				}
			case wsx.EventFrame:
				info := wsx.InspectFrame(e.Frame)
				kind := info.Kind.String()
				out.Frames[kind]++
				if haveLast {
					gaps = append(gaps, e.At.Mono-lastMono)
				} else {
					firstFrameMono = e.At.Mono
				}
				lastMono, haveLast = e.At.Mono, true
				lastFrameMono = e.At.Mono
				switch info.Kind {
				case wsx.FrameSnapshot:
					if !seenSnapshot && out.Connects == 1 {
						out.FirstSnapshotMs = ms(e.At.Mono - connectAt)
					}
					seenSnapshot = true
				case wsx.FrameDelta:
					out.DeltasByTicker[info.Ticker]++
					if !seenDelta && out.Connects == 1 {
						out.FirstDeltaMs = ms(e.At.Mono - connectAt)
					}
					seenDelta = true
					if haveLastDelta {
						deltaGaps = append(deltaGaps, e.At.Mono-lastDeltaMono)
					}
					lastDeltaMono, haveLastDelta = e.At.Mono, true
					if ts, field, ok := exchangeTs(e.Frame); ok {
						deltaLag = append(deltaLag, float64(e.At.WallMs-ts))
						out.DeltaLagField = field
					}
					if out.SampleDeltaFrame == "" {
						out.SampleDeltaFrame = trunc(e.Frame, 400)
					}
				case wsx.FrameTrade:
					tradeTickers[info.Ticker]++
					if subscribed[info.Ticker] {
						out.TradesByTicker[info.Ticker]++
					}
					if ts, field, ok := exchangeTs(e.Frame); ok {
						tradeLag = append(tradeLag, float64(e.At.WallMs-ts))
						out.TradeLagField = field
					}
					if out.SampleTradeFrame == "" {
						out.SampleTradeFrame = trunc(e.Frame, 400)
					}
				default:
					if len(out.SampleOtherFrames) < 4 {
						out.SampleOtherFrames = append(out.SampleOtherFrames, trunc(e.Frame, 300))
					}
				}
			}
		}
	}
	<-errc
	out.TradeTickersSeen = len(tradeTickers)
	type tc struct {
		T string
		N int
	}
	var ranked []tc
	for t, n := range tradeTickers {
		ranked = append(ranked, tc{t, n})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].N > ranked[j].N })
	for i := 0; i < len(ranked) && i < 10; i++ {
		out.TopTradeTickers = append(out.TopTradeTickers, fmt.Sprintf("%s:%d", ranked[i].T, ranked[i].N))
	}
	out.InterFrameGap = summarize(gaps)
	out.InterDeltaGap = summarize(deltaGaps)
	out.TradeLag = summarizeMs(tradeLag)
	out.DeltaLag = summarizeMs(deltaLag)
	if span := lastFrameMono - firstFrameMono; span > 0 {
		out.TradeRatePerS = float64(out.Frames["trade"]) / span.Seconds()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

type report struct {
	StartedUTC   string       `json:"started_utc"`
	GoVersion    string       `json:"go_version"`
	Host         string       `json:"rest_host"`
	SignMs       distOut      `json:"rsa_pss_sign_ms"`
	ColdConnect  distOut      `json:"cold_connection_get_exchange_status_ms"`
	REST         []restProbe  `json:"rest"`
	Typed        []typedProbe `json:"typed_client_path"`
	Concurrent   []concProbe  `json:"concurrent_orderbook_gets"`
	WS           wsProbe      `json:"websocket"`
	GuardChecked string       `json:"write_guard_check"`
}

func main() {
	tickersFlag := flag.String("tickers", "", "comma-separated market tickers to subscribe/poll (required)")
	n := flag.Int("n", 15, "sequential samples per REST endpoint")
	gap := flag.Duration("gap", 250*time.Millisecond, "spacing between sequential REST calls")
	wsDur := flag.Duration("ws", 120*time.Second, "websocket observation window")
	outPath := flag.String("out", "", "JSON output path")
	host := flag.String("host", feed.RestHost, "REST host to measure (production default is the shared host)")
	restOnly := flag.Bool("rest-only", false, "skip the websocket window")
	flag.Parse()
	tickers := strings.Split(*tickersFlag, ",")
	if *tickersFlag == "" || len(tickers) == 0 {
		fmt.Fprintln(os.Stderr, "-tickers is required")
		os.Exit(2)
	}
	ctx := context.Background()
	rep := report{StartedUTC: time.Now().UTC().Format(time.RFC3339), GoVersion: runtime.Version(), Host: *host}

	signer, err := feed.NewSigner()
	if err != nil {
		fmt.Fprintln(os.Stderr, "signer:", err)
		os.Exit(1)
	}

	// 1. signing cost (RSA-PSS SHA-256, 2048-bit)
	var signD []time.Duration
	for i := 0; i < 200; i++ {
		t0 := time.Now()
		if _, err := signer.Headers(time.Now().UnixMilli(), "GET", rest.APIPrefix+"/portfolio/balance"); err != nil {
			fmt.Fprintln(os.Stderr, "sign:", err)
			os.Exit(1)
		}
		signD = append(signD, time.Since(t0))
	}
	rep.SignMs = summarize(signD)

	// 2. cold connections: a fresh HTTPDoer (fresh transport pool) per call.
	var cold []time.Duration
	for i := 0; i < 3; i++ {
		d := rest.NewHTTPDoer(signer, 10*time.Second)
		d.Host = *host
		t0 := time.Now()
		if _, err := d.Do(ctx, rest.Request{Method: "GET", Path: "/exchange/status"}); err != nil {
			fmt.Fprintln(os.Stderr, "cold get:", err)
		}
		cold = append(cold, time.Since(t0))
		time.Sleep(300 * time.Millisecond)
	}
	rep.ColdConnect = summarize(cold)

	// 3. the production read path: HTTPDoer under a zero-armed WriteGuard.
	raw := rest.NewHTTPDoer(signer, 10*time.Second)
	raw.Host = *host
	guard, err := rest.NewWriteGuard(raw, rest.WriteArm{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "guard:", err)
		os.Exit(1)
	}
	// Prove the guard refuses a write before the network, without sending one.
	if _, err := guard.Do(ctx, rest.Request{Method: "POST", Path: "/portfolio/events/orders", Body: []byte(`{}`)}); err != nil && !rest.WasSent(err) {
		rep.GuardChecked = "POST refused before transmission: " + err.Error()
	} else {
		fmt.Fprintln(os.Stderr, "FATAL: write guard did not refuse a POST; aborting")
		os.Exit(1)
	}
	var d rest.Doer = guard

	t0 := tickers[0]
	probes := []struct {
		name, path string
		q          url.Values
	}{
		{"exchange_status", "/exchange/status", nil},
		{"balance", "/portfolio/balance", nil},
		{"orders_resting_1page", "/portfolio/orders", url.Values{"status": {"resting"}, "limit": {"1000"}}},
		{"positions_1page", "/portfolio/positions", url.Values{"limit": {"200"}}},
		{"fills_1page", "/portfolio/fills", url.Values{"limit": {"1000"}}},
		{"orderbook", "/markets/" + t0 + "/orderbook", nil},
		{"market", "/markets/" + t0, nil},
	}
	for _, p := range probes {
		rep.REST = append(rep.REST, probeREST(ctx, d, p.name, p.path, p.q, *n, *gap))
	}

	// 4. typed client path (parsing + validation included), once each.
	c := rest.NewClient(d)
	{
		s := time.Now()
		_, err := c.Balance(ctx)
		rep.Typed = append(rep.Typed, typedProbe{"Balance", ms(time.Since(s)), fmt.Sprint(err)})
		s = time.Now()
		ob := c.Orderbook(ctx, t0)
		rep.Typed = append(rep.Typed, typedProbe{"Orderbook", ms(time.Since(s)), ob.Outcome.String()})
		s = time.Now()
		or := c.Orders(ctx, "", rest.StatusResting)
		rep.Typed = append(rep.Typed, typedProbe{"Orders(resting, all tickers)", ms(time.Since(s)), or.Walk.Outcome.String()})
		s = time.Now()
		ps := c.Positions(ctx)
		rep.Typed = append(rep.Typed, typedProbe{"Positions", ms(time.Since(s)), ps.Walk.Outcome.String()})
		s = time.Now()
		fl := c.Fills(ctx, "", time.Now().Add(-24*time.Hour))
		rep.Typed = append(rep.Typed, typedProbe{"Fills(24h, complete walk)", ms(time.Since(s)), fl.Walk.Outcome.String()})
	}

	// 5. concurrent orderbook GETs across tickers (round 1 opens pool conns, round 2 warm).
	conc := tickers
	if len(conc) > 6 {
		conc = conc[:6]
	}
	for r := 1; r <= 2; r++ {
		rep.Concurrent = append(rep.Concurrent, probeConcurrent(ctx, d, conc, r))
		time.Sleep(500 * time.Millisecond)
	}

	// 6. websocket via the production supervisor.
	if !*restOnly {
		ws, err := probeWS(ctx, signer, tickers, *wsDur)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ws:", err)
		}
		rep.WS = ws
	}

	js, _ := json.MarshalIndent(rep, "", "  ")
	if *outPath != "" {
		if err := os.WriteFile(*outPath, js, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
		}
	}
	// human summary
	fmt.Printf("started %s go=%s host=%s\n", rep.StartedUTC, rep.GoVersion, rep.Host)
	fmt.Printf("guard: %s\n", rep.GuardChecked)
	fmt.Printf("sign RSA-PSS: p50=%.2fms p90=%.2fms max=%.2fms (n=%d)\n", rep.SignMs.P50, rep.SignMs.P90, rep.SignMs.Max, rep.SignMs.N)
	fmt.Printf("cold connection GET /exchange/status: p50=%.0fms max=%.0fms (n=%d)\n", rep.ColdConnect.P50, rep.ColdConnect.Max, rep.ColdConnect.N)
	fmt.Printf("%-24s %8s %8s %8s %8s %8s  %s\n", "warm REST", "first", "min", "p50", "p90", "max", "status/bytes")
	for _, p := range rep.REST {
		fmt.Printf("%-24s %8.0f %8.0f %8.0f %8.0f %8.0f  %v/%d %s\n", p.Name, p.FirstMs, p.Warm.Min, p.Warm.P50, p.Warm.P90, p.Warm.Max, p.Statuses, p.Bytes, strings.Join(p.Errors, ";"))
	}
	for _, t := range rep.Typed {
		fmt.Printf("typed %-30s %8.0fms  %s\n", t.Name, t.Ms, t.Outcome)
	}
	for _, cp := range rep.Concurrent {
		fmt.Printf("concurrent orderbook x%d round %d: wall=%.0fms each p50=%.0f max=%.0f statuses=%v\n", len(cp.Tickers), cp.Round, cp.WallMs, cp.Each.P50, cp.Each.Max, cp.Statuses)
	}
	w := rep.WS
	fmt.Printf("ws: connect=%.0fms first_snapshot=+%.0fms first_delta=+%.0fms connects=%d disconnects=%d causes=%v\n", w.ConnectMs, w.FirstSnapshotMs, w.FirstDeltaMs, w.Connects, w.Disconnects, w.DisconnectCauses)
	fmt.Printf("ws frames: %v  deltas_by_ticker=%v  trades_on_subscribed=%v  distinct_trade_tickers=%d  trade_rate=%.1f/s\n", w.Frames, w.DeltasByTicker, w.TradesByTicker, w.TradeTickersSeen, w.TradeRatePerS)
	fmt.Printf("ws inter-frame gap (all): p50=%.0fms p90=%.0fms max=%.0fms n=%d; subscribed deltas: p50=%.0fms p90=%.0fms max=%.0fms n=%d\n", w.InterFrameGap.P50, w.InterFrameGap.P90, w.InterFrameGap.Max, w.InterFrameGap.N, w.InterDeltaGap.P50, w.InterDeltaGap.P90, w.InterDeltaGap.Max, w.InterDeltaGap.N)
	fmt.Printf("ws exchange-ts -> local: trade[%s] p50=%.0fms p90=%.0fms max=%.0fms n=%d; delta[%s] p50=%.0fms p90=%.0fms n=%d\n", w.TradeLagField, w.TradeLag.P50, w.TradeLag.P90, w.TradeLag.Max, w.TradeLag.N, w.DeltaLagField, w.DeltaLag.P50, w.DeltaLag.P90, w.DeltaLag.N)
	fmt.Printf("ws top traded tickers: %v\n", w.TopTradeTickers)
	fmt.Printf("sample trade frame: %s\nsample delta frame: %s\n", w.SampleTradeFrame, w.SampleDeltaFrame)
	for _, o := range w.SampleOtherFrames {
		fmt.Printf("other frame: %s\n", o)
	}
}

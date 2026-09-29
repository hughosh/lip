// incentives inspects public Kalshi incentive programs. It has no account or
// trading configuration and sends GETs only to the four public routes below.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lip/harness/rest"
)

const apiBase = "https://external-api.kalshi.com/trade-api/v2"

var tickerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type publicDoer struct{ client *http.Client }

func (d publicDoer) Do(ctx context.Context, r rest.Request) (rest.Response, error) {
	if r.Method != http.MethodGet || len(r.Body) != 0 || !publicPath(r.Path) {
		return rest.Response{}, fmt.Errorf("refused non-public read: %s %s", r.Method, r.Path)
	}
	u, err := url.Parse(apiBase + r.Path)
	if err != nil {
		return rest.Response{}, err
	}
	u.RawQuery = r.Query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return rest.Response{}, err
	}
	client := *d.client
	// Even a public GET must not follow a redirect to an unapproved host or
	// route. The 3xx response is reported as an HTTP failure by the caller.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return rest.Response{}, err
	}
	defer resp.Body.Close()
	// A bounded body makes a malformed public response a failure, not a memory
	// sink. The list endpoint's measured page limit is 1000 records.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return rest.Response{}, err
	}
	if len(body) > 8<<20 {
		return rest.Response{}, errors.New("public response exceeds 8 MiB")
	}
	return rest.Response{Status: resp.StatusCode, Body: body}, nil
}

func publicPath(path string) bool {
	if path == "/incentive_programs" {
		return true
	}
	for _, prefix := range []string{"/markets/", "/events/", "/series/"} {
		if strings.HasPrefix(path, prefix) && tickerPattern.MatchString(strings.TrimPrefix(path, prefix)) {
			return true
		}
	}
	return false
}

type selection struct {
	Ticker   string            `json:"ticker"`
	Programs []json.RawMessage `json:"programs"`
	Market   json.RawMessage   `json:"market"`
	Event    json.RawMessage   `json:"event"`
	Series   json.RawMessage   `json:"series"`
}

type report struct {
	StartedAtUTC   time.Time `json:"started_at_utc"`
	CompletedAtUTC time.Time `json:"completed_at_utc"`
	Complete       bool      `json:"complete"`
	// These are sequential public reads, not an atomic exchange snapshot.
	ObservationNote string            `json:"observation_note"`
	Pages           int               `json:"pages"`
	TotalCount      int               `json:"total_count"`
	Programs        []json.RawMessage `json:"programs"`
	Selected        *selection        `json:"selected,omitempty"`
}

type programFields struct {
	MarketTicker      string      `json:"market_ticker"`
	IncentiveType     string      `json:"incentive_type"`
	PeriodReward      json.Number `json:"period_reward"`
	StartDate         string      `json:"start_date"`
	EndDate           string      `json:"end_date"`
	TargetSizeFP      string      `json:"target_size_fp"`
	DiscountFactorBPS json.Number `json:"discount_factor_bps"`
}

func validateProgram(raw json.RawMessage) (string, error) {
	var p programFields
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if !tickerPattern.MatchString(p.MarketTicker) || p.IncentiveType != "liquidity" {
		return "", fmt.Errorf("invalid market_ticker or non-liquidity program")
	}
	var start, end time.Time
	for name, value := range map[string]string{"start_date": p.StartDate, "end_date": p.EndDate} {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if name == "start_date" {
			start = parsed
		} else {
			end = parsed
		}
	}
	if !end.After(start) {
		return "", fmt.Errorf("end_date must be after start_date")
	}
	size, err := strconv.ParseFloat(p.TargetSizeFP, 64)
	if err != nil || math.IsNaN(size) || math.IsInf(size, 0) || size <= 0 {
		return "", fmt.Errorf("invalid target_size_fp %q", p.TargetSizeFP)
	}
	for name, value := range map[string]json.Number{"period_reward": p.PeriodReward, "discount_factor_bps": p.DiscountFactorBPS} {
		if value == "" {
			return "", fmt.Errorf("missing %s", name)
		}
		n, err := value.Int64()
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if n < 0 {
			return "", fmt.Errorf("%s is negative", name)
		}
		if name == "discount_factor_bps" && n > 10000 {
			return "", fmt.Errorf("discount_factor_bps exceeds 10000")
		}
	}
	return p.MarketTicker, nil
}

func publicObject(ctx context.Context, d rest.Doer, path, key, expectedTicker string) (json.RawMessage, error) {
	resp, err := d.Do(ctx, rest.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.Status)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw := envelope[key]
	var fields map[string]json.RawMessage
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("%s: missing or malformed %s", path, key)
	}
	idField := "ticker"
	if key == "event" {
		idField = "event_ticker"
	}
	var ticker string
	if err := json.Unmarshal(fields[idField], &ticker); err != nil || ticker != expectedTicker {
		return nil, fmt.Errorf("%s: ticker mismatch", path)
	}
	return raw, nil
}

func inspect(ctx context.Context, d rest.Doer, ticker string, now func() time.Time) (report, error) {
	started := now().UTC()
	filters := url.Values{"status": {"active"}, "type": {"liquidity"}}
	w := rest.NewClient(d).Walk(ctx, rest.EpPrograms, filters)
	if !w.Replaces() {
		if w.Err != nil {
			return report{}, w.Err
		}
		return report{}, fmt.Errorf("incentive walk %s after %d pages", w.Outcome, w.Pages)
	}
	programs := w.Records("incentive_programs")
	out := report{StartedAtUTC: started, Complete: true, Pages: w.Pages,
		ObservationNote: "Sequential public reads; not an atomic exchange snapshot.",
		TotalCount:      len(programs), Programs: programs}
	if out.Programs == nil {
		out.Programs = []json.RawMessage{}
	}
	var selected []json.RawMessage
	for i, raw := range programs {
		marketTicker, err := validateProgram(raw)
		if err != nil {
			return report{}, fmt.Errorf("program %d: %w", i, err)
		}
		if marketTicker == ticker {
			selected = append(selected, raw)
		}
	}
	if ticker == "" {
		out.CompletedAtUTC = now().UTC()
		return out, nil
	}
	if len(selected) == 0 {
		return report{}, fmt.Errorf("%s has no active liquidity incentive program", ticker)
	}
	market, err := publicObject(ctx, d, "/markets/"+ticker, "market", ticker)
	if err != nil {
		return report{}, err
	}
	var m struct {
		EventTicker string `json:"event_ticker"`
	}
	if err := json.Unmarshal(market, &m); err != nil || !tickerPattern.MatchString(m.EventTicker) {
		return report{}, fmt.Errorf("%s: missing or malformed event_ticker", ticker)
	}
	event, err := publicObject(ctx, d, "/events/"+m.EventTicker, "event", m.EventTicker)
	if err != nil {
		return report{}, err
	}
	var e struct {
		SeriesTicker string `json:"series_ticker"`
	}
	if err := json.Unmarshal(event, &e); err != nil || !tickerPattern.MatchString(e.SeriesTicker) {
		return report{}, fmt.Errorf("%s: missing or malformed series_ticker", m.EventTicker)
	}
	series, err := publicObject(ctx, d, "/series/"+e.SeriesTicker, "series", e.SeriesTicker)
	if err != nil {
		return report{}, err
	}
	out.Selected = &selection{Ticker: ticker, Programs: selected, Market: market, Event: event, Series: series}
	out.CompletedAtUTC = now().UTC()
	return out, nil
}

func main() {
	ticker := flag.String("ticker", "", "inspect one incentivized market and its public series metadata")
	flag.Parse()
	if flag.NArg() != 0 || (*ticker != "" && !tickerPattern.MatchString(*ticker)) {
		fmt.Fprintln(os.Stderr, "invalid arguments or ticker")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d := publicDoer{client: &http.Client{Timeout: 30 * time.Second}}
	out, err := inspect(ctx, d, *ticker, time.Now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// confidence: high

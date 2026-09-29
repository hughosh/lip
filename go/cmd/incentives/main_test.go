package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lip/harness/rest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func fixtureProgram(ticker string) string {
	return fmt.Sprintf(`{"market_ticker":%q,"incentive_type":"liquidity","period_reward":200000,"start_date":"2026-09-26T01:00:00Z","end_date":"2026-09-26T02:00:00Z","target_size_fp":"300.00","discount_factor_bps":5000}`, ticker)
}

func TestInspectWalksAllPagesWithFiltersAndSelectedMetadata(t *testing.T) {
	var paths []string
	d := publicDoer{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.URL.Host != "external-api.kalshi.com" {
			t.Fatalf("non-public request: %s %s headers=%v", r.Method, r.URL, r.Header)
		}
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/trade-api/v2/incentive_programs":
			q := r.URL.Query()
			if q.Get("status") != "active" || q.Get("type") != "liquidity" || q.Get("limit") == "" {
				t.Fatalf("lost program filters: %v", q)
			}
			switch q.Get("cursor") {
			case "":
				return response(200, `{"next_cursor":"second","incentive_programs":[`+fixtureProgram("FIRST")+`]}`), nil
			case "second":
				return response(200, `{"next_cursor":"","incentive_programs":[`+fixtureProgram("SECOND")+`]}`), nil
			}
		case "/trade-api/v2/markets/SECOND":
			return response(200, `{"market":{"ticker":"SECOND","event_ticker":"EVENT","maker_fee":"raw-market-fee"}}`), nil
		case "/trade-api/v2/events/EVENT":
			return response(200, `{"event":{"event_ticker":"EVENT","series_ticker":"SERIES"}}`), nil
		case "/trade-api/v2/series/SERIES":
			return response(200, `{"series":{"ticker":"SERIES","fee_multiplier":0.5}}`), nil
		}
		t.Fatalf("unexpected route: %s", r.URL)
		return nil, nil
	})}}
	stamp := time.Date(2026, 9, 26, 3, 4, 5, 0, time.FixedZone("other", 3600))
	out, err := inspect(context.Background(), d, "SECOND", func() time.Time { return stamp })
	if err != nil {
		t.Fatal(err)
	}
	if !out.Complete || out.Pages != 2 || out.TotalCount != 2 || len(out.Programs) != 2 ||
		out.Selected == nil || len(out.Selected.Programs) != 1 || out.StartedAtUTC.Location() != time.UTC || out.CompletedAtUTC.Location() != time.UTC {
		t.Fatalf("incomplete output: %+v", out)
	}
	if !strings.Contains(string(out.Selected.Market), "raw-market-fee") || !strings.Contains(string(out.Selected.Event), `"series_ticker":"SERIES"`) || !strings.Contains(string(out.Selected.Series), "fee_multiplier") ||
		!strings.Contains(string(out.Selected.Programs[0]), `"period_reward":200000`) {
		t.Fatalf("raw public fields lost: %+v", out.Selected)
	}
	if len(paths) != 5 {
		t.Fatalf("expected two pages and market/event/series GETs, got %v", paths)
	}
}

func TestInspectFailsClosedOnIncompleteAndMalformedWalks(t *testing.T) {
	cases := []struct{ name, first, second string }{
		{"missing cursor", `{"incentive_programs":[]}`, ""},
		{"missing array", `{"next_cursor":""}`, ""},
		{"wrong array", `{"next_cursor":"","incentive_programs":{}}`, ""},
		{"wrong type", `{"next_cursor":"","incentive_programs":[` + strings.Replace(fixtureProgram("FIRST"), `"liquidity"`, `"volume"`, 1) + `]}`, ""},
		{"missing reward", `{"next_cursor":"","incentive_programs":[` + strings.Replace(fixtureProgram("FIRST"), `"period_reward":200000,`, "", 1) + `]}`, ""},
		{"reversed dates", `{"next_cursor":"","incentive_programs":[` + strings.Replace(fixtureProgram("FIRST"), `"end_date":"2026-09-26T02:00:00Z"`, `"end_date":"2026-09-26T00:00:00Z"`, 1) + `]}`, ""},
		{"bad discount", `{"next_cursor":"","incentive_programs":[` + strings.Replace(fixtureProgram("FIRST"), `"discount_factor_bps":5000`, `"discount_factor_bps":10001`, 1) + `]}`, ""},
		{"repeated cursor", `{"next_cursor":"again","incentive_programs":[]}`, `{"next_cursor":"again","incentive_programs":[]}`},
		{"repeated page", `{"next_cursor":"again","incentive_programs":[` + fixtureProgram("FIRST") + `]}`, `{"next_cursor":"","incentive_programs":[` + fixtureProgram("FIRST") + `]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := publicDoer{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("cursor") != "" {
					return response(200, tc.second), nil
				}
				return response(200, tc.first), nil
			})}}
			if got, err := inspect(context.Background(), d, "", time.Now); err == nil || got.Complete {
				t.Fatalf("malformed/incomplete walk passed: %+v, %v", got, err)
			}
		})
	}
	for _, code := range []int{401, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			d := publicDoer{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(code, `{}`), nil
			})}}
			if got, err := inspect(context.Background(), d, "", time.Now); err == nil || got.Complete {
				t.Fatalf("HTTP failure passed: %+v, %v", got, err)
			}
		})
	}
}

func TestPublicTransportRefusesPrivateAndWriteRoutes(t *testing.T) {
	calls := 0
	d := publicDoer{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(200, `{}`), nil
	})}}
	for _, r := range []rest.Request{
		{Method: "POST", Path: "/incentive_programs"},
		{Method: "GET", Path: "/portfolio/positions"},
		{Method: "GET", Path: "/markets/A/B"},
		{Method: "GET", Path: "/events/A/B"},
		{Method: "GET", Path: "/series/../portfolio"},
		{Method: "GET", Path: "/markets/SAFE", Body: []byte(`{}`)},
	} {
		if _, err := d.Do(context.Background(), r); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	if calls != 0 {
		t.Fatalf("sent %d refused requests", calls)
	}
}

func TestPublicTransportDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	d := publicDoer{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatalf("followed redirect to %s", r.URL)
		}
		resp := response(302, "")
		resp.Header.Set("Location", "https://private.example/portfolio/orders")
		return resp, nil
	})}}
	resp, err := d.Do(context.Background(), rest.Request{Method: "GET", Path: "/incentive_programs"})
	if err != nil || resp.Status != 302 || calls != 1 {
		t.Fatalf("redirect was not returned for fail-closed status handling: %+v, %v, calls=%d", resp, err, calls)
	}
}

package rest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lip/harness/cfg"
)

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, raw string
		want      time.Duration
		valid     bool
	}{
		{"absent", "", 0, false},
		{"seconds", "7", 7 * time.Second, true},
		{"zero", "0", 0, true},
		{"http date", now.Add(13 * time.Second).Format(http.TimeFormat), 13 * time.Second, true},
		{"past date", now.Add(-time.Second).Format(http.TimeFormat), 0, true},
		{"negative", "-3", 0, false},
		{"fractional", "1.5", 0, false},
		{"nonsense", "soon", 0, false},
		{"huge", "18446744073709551615", time.Duration(1<<63 - 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := parseRetryAfter(tc.raw, now)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("parseRetryAfter(%q) = (%s, %v), want (%s, %v)",
					tc.raw, got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestRateLimitOnEveryRESTMethod(t *testing.T) {
	for _, method := range []string{"create", "cancel", "walk", "schedule", "balance"} {
		t.Run(method, func(t *testing.T) {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				if n != 0 {
					t.Fatalf("429 caused an in-slot retry: call %d", n+1)
				}
				return Response{Status: 429, Body: []byte(`{"error":{"code":"rate_limited"}}`),
					Header: http.Header{"Retry-After": {"7"}}}, nil
			}}
			c := NewClient(d)
			var err error
			switch method {
			case "create":
				r := c.Create(context.Background(),
					testOrder(t, "lipH-run1-000-yes-00000099"), cfg.Default())
				if r.Outcome != CreateUnknown || r.MaxLive == 0 ||
					r.RejectReason != "" || !r.ReconcileNow() || r.Attempts != 1 {
					t.Fatalf("429 lost possible create exposure: %+v", r)
				}
				err = r.Err
			case "cancel":
				r := c.Cancel(context.Background(), "o1")
				if r.Outcome != CancelUnknown || !r.Sent ||
					r.RejectReason != "" || r.ReducedBy != 0 {
					t.Fatalf("429 established false cancel fact: %+v", r)
				}
				err = r.Err
			case "walk":
				r := c.Walk(context.Background(), EpOrders, nil)
				if r.Replaces() || r.Outcome != WalkFailed || len(r.Items) != 0 {
					t.Fatalf("429 replaced read state: %+v", r)
				}
				err = r.Err
			case "schedule":
				r := c.Schedule(context.Background(), schedTicker)
				if r.Observed() || r.HasClose {
					t.Fatalf("429 produced schedule: %+v", r)
				}
				err = r.Err
			case "balance":
				_, err = c.Balance(context.Background())
			}
			var throttle *RateLimitError
			if !errors.As(err, &throttle) || !throttle.HasDelay ||
				throttle.Delay != 7*time.Second || throttle.RetryAfter != "7" {
				t.Fatalf("429 retry signal = %v (%+v)", err, throttle)
			}
			if len(d.Calls()) != 1 {
				t.Fatalf("calls = %d, want one", len(d.Calls()))
			}
		})
	}
}

func TestSweepStopsAt429WithoutClaimingAbsence(t *testing.T) {
	coid := "lipH-run1-000-yes-00000099"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n != 0 {
			t.Fatalf("sweep retried or read before backoff: call %d", n+1)
		}
		return Response{Status: 429, Header: http.Header{"Retry-After": {"3"}}}, nil
	}}
	r := NewClient(d).CancelAndSweep(context.Background(), schedTicker,
		[]Order{{OrderID: "o1", ClientOrderID: coid}})
	if r.Clean || r.Throttle == nil || r.Throttle.Delay != 3*time.Second ||
		len(r.Cancels) != 1 || r.Cancels[0].Outcome != CancelUnknown {
		t.Fatalf("429 sweep = %+v", r)
	}
}

type headerRoundTripper struct{}

func (headerRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"11"}},
		Body: io.NopCloser(strings.NewReader(`{"error":"slow down"}`))}, nil
}

func TestHTTPDoerPreservesRetryHeader(t *testing.T) {
	signer, _ := newTestSigner(t)
	d := NewHTTPDoerWithTransport(signer, time.Second, headerRoundTripper{})
	r, err := d.Do(context.Background(), Request{Method: "GET", Path: "/portfolio/balance"})
	if err != nil || r.Status != 429 || r.Header.Get("Retry-After") != "11" {
		t.Fatalf("wire 429 = %+v, %v", r, err)
	}
}

package rest

import (
	"context"
	"strings"
	"testing"
)

// The incentives endpoint defaults type to "all". An active volume program
// can have no target_size_fp; decoding it as a liquidity program would fail the
// entire walk, including a valid pilot ticker on a later page. Both filters
// must survive cursor pagination.
func TestProgramsFiltersActiveLiquidityOnEveryPage(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Path != EpPrograms.Path || req.Query.Get("status") != "active" ||
			req.Query.Get("type") != "liquidity" {
			t.Fatalf("page %d must request active liquidity programs: %+v", n+1, req)
		}
		switch n {
		case 0:
			if got := req.Query.Get("cursor"); got != "" {
				t.Fatalf("first page cursor = %q, want empty", got)
			}
			return jsonPage(EpPrograms, "PAGE2", map[string][]any{
				"incentive_programs": {
					map[string]any{"market_ticker": "OTHER", "target_size_fp": "100.00"},
				},
			}), nil
		case 1:
			if got := req.Query.Get("cursor"); got != "PAGE2" {
				t.Fatalf("second page cursor = %q, want PAGE2", got)
			}
			return jsonPage(EpPrograms, "", map[string][]any{
				"incentive_programs": {
					map[string]any{"market_ticker": "PILOT", "target_size_fp": "250.50"},
				},
			}), nil
		}
		t.Fatalf("unexpected page %d", n+1)
		return Response{}, nil
	}}

	got := NewClient(d).Programs(context.Background())
	if !got.Replaces() {
		t.Fatalf("active liquidity walk failed: %v", got.Err)
	}
	if got.Pages != 2 || len(d.Calls()) != 2 {
		t.Fatalf("walk reached %d pages in %d calls, want two", got.Pages, len(d.Calls()))
	}
	if target, ok := got.ByTarget["PILOT"]; !ok || target != 250.5 {
		t.Fatalf("pilot target = %v (present=%v), want 250.5", target, ok)
	}
}

// Filtering by type must not turn an ill-formed liquidity program into a
// successful partial universe. A missing target still refuses the whole walk.
func TestProgramsLiquidityFilterKeepsMalformedTargetRefusal(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(_ int, req Request) (Response, error) {
		if req.Query.Get("type") != "liquidity" {
			t.Fatalf("type filter = %q, want liquidity", req.Query.Get("type"))
		}
		return jsonPage(EpPrograms, "", map[string][]any{
			"incentive_programs": {
				map[string]any{"market_ticker": "PILOT", "incentive_type": "liquidity"},
			},
		}), nil
	}}

	got := NewClient(d).Programs(context.Background())
	if got.Replaces() || len(got.ByTarget) != 0 || got.Err == nil ||
		!strings.Contains(got.Err.Error(), "target_size_fp") {
		t.Fatalf("malformed liquidity target must refuse the walk: %+v", got)
	}
}

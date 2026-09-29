package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"lip/harness/rest"
)

func selectionBook(best, deep, bestSize, deepSize int) rest.OrderbookResult {
	levels := []rest.BookLevel{
		{Cents: best, Size: float64(bestSize), Wire: [2]string{fmt.Sprintf("0.%02d00", best), fmt.Sprintf("%d.00", bestSize)}},
		{Cents: deep, Size: float64(deepSize), Wire: [2]string{fmt.Sprintf("0.%02d00", deep), fmt.Sprintf("%d.00", deepSize)}},
	}
	return rest.OrderbookResult{Outcome: rest.OrderbookRead, Ticker: "A", Yes: levels, No: levels}
}

func TestTurnoverCandidateFilters(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	p := turnoverProgram{ID: "p", Ticker: "A", Target: 100, Pool: 100, Discount: .5, Start: now.Add(-time.Hour), End: now.Add(4 * time.Hour)}
	s := rest.ScheduleResult{ScheduleRead: rest.ScheduleRead{Outcome: rest.ScheduleObserved}, Ticker: "A", Status: rest.MarketStatusActive, HasClose: true, CloseTime: now.Add(24 * time.Hour)}
	book := selectionBook(49, 48, 80, 50)
	base := evaluateTurnoverCandidate(p, book, s, 12, now, 30*24*time.Hour)
	if !base.Eligible || base.FieldScore <= 0 || base.Score != base.Pool/base.FieldScore || base.PredictedShare > .5 {
		t.Fatalf("eligible baseline: %+v", base)
	}
	tests := []struct {
		name     string
		p        turnoverProgram
		book     rest.OrderbookResult
		schedule rest.ScheduleResult
		reason   string
	}{
		{"elapsed", func() turnoverProgram { x := p; x.Start = now.Add(-2 * time.Hour); return x }(), book, s, "program_elapsed"},
		{"not qualified", func() turnoverProgram { return p }(), selectionBook(49, 48, 80, 10), s, "depth_slack"},
		{"sum bids", p, selectionBook(50, 49, 80, 50), s, "bid_sum"},
		{"far close", p, book, func() rest.ScheduleResult { x := s; x.CloseTime = now.Add(31 * 24 * time.Hour); return x }(), "tenor_or_closed"},
		{"predicted share", p, selectionBook(49, 1, 1, 129), s, "predicted_share"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateTurnoverCandidate(tt.p, tt.book, tt.schedule, 12, now, 30*24*time.Hour)
			if got.Eligible {
				t.Fatalf("eligible despite %s: %+v", tt.reason, got)
			}
			found := false
			for _, r := range got.Reasons {
				if r == tt.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasons=%v, want %s", got.Reasons, tt.reason)
			}
		})
	}
}

func TestTurnoverChooserHysteresisAndExpiration(t *testing.T) {
	rows := []turnoverCandidate{
		{Ticker: "A", Score: 70, Eligible: true}, {Ticker: "B", Score: 100, Eligible: true},
		{Ticker: "C", Score: 101, Eligible: true}, {Ticker: "D", Score: 200, Eligible: false},
	}
	if got := chooseTurnoverSelection(rows, []string{"A", "D"}, 2, .30); !reflect.DeepEqual(got, []string{"C", "A"}) {
		t.Fatalf("within hysteresis and expired removal: %v", got)
	}
	rows[2].Score = 101.1 // 70 < 101.1 * .7: challenger now clears threshold.
	if got := chooseTurnoverSelection(rows, []string{"A", "B"}, 2, .30); !reflect.DeepEqual(got, []string{"C", "B"}) {
		t.Fatalf("replacement beyond hysteresis: %v", got)
	}
	if got := chooseTurnoverSelection(rows, nil, 2, .30); !reflect.DeepEqual(got, []string{"C", "B"}) {
		t.Fatalf("initial rank: %v", got)
	}
	base := time.Now()
	if !turnoverSelectionDue(base, time.Time{}, time.Hour) || turnoverSelectionDue(base, base, time.Hour) || !turnoverSelectionDue(base.Add(time.Hour), base, time.Hour) {
		t.Fatal("reselect cadence boundary")
	}
}

type turnoverSelectionDoer struct {
	calls []rest.Request
	now   time.Time
}

func (d *turnoverSelectionDoer) Do(_ context.Context, req rest.Request) (rest.Response, error) {
	d.calls = append(d.calls, req)
	if req.Method != "GET" {
		return rest.Response{}, fmt.Errorf("write attempted")
	}
	var body any
	switch req.Path {
	case rest.EpPrograms.Path:
		if req.Query.Get("status") != "active" || req.Query.Get("type") != "liquidity" {
			return rest.Response{}, fmt.Errorf("wrong program filter")
		}
		body = map[string]any{"incentive_programs": []any{
			map[string]any{"id": "p-a", "market_ticker": "A", "target_size_fp": "100.00", "period_reward": 1000000, "discount_factor_bps": 5000, "start_date": d.now.Add(-time.Hour).Format(time.RFC3339), "end_date": d.now.Add(4 * time.Hour).Format(time.RFC3339)},
			map[string]any{"id": "p-unapproved", "market_ticker": "UNAPPROVED", "target_size_fp": "100.00"},
		}, "next_cursor": ""}
	case "/markets/A/orderbook":
		body = map[string]any{"orderbook_fp": map[string]any{"yes_dollars": [][]string{{"0.4800", "50.00"}, {"0.4900", "80.00"}}, "no_dollars": [][]string{{"0.4800", "50.00"}, {"0.4900", "80.00"}}}}
	case "/markets/A":
		body = map[string]any{"market": map[string]any{"ticker": "A", "status": "active", "result": "", "close_time": d.now.Add(24 * time.Hour).Format(time.RFC3339), "can_close_early": false}}
	default:
		return rest.Response{}, fmt.Errorf("unapproved read %s", req.Path)
	}
	b, _ := json.Marshal(body)
	return rest.Response{Status: 200, Body: b}, nil
}

func TestTurnoverReadApprovedGETsOnly(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	d := &turnoverSelectionDoer{now: now}
	got := readTurnoverCandidates(context.Background(), rest.NewClient(d), []string{"A", "MISSING"}, 12, now, 30*24*time.Hour)
	if !got.Complete || got.Err != nil || len(got.Candidates) != 2 || !got.Candidates[0].Eligible || got.Candidates[1].Eligible {
		t.Fatalf("selection read: %+v", got)
	}
	if len(d.calls) != 3 {
		t.Fatalf("calls=%v, want program, A book, A schedule only", d.calls)
	}
}

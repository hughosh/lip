package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

type programDoer func(context.Context, rest.Request) (rest.Response, error)

func (f programDoer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	return f(ctx, req)
}

func programPage(t *testing.T, cursor string, records ...map[string]any) rest.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"next_cursor": cursor, "incentive_programs": records,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rest.Response{Status: 200, Body: body}
}

// The page-two record is the selected market, and its end_date is already in
// the past. Only disappearance from a complete active set may stop adding.
func TestProgramMembershipUsesCompleteActiveSetAtOwner(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	h.connectGate()
	h.installBook([][]string{{"0.4000", "20.00"}}, [][]string{{"0.5500", "20.00"}})
	h.installPosition(num.QtyFromFloat(1))
	h.installResting(
		risk.LiveOrder{OrderID: "EX-ADD", Ticker: seamTicker, Side: quote.SideYes,
			Price4: 4000, Remaining: num.QtyFromFloat(12)},
		risk.LiveOrder{OrderID: "EX-EXIT", Ticker: seamTicker, Side: quote.SideNo,
			Price4: 5500, Remaining: num.QtyFromFloat(1)},
	)
	o.closeAt = time.Now().Add(24 * time.Hour)
	o.hasClose = true
	o.scheduleEver = true
	o.evaluate(0)
	if o.market != quote.Quoting {
		t.Fatalf("control market = %s, want QUOTING", o.market)
	}

	phase := "expired period"
	pages := 0
	api := rest.NewClient(programDoer(func(_ context.Context, req rest.Request) (rest.Response, error) {
		if req.Path != rest.EpPrograms.Path || req.Query.Get("status") != "active" ||
			req.Query.Get("type") != "liquidity" {
			t.Fatalf("program poll lacked active liquidity filters: %+v", req)
		}
		pages++
		cursor := req.Query.Get("cursor")
		if cursor == "" {
			switch phase {
			case "expired period", "rolled period", "partial":
				return programPage(t, "PAGE2", map[string]any{
					"market_ticker": "OTHER", "target_size_fp": "100.00",
				}), nil
			case "absent", "absent failed":
				if phase == "absent failed" {
					return rest.Response{}, errors.New("transport down")
				}
				return programPage(t, "", map[string]any{
					"market_ticker": "OTHER", "target_size_fp": "100.00",
				}), nil
			case "returned":
				return programPage(t, "", map[string]any{
					"market_ticker": seamTicker, "target_size_fp": "250.50",
				}), nil
			}
		}
		if cursor == "PAGE2" {
			if phase == "partial" {
				return rest.Response{}, errors.New("page two failed")
			}
			end := "2026-09-25T00:00:00Z"
			if phase == "rolled period" {
				end = "2026-09-26T01:00:00Z"
			}
			return programPage(t, "", map[string]any{
				"market_ticker": seamTicker, "target_size_fp": "250.50",
				"end_date": end,
			}), nil
		}
		t.Fatalf("unexpected cursor %q in phase %q", cursor, phase)
		return rest.Response{}, nil
	}))

	read := func(wantPages int) rest.ProgramsResult {
		t.Helper()
		before := pages
		res := api.Programs(context.Background())
		if got := pages - before; got != wantPages {
			t.Fatalf("%s used %d pages, want %d", phase, got, wantPages)
		}
		o.applyProgramMembership(res)
		return res
	}

	for _, current := range []string{"expired period", "rolled period"} {
		phase = current
		if res := read(2); !res.Replaces() || o.programMembership.ended {
			t.Fatalf("%s: complete active membership must keep adding: %+v, ended=%v",
				phase, res, o.programMembership.ended)
		}
		o.evaluate(0)
		if o.market != quote.Quoting {
			t.Fatalf("%s moved market to %s despite active membership", phase, o.market)
		}
	}

	phase = "partial"
	if res := read(2); res.Replaces() || o.programMembership.ended {
		t.Fatalf("partial walk established absence: %+v, ended=%v", res, o.programMembership.ended)
	}
	o.evaluate(0)
	if o.market != quote.Quoting {
		t.Fatalf("partial walk changed market to %s", o.market)
	}

	phase = "absent"
	if res := read(1); !res.Replaces() || !o.programMembership.ended {
		t.Fatalf("complete absent walk did not establish end: %+v, ended=%v",
			res, o.programMembership.ended)
	}
	o.evaluate(0)
	if o.market != quote.Reducing {
		t.Fatalf("absent program left market %s, want REDUCING", o.market)
	}
	if got := len(seamCancelsOn(h.rig.queue, quote.SideYes)); got != 1 {
		t.Fatalf("adding side has %d cancel intents, want one", got)
	}
	if got := len(seamCancelsOn(h.rig.queue, quote.SideNo)); got != 0 {
		t.Fatalf("reducer has %d cancel intents, want none", got)
	}

	phase = "absent failed"
	if res := read(1); res.Replaces() || !o.programMembership.ended {
		t.Fatalf("failed walk cleared established absence: %+v, ended=%v",
			res, o.programMembership.ended)
	}
	// An unpopulated hand-off (for example, a closed channel's zero value)
	// cannot invent renewed membership after a complete absent observation.
	o.applyProgramMembership(rest.ProgramsResult{})
	if !o.programMembership.ended {
		t.Fatal("an unpopulated program result cleared established absence")
	}
	phase = "returned"
	if res := read(1); !res.Replaces() || o.programMembership.ended {
		t.Fatalf("new active period did not restore membership: %+v, ended=%v",
			res, o.programMembership.ended)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"lip/core"
	"lip/harness/rest"
)

// turnoverCandidate is one explicitly approved ticker's selection evidence.
// An absent or ended program is an ineligible candidate, never a read failure.
type turnoverCandidate struct {
	Ticker         string
	ProgramID      string
	Pool           float64 // dollars in this reward period
	Target         float64
	FieldScore     float64 // sum of the two qualifying side scores
	Score          float64 // pool / field qualifying score, H-SEL-1
	PredictedShare float64 // mean of our two side shares after adding size
	Eligible       bool
	Reasons        []string
	ProgramEnd     time.Time
	MarketClose    time.Time
}

// A failed walk or any failed approved-market read cannot replace a selection.
// Complete means every approved ticker has an evaluated row, including inactive
// programs. Observed is the wall-clock time used for all time-dependent gates.
type turnoverSelectionResult struct {
	Candidates []turnoverCandidate
	Observed   time.Time
	Complete   bool
	Err        error
}

type turnoverProgram struct {
	ID         string
	Ticker     string
	Target     float64
	Pool       float64
	Discount   float64
	Start, End time.Time
}

// readTurnoverCandidates performs only GETs. The one paginated program walk is
// required for H-SEL-2; book and schedule reads are restricted to approved.
// It does not read account state, select held inventory, or install targets.
func readTurnoverCandidates(ctx context.Context, api *rest.Client, approved []string, size float64, now time.Time, maxTenor time.Duration) turnoverSelectionResult {
	res := turnoverSelectionResult{Observed: now}
	if api == nil || size <= 0 || maxTenor <= 0 || len(approved) == 0 {
		res.Err = fmt.Errorf("selection needs a client, positive size and tenor, and approved candidates")
		return res
	}
	seen := make(map[string]bool, len(approved))
	for _, ticker := range approved {
		if ticker == "" || seen[ticker] {
			res.Err = fmt.Errorf("empty or repeated approved selection ticker %q", ticker)
			return res
		}
		seen[ticker] = true
	}
	programs := api.Programs(ctx)
	if !programs.Replaces() {
		res.Err = fmt.Errorf("active program walk %s: %w", programs.Outcome, programs.Err)
		return res
	}
	byTicker := make(map[string]turnoverProgram, len(approved))
	for _, raw := range programs.Records("incentive_programs") {
		var header struct {
			MarketTicker string `json:"market_ticker"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			res.Err = fmt.Errorf("program ticker: %w", err)
			return res
		}
		if !seen[header.MarketTicker] {
			continue
		}
		p, err := parseTurnoverProgram(raw)
		if err != nil {
			res.Err = fmt.Errorf("program %s: %w", header.MarketTicker, err)
			return res
		}
		// Two active records for one ticker make target/reward authority
		// ambiguous. Refuse the whole replacement instead of page-order choice.
		if _, present := byTicker[p.Ticker]; present {
			res.Err = fmt.Errorf("multiple active programs for approved ticker %s", p.Ticker)
			return res
		}
		byTicker[p.Ticker] = p
	}
	for _, ticker := range approved {
		c := turnoverCandidate{Ticker: ticker}
		p, active := byTicker[ticker]
		if !active {
			c.Reasons = []string{"inactive_program"}
			res.Candidates = append(res.Candidates, c)
			continue
		}
		book := api.Orderbook(ctx, ticker)
		if !book.Read() {
			res.Err = fmt.Errorf("selection book %s: %v", ticker, book.Err)
			return res
		}
		schedule := api.Schedule(ctx, ticker)
		if !schedule.Observed() {
			res.Err = fmt.Errorf("selection schedule %s: %v", ticker, schedule.Err)
			return res
		}
		c = evaluateTurnoverCandidate(p, book, schedule, size, now, maxTenor)
		res.Candidates = append(res.Candidates, c)
	}
	res.Complete = true
	return res
}

func parseTurnoverProgram(raw json.RawMessage) (turnoverProgram, error) {
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return turnoverProgram{}, err
	}
	get := func(key string) string {
		var s string
		if json.Unmarshal(wire[key], &s) == nil {
			return s
		}
		return strings.TrimSpace(string(wire[key]))
	}
	parse := func(key string) (float64, error) {
		v, err := strconv.ParseFloat(get(key), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return v, nil
	}
	target, err := parse("target_size_fp")
	if err != nil {
		return turnoverProgram{}, err
	}
	reward, err := parse("period_reward")
	if err != nil {
		return turnoverProgram{}, err
	}
	discount, err := parse("discount_factor_bps")
	if err != nil {
		return turnoverProgram{}, err
	}
	start, err := time.Parse(time.RFC3339Nano, get("start_date"))
	if err != nil {
		return turnoverProgram{}, fmt.Errorf("start_date: %w", err)
	}
	end, err := time.Parse(time.RFC3339Nano, get("end_date"))
	if err != nil {
		return turnoverProgram{}, fmt.Errorf("end_date: %w", err)
	}
	if target <= 0 || reward <= 0 || discount <= 0 || discount > 10000 || !end.After(start) {
		return turnoverProgram{}, fmt.Errorf("invalid target, reward, discount or period")
	}
	return turnoverProgram{ID: get("id"), Ticker: get("market_ticker"), Target: target, Pool: reward / 10000, Discount: discount / 10000, Start: start, End: end}, nil
}

func evaluateTurnoverCandidate(p turnoverProgram, orderbook rest.OrderbookResult, schedule rest.ScheduleResult, size float64, now time.Time, maxTenor time.Duration) turnoverCandidate {
	c := turnoverCandidate{Ticker: p.Ticker, ProgramID: p.ID, Pool: p.Pool, Target: p.Target, ProgramEnd: p.End, MarketClose: schedule.CloseTime}
	bad := func(reason string) { c.Reasons = append(c.Reasons, reason) }
	if now.Before(p.Start) || !now.Before(p.End) || now.Sub(p.Start) > p.End.Sub(p.Start)/4 {
		bad("program_elapsed")
	}
	if schedule.TradingClosed || schedule.Status != rest.MarketStatusActive || !schedule.HasClose || !schedule.CloseTime.After(now) || schedule.CloseTime.Sub(now) > maxTenor {
		bad("tenor_or_closed")
	}
	if len(orderbook.Yes) == 0 || len(orderbook.No) == 0 {
		bad("empty_book")
		return c
	}
	by, bn := orderbook.Yes[0].Cents, orderbook.No[0].Cents
	if by+bn > 99 {
		bad("bid_sum")
	}
	if mid := float64(by+100-bn) / 2; mid < 10 || mid > 90 {
		bad("mid")
	}
	var ysum, nsum float64
	for _, l := range orderbook.Yes {
		ysum += l.Size
	}
	for _, l := range orderbook.No {
		nsum += l.Size
	}
	if math.Min(ysum, nsum) < 1.3*p.Target {
		bad("depth_slack")
	}
	book := core.NewBook(p.Target)
	yes, no := orderbook.Snapshot()
	if err := book.ApplySnapshot(yes, no); err != nil {
		bad("book_parse")
		return c
	}
	if book.Qualifies() != 1 {
		bad("qualifies_now")
	}
	book.ApplyDelta("yes", by, size)
	book.ApplyDelta("no", bn, size)
	if book.Qualifies() != 1 {
		bad("qualifies_after")
	}
	fy, okY := turnoverSideScore(orderbook.Yes, p.Target, p.Discount, 0)
	fn, okN := turnoverSideScore(orderbook.No, p.Target, p.Discount, 0)
	ay, afterY := turnoverSideScore(orderbook.Yes, p.Target, p.Discount, size)
	an, afterN := turnoverSideScore(orderbook.No, p.Target, p.Discount, size)
	if !okY || !okN || !afterY || !afterN || ay <= 0 || an <= 0 {
		bad("field_score")
	} else {
		c.FieldScore = fy + fn
		if c.FieldScore > 0 {
			c.Score = p.Pool / c.FieldScore
		}
		c.PredictedShare = (size/ay + size/an) / 2
		if c.PredictedShare > 0.5 {
			bad("predicted_share")
		}
	}
	c.Eligible = len(c.Reasons) == 0 && c.Score > 0
	return c
}

// turnoverSideScore follows score.py's qualifying walk: stop when cumulative
// size first reaches target, then discount each qualifying level from the touch.
func turnoverSideScore(levels []rest.BookLevel, target, discount, added float64) (float64, bool) {
	if len(levels) == 0 || levels[0].Cents >= 100 {
		return 0, false
	}
	ref := levels[0].Cents
	var total, score float64
	for i, level := range levels {
		size := level.Size
		if i == 0 {
			size += added
		}
		total += size
		score += math.Pow(discount, float64(ref-level.Cents)) * size
		if total >= target {
			return score, true
		}
	}
	return 0, false
}

func turnoverSelectionDue(now, last time.Time, cadence time.Duration) bool {
	return cadence > 0 && (last.IsZero() || !now.Before(last.Add(cadence)))
}

// chooseTurnoverSelection is pure. Ineligible incumbents leave immediately;
// eligible ones resist a challenger until below it by the hysteresis fraction.
// Held inventory is outside this function and remains managed by the owner.
func chooseTurnoverSelection(rows []turnoverCandidate, previous []string, slots int, hysteresis float64) []string {
	if slots <= 0 || hysteresis < 0 || hysteresis >= 1 {
		return nil
	}
	eligible := make(map[string]turnoverCandidate, len(rows))
	for _, c := range rows {
		if c.Eligible && c.Score > 0 {
			eligible[c.Ticker] = c
		}
	}
	selected := make(map[string]bool, slots)
	for _, ticker := range previous {
		if _, ok := eligible[ticker]; ok {
			selected[ticker] = true
		}
	}
	best := make([]turnoverCandidate, 0, len(eligible))
	for _, c := range eligible {
		best = append(best, c)
	}
	sort.Slice(best, func(i, j int) bool {
		if best[i].Score == best[j].Score {
			return best[i].Ticker < best[j].Ticker
		}
		return best[i].Score > best[j].Score
	})
	for len(selected) > slots {
		weak := weakestTurnover(selected, eligible)
		delete(selected, weak)
	}
	for _, challenger := range best {
		if selected[challenger.Ticker] {
			continue
		}
		if len(selected) < slots {
			selected[challenger.Ticker] = true
			continue
		}
		weak := weakestTurnover(selected, eligible)
		if eligible[weak].Score < challenger.Score*(1-hysteresis) {
			delete(selected, weak)
			selected[challenger.Ticker] = true
		}
	}
	out := make([]string, 0, len(selected))
	for _, c := range best {
		if selected[c.Ticker] {
			out = append(out, c.Ticker)
		}
	}
	return out
}

func weakestTurnover(selected map[string]bool, rows map[string]turnoverCandidate) string {
	var weak string
	for ticker := range selected {
		if weak == "" || rows[ticker].Score < rows[weak].Score || rows[ticker].Score == rows[weak].Score && ticker > weak {
			weak = ticker
		}
	}
	return weak
}

// confidence: low

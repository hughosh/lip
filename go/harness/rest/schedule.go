package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// §9 — the market schedule read
// ---------------------------------------------------------------------------
//
// §9's source table names two deadlines with two different meanings, and this
// file reads the second one:
//
//	| program.end_date   | /incentive_programs?status=active | reward accrual stops |
//	| market.close_time  | /markets/{ticker}                 | trading stops        |
//
// "`close_time` may fall before or after `end_date`. Neither is assumed
// static." Until this file existed the harness had no read for the second row
// at all: `close_time` came from a hand-written config field and
// `quote.MarketInput.TradingClosed` was hard-coded false, which left H-CLOSE-4
// — markets that settle BEFORE `close_time` — with nothing that could ever
// produce it.
//
// H-CLOSE-0 is why this is a per-market poll and not a start-up read: at
// `schedule_poll_s = 300` with `final_lead = 60s`, HR-017 walked through a
// `close_time` moving from 17:00 to 12:03 at 12:00:01 and next observed at
// 12:05 — after the close, with neither the close lead nor the final cancel
// ever running. `cfg.Params.SchedulePoll` is 30s for that reason, and
// `cfg.Validate` already refuses a poll interval that is not comfortably
// shorter than the lead it triggers. This function is the thing being polled.

// MarketStatusActive is the ONLY `status` value observed on this endpoint.
//
// It is a constant rather than a literal for the same reason StatusCanceled is:
// the value that gets typed from memory is the one that is silently wrong, and
// here a mistyped comparison does not error, it reclassifies a live market.
const MarketStatusActive = "active"

// MarketStatusFinalized is the settled terminal status, MEASURED 2026-08-12 by
// `go/cmd/conform` over 219 real markets sampled across several months of
// history. The vocabulary was exactly two shapes and there was no third:
//
//	active / no result yet     68
//	finalized / has result    151
//
// Adding it is what stops a settling market raising SEV2 MARKET_STATUS_UNKNOWN
// on every schedule read for the rest of the run -- the canary ticker is dated
// for the run day, so a 4-6h session can watch its own market finalize.
//
// `finalized` WITHOUT a result is deliberately NOT accepted here. It was not
// observed once in 219 markets, so it stays with the refusing default where an
// unmeasured shape belongs: a settled market that has not said what it settled
// to is exactly the surprise that rule exists for.
const MarketStatusFinalized = "finalized"

// ScheduleOutcome is whether a schedule read landed.
type ScheduleOutcome uint8

const (
	// ScheduleUnset is the zero value, and it is NOT "observed".
	//
	// Same argument as WalkUnset, and it is load-bearing here for a second
	// reason: every field of an unpopulated ScheduleResult reads as a
	// plausible fact. CloseTime is the zero instant, HasClose is false,
	// TradingClosed is false, CanCloseEarly is false — a schedule for a market
	// that trades forever and cannot close early. Silence has to mean "we did
	// not read it", so the zero value is the outcome that licenses nothing.
	ScheduleUnset ScheduleOutcome = iota
	// ScheduleObserved means the exchange answered and the answer was
	// accounted for field by field. Only this outcome may update a market's
	// schedule.
	ScheduleObserved
	// ScheduleFailed means the read did not land: transport error, non-200,
	// undecodable body, a payload whose shape no longer matches the measured
	// contract, or a status this harness cannot interpret. The caller keeps its
	// prior schedule and starts the staleness clock.
	ScheduleFailed
)

func (o ScheduleOutcome) String() string {
	switch o {
	case ScheduleObserved:
		return "observed"
	case ScheduleFailed:
		return "failed"
	}
	return "unset"
}

// ScheduleRead is the outcome channel a ScheduleResult embeds.
//
// It is embedded for the reason OrdersResult embeds Walk: a caller cannot reach
// CloseTime without also holding an Observed() to check. A `(Schedule, error)`
// signature has the same information in it and none of that property — the
// value survives `if err != nil { log(err) }` and goes on to answer questions,
// and the answer it gives is "close_time is the zero instant, this market
// cannot close early, and trading is open".
type ScheduleRead struct {
	Outcome ScheduleOutcome
	// Anomalies are what the operator is told (§13). A schedule read produces
	// them on paths that still LAND — an unfamiliar status carried by a
	// settlement result, a close_time we could not parse — because those are
	// exactly the cases where the harness keeps running on a degraded input.
	Anomalies []risk.Anomaly
	// Err is the underlying cause when Outcome is ScheduleFailed.
	Err error
}

// Observed reports whether this result may be used as a schedule.
//
// Stated as a method, and positively, because `if r.Err == nil` is the wrong
// question: it is true of the zero value.
func (r ScheduleRead) Observed() bool { return r.Outcome == ScheduleObserved }

// ScheduleResult is one market's §9 schedule as the exchange stated it.
type ScheduleResult struct {
	ScheduleRead

	// Ticker is the market this schedule is FOR, and it is the requested
	// ticker, not the returned one — the two are compared and a mismatch fails
	// the read rather than being adopted.
	Ticker string

	// Status is the raw `status` string, kept verbatim so an unfamiliar value
	// reaches the operator as itself rather than as a category.
	Status string
	// Result is the raw `result` string: empty until the market settles, and
	// the field that makes TradingClosed reachable at all. See
	// deriveTradingClosed.
	Result string

	// CloseTime is `close_time`; HasClose says whether it was actually read.
	//
	// The pair is not a convenience. quote.MarketInput.HasClose distinguishes
	// "the schedule has not been read yet" from "a close far away", and a
	// missing, empty or unparseable close_time is NEITHER of the two answers a
	// bare time.Time can give. It is not a close far away, so the market must
	// not keep quoting on the strength of it; and it is not a close in the past
	// either, which is what the zero instant would say and what would drag
	// every market into SETTLING on a parse failure. It is the absence of the
	// input the close lead is enforced from, and H-CLOSE-0 makes that the
	// caller's problem to escalate rather than this package's to invent.
	CloseTime time.Time
	HasClose  bool

	// CanCloseEarly is H-CLOSE-4: the market can settle BEFORE close_time, so
	// the close lead is not a guarantee. Selection prefers markets without it
	// but does not exclude them, "the thin books where the opportunity lives
	// (S6) frequently have it" — so this is the common case, not the exotic
	// one, and it is the reason TradingClosed has to be observed rather than
	// computed from CloseTime.
	CanCloseEarly bool

	// TradingClosed is quote.MarketInput.TradingClosed: the close OBSERVED.
	// NextMarket takes a market to `Closed` on this flag alone, and `Closed` is
	// terminal. See deriveTradingClosed for why it is never guessed.
	TradingClosed bool
}

// UntilClose is `close_time − now`, and the second return is HasClose.
//
// It exists so a caller wires quote.MarketInput without ever touching
// CloseTime: `r.CloseTime.Sub(now)` on a result that did not land is roughly
// minus two thousand years, which is a number the close-lead comparison
// accepts. `now` is a parameter because no clock read hides in this package.
func (r ScheduleResult) UntilClose(now time.Time) (time.Duration, bool) {
	if !r.Observed() || !r.HasClose {
		return 0, false
	}
	return r.CloseTime.Sub(now), true
}

// Schedule reads GET /markets/{ticker}.
//
// It is a single object, not a list: there is no cursor and it does NOT go
// through Walk. The guarded walk exists to stop a paginated read returning page
// one as the whole answer, and there is no page one here to return.
//
// The register is Balance's — a single-object GET, non-2xx is an answer and not
// an error, and every field is either accounted for or the read fails.
func (c *Client) Schedule(ctx context.Context, ticker string) ScheduleResult {
	if err := validateTicker(ticker); err != nil {
		return scheduleFailed(ticker, err, nil)
	}
	// The ticker is interpolated into the path, which is also the string the
	// signature covers (client.go's Request: "the signature covers the path
	// WITHOUT the query string"). validateTicker is what makes that safe, and
	// it runs BEFORE the Doer is touched so a malformed ticker cannot reach the
	// wire at all.
	resp, err := c.Doer.Do(ctx, Request{Method: "GET", Path: "/markets/" + ticker})
	if err != nil {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: %w", ticker, err), nil)
	}
	if resp.Status != 200 {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: HTTP %d: %s",
			ticker, resp.Status, snippet(resp.Body)), nil)
	}
	return decodeSchedule(ticker, resp.Body)
}

// validateTicker refuses a ticker that would change which endpoint we call.
//
// The path is concatenated into the URL and into the signature. A `/` re-targets
// the request — `/markets/` with an empty or slash-bearing ticker is the LIST
// endpoint, which answers HTTP 200 with a different shape entirely — and a `?`
// or `#` opens a query string, which the signature does not cover, so the
// exchange answers 401 and the failure reads as a credential problem rather
// than as a bad ticker.
//
// The alphabet is the one the observed tickers use: `KXSILVER15M-26AUG071800-00`
// and `KXUST10AD-26AUG05-T4.65`, so letters, digits, `-`, `.` and `_`. A ticker
// outside it is refused rather than escaped, because escaping would change the
// signed path and there is no measured escaping contract to escape it by.
//
// `.` has to be allowed for the dotted strike, and that drags in the dot
// segments: `/markets/..` and `/markets/.` both normalise to `/markets` — the
// LIST endpoint again — under RFC 3986's remove_dot_segments, which any proxy
// between us and the exchange is entitled to apply even if net/http does not.
// A real ticker never begins with a dot and never carries two in a row, so
// requiring an alphanumeric first character and forbidding `..` costs nothing
// and removes the whole family.
func validateTicker(ticker string) error {
	if ticker == "" {
		return fmt.Errorf("empty market ticker: GET /markets/ is the list " +
			"endpoint and answers 200 with a different payload, so this would " +
			"read as a malformed market rather than as a missing argument")
	}
	if c := ticker[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9') {
		return fmt.Errorf("market ticker %q does not begin with a letter or "+
			"digit; a leading dot segment normalises the path back to the list "+
			"endpoint", ticker)
	}
	if strings.Contains(ticker, "..") {
		return fmt.Errorf("market ticker %q contains a `..` path segment",
			ticker)
	}
	for _, r := range ticker {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_':
		default:
			return fmt.Errorf("market ticker %q contains %q, which is outside "+
				"the observed alphabet; it is interpolated into the signed path "+
				"and a separator there changes which endpoint is called",
				ticker, string(r))
		}
	}
	return nil
}

// scheduleFailed is the one shape every non-landing path returns. Nothing but
// the outcome, the anomalies and the cause: a failed read has no schedule in
// it, and attaching a partial one is how a caller ends up reading half of it.
func scheduleFailed(ticker string, err error, anoms []risk.Anomaly) ScheduleResult {
	return ScheduleResult{
		ScheduleRead: ScheduleRead{
			Outcome:   ScheduleFailed,
			Anomalies: anoms,
			Err:       err,
		},
		Ticker: ticker,
	}
}

// decodeSchedule reads the payload against the measured shape.
func decodeSchedule(ticker string, body []byte) ScheduleResult {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: undecodable body: %w",
			ticker, err), nil)
	}
	// The market is wrapped: the observed response is `{"market": {...}}` with
	// 43 keys inside. An absent or null envelope is a malformed response and
	// not an empty market, by the same argument decodePage makes about an
	// absent item array: `200 {}` from a proxy would otherwise decode into a
	// market that never closes.
	raw, ok := env["market"]
	if !ok {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: no `market` "+
			"object in the response; every observed response wraps the market, "+
			"so its absence is a malformed body and not an empty schedule",
			ticker), nil)
	}
	if isJSONNull(raw) {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: `market` is null",
			ticker), nil)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: `market` is not "+
			"an object: %w", ticker, err), nil)
	}

	// --- identity ------------------------------------------------------
	//
	// Cheap, and it guards the one failure that would otherwise be invisible:
	// a cached or misrouted response enforcing another market's close on this
	// one. H-CLOSE-3 cancels everything in a market at close_time − final_lead,
	// so the wrong close_time does not merely miss a deadline, it fires one.
	if got := scalar(rec["ticker"]); got != ticker {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: the response "+
			"describes %q; a schedule adopted for the wrong market would run "+
			"H-CLOSE-2 and H-CLOSE-3 against the wrong deadline", ticker, got),
			[]risk.Anomaly{{
				Class: "SCHEDULE_IDENTITY", Sev: risk.SEV1, Ticker: ticker,
				Text: fmt.Sprintf("GET /markets/%s returned a market with "+
					"ticker %q; schedule read refused", ticker, got),
			}})
	}

	out := ScheduleResult{
		Ticker: ticker,
		Status: scalar(rec["status"]),
		Result: scalar(rec["result"]),
	}

	// --- can_close_early -------------------------------------------------
	//
	// Required, and decoded through a POINTER for the reason is_taker is: a
	// JSON null unmarshals into a bool as false with no error. False is the
	// assertion that close_time IS a guarantee, which is precisely what
	// H-CLOSE-4 says it is not, so it must never be arrived at by default.
	v, ok := rec["can_close_early"]
	if !ok {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: no "+
			"can_close_early field; H-CLOSE-4 cannot be evaluated without it, "+
			"and its absence defaulting to false would assert that close_time "+
			"is a guarantee", ticker), nil)
	}
	var canEarly *bool
	if err := json.Unmarshal(v, &canEarly); err != nil {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: can_close_early "+
			"is not a bool: %w", ticker, err), nil)
	}
	if canEarly == nil {
		return scheduleFailed(ticker, fmt.Errorf("schedule %s: can_close_early "+
			"is null; null is not false, and false is the claim that this "+
			"market cannot settle before close_time", ticker), nil)
	}
	out.CanCloseEarly = *canEarly

	// --- trading closed ---------------------------------------------------
	closed, anoms, err := deriveTradingClosed(ticker, out.Status, out.Result)
	if err != nil {
		return scheduleFailed(ticker, err, anoms)
	}
	out.TradingClosed = closed
	out.Anomalies = append(out.Anomalies, anoms...)

	// --- close_time -------------------------------------------------------
	closeTime, hasClose, closeAnoms := readCloseTime(ticker, rec["close_time"])
	out.CloseTime, out.HasClose = closeTime, hasClose
	out.Anomalies = append(out.Anomalies, closeAnoms...)

	out.Outcome = ScheduleObserved
	return out
}

// deriveTradingClosed produces quote.MarketInput.TradingClosed — the close
// OBSERVED, not the close computed — from `status` and `result`.
//
// **It refuses to guess, and the refusal is the design.** `active` is the only
// status value ever observed on this endpoint. A rule that maps every OTHER
// string onto a boolean is choosing between two failures with no evidence, and
// both are expensive:
//
//   - Unrecognised ⇒ closed. NextMarket returns `Closed` on this flag alone and
//     `Closed` is terminal — "nothing reopens a settled market". So one
//     unfamiliar string, from a new lifecycle state or a rewritten proxy
//     response, abandons a LIVE market for the rest of the run while we hold q,
//     with the exit cancelled and no state left that can place another. That is
//     `probebot.py`'s defect reached by a spelling.
//   - Unrecognised ⇒ still trading. The adding quote rests into a market that
//     has stopped trading and H-CLOSE-4's early close — the exact case the
//     close-lead arithmetic cannot cover, and the reason this flag exists —
//     goes undetected for as long as the vocabulary is unfamiliar.
//
// So neither branch is taken: an unrecognised status FAILS THE READ. That is
// the answer Walk already gives to a page it cannot account for, and it is the
// only one that is honest — the caller keeps its previous schedule, marks it
// stale, and the operator gets a SEV1 naming the string verbatim. Adding a
// measured constant afterwards is a one-line change; recovering a position
// abandoned in a terminal state is not.
//
// `result` is the exception that makes `true` reachable at all, and it is an
// observation rather than a vocabulary: a settlement result is only published
// once trading has stopped, so a non-empty `result` is the close observed
// whatever `status` happens to be called this month.
//
// The two together must agree. `status == "active"` alongside a non-empty
// `result` is refused for the reason readPrice refuses a yes/no pair that does
// not sum to $1.00: the fields contradict each other, and picking a winner
// would present a guess as an observation.
func deriveTradingClosed(ticker, status, result string) (bool, []risk.Anomaly, error) {
	// The refusals carry the same text to the operator as to the log, because
	// the two audiences want the same thing here: the string verbatim, and the
	// reason no boolean was invented from it.
	refuse := func(err error) (bool, []risk.Anomaly, error) {
		return false, []risk.Anomaly{{
			Class: "MARKET_STATUS_UNKNOWN", Sev: risk.SEV1,
			Ticker: ticker, Text: err.Error(),
		}}, err
	}
	switch {
	case status == "":
		return refuse(fmt.Errorf("schedule %s: no status field; TradingClosed "+
			"cannot be observed and this harness does not assume it", ticker))

	case status == MarketStatusActive && result != "":
		return refuse(fmt.Errorf("schedule %s: status is %q and result is %q at "+
			"the same time; a settlement result is published only after trading "+
			"stops, so these contradict and neither may be treated as the "+
			"observed close", ticker, status, result))

	case status == MarketStatusActive:
		return false, nil, nil

	case status == MarketStatusFinalized && result != "":
		// Measured, so it lands silently. This is the ordinary end of every
		// market's life: 151 of the 219 markets sampled were in exactly this
		// state, and before this case existed each one raised a SEV2 on every
		// single schedule read.
		return true, nil, nil

	case result != "":
		// Unfamiliar status, but the settlement result is itself the
		// observation, so the read lands. SEV2 rather than SEV1: the risk is
		// managed — the market goes to Closed, which is where a settled market
		// belongs — but we are now running on a vocabulary we have not
		// measured, and the next unfamiliar status may not carry a result.
		return true, []risk.Anomaly{{
			Class: "MARKET_STATUS_UNKNOWN", Sev: risk.SEV2, Ticker: ticker,
			Text: fmt.Sprintf("%s: unmeasured market status %q, read as CLOSED "+
				"only because result %q is present; %q is the sole measured "+
				"status and this one should be added once confirmed",
				ticker, status, result, MarketStatusActive),
		}}, nil

	default:
		return refuse(fmt.Errorf("schedule %s: status %q is not %q and carries "+
			"no settlement result; reading it as closed would send a possibly "+
			"live market to the terminal Closed state, and reading it as open "+
			"would hide an H-CLOSE-4 early close, so the read is refused "+
			"instead", ticker, status, MarketStatusActive))
	}
}

// readCloseTime reads `close_time` into the (CloseTime, HasClose) pair.
//
// It does NOT fail the read, and the asymmetry with deriveTradingClosed is
// deliberate. quote.MarketInput HAS a representation for "the close is not
// known" — HasClose false, which enforces no lead and lets NextMarket fall
// through to the ordinary rules — so the honest answer is expressible and is
// given. It has no representation for "the status is not known": TradingClosed
// false is indistinguishable from an observed open market, and true is
// terminal. The field that can carry its own uncertainty carries it; the one
// that cannot fails the read.
//
// Landing also keeps the more urgent half of the answer: a market that has
// closed early is exactly the one whose close_time we may no longer be able to
// read, and failing here would discard the TradingClosed we just observed.
//
// Every path that yields HasClose false raises SEV1. A market whose close_time
// is unknown cannot have H-CLOSE-2's lead or H-CLOSE-3's final cancel enforced
// at all, which is unmanaged risk under §13.2's definition, and H-CLOSE-0 asks
// for it to be escalated rather than assumed away.
func readCloseTime(ticker string, raw json.RawMessage) (time.Time, bool, []risk.Anomaly) {
	noClose := func(text string) []risk.Anomaly {
		return []risk.Anomaly{{
			Class: "MARKET_CLOSE_UNKNOWN", Sev: risk.SEV1, Ticker: ticker, Text: text,
		}}
	}
	s := scalar(raw)
	if s == "" {
		return time.Time{}, false, noClose(fmt.Sprintf("%s: close_time is "+
			"absent, null or empty; no close lead and no final cancel can be "+
			"enforced for this market until it is read", ticker))
	}
	t, err := parseWireTime(s)
	if err != nil {
		return time.Time{}, false, noClose(fmt.Sprintf("%s: close_time %q is "+
			"unusable: %v; no close lead and no final cancel can be enforced "+
			"for this market until it is read", ticker, s, err))
	}
	return t, true, nil
}

// parseWireTime parses an exchange timestamp as a strict RFC 3339 instant.
//
// The layout is RFC3339 and not a hand-written one WITHOUT the offset, because
// `2026-08-07T22:00:00` parsed by a layout that has no zone is interpreted in
// the host's local zone: the same string becomes a different instant on a
// machine set to Europe/London, and `close_time − close_lead` then fires an
// hour early or an hour late. Every observed value carries an explicit `Z`, and
// time.Parse with the RFC3339 layout rejects one that does not, so the
// strictness is free.
//
// The zero instant is rejected rather than returned. It parses cleanly from
// `0001-01-01T00:00:00Z`, and returning it would hand the caller a HasClose of
// true over a CloseTime that is two thousand years in the past — a zero
// presented as a fact, which is the one thing the pair exists to prevent.
//
// The result is normalised to UTC. Only the instant is ever compared, so the
// zone is presentation, and a fixed-offset zone in a logged deadline is a
// second thing for a human to get wrong at four in the morning.
func parseWireTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 timestamp with an "+
			"explicit offset: %w", s, err)
	}
	if t.IsZero() {
		return time.Time{}, fmt.Errorf("%q is the zero instant, which is a "+
			"placeholder and not a deadline", s)
	}
	return t.UTC(), nil
}

// confidence: high

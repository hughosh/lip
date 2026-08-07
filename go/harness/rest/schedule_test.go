package rest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lip/harness/risk"
)

// The ticker, and the payload, of the response this endpoint was verified
// against live: KXSILVER15M-26AUG071800-00, an `active` market with
// `can_close_early: true`, a 15-minute trading window and a 5-minute settlement
// timer. Every fixture below is a mutation of it.
const schedTicker = "KXSILVER15M-26AUG071800-00"

// omitField deletes a key from a fixture. It is a distinct sentinel from `nil`
// because null and absent are different wire states and this package treats
// them differently everywhere else.
type omitField struct{}

// marketBody builds a `{"market": {...}}` response carrying the observed field
// set, with `over` applied on top.
//
// It is a synthetic fixture rather than a captured one on purpose: the captured
// responses live in `settle.db`, which a live collector is writing, and a test
// that opened it would be reading a moving file with a lock on it. The field
// list is transcribed from the verified response instead.
func marketBody(t *testing.T, over map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"ticker":                   schedTicker,
		"event_ticker":             "KXSILVER15M-26AUG071800",
		"status":                   MarketStatusActive,
		"open_time":                "2026-08-07T21:45:00Z",
		"close_time":               "2026-08-07T22:00:00Z",
		"expected_expiration_time": "2026-08-07T22:05:00Z",
		"expiration_time":          "2026-08-14T22:00:00Z",
		"latest_expiration_time":   "2026-08-14T22:00:00Z",
		"can_close_early":          true,
		"settlement_timer_seconds": 300,
		"result":                   "",
	}
	for k, v := range over {
		if _, drop := v.(omitField); drop {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	raw, err := json.Marshal(map[string]any{"market": m})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return raw
}

// schedule200 answers every call with one body.
func schedule200(t *testing.T, body []byte) *scriptedDoer {
	t.Helper()
	return &scriptedDoer{t: t, handle: func(_ int, _ Request) (Response, error) {
		return Response{Status: 200, Body: body}, nil
	}}
}

// §9's source row, read: `market.close_time` from `/markets/{ticker}`.
func TestScheduleReadsTheObservedPayload(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n != 0 {
			t.Fatalf("a single-object read must not paginate; call %d: %+v", n, req)
		}
		if req.Method != "GET" || req.Path != "/markets/"+schedTicker {
			t.Fatalf("want GET /markets/%s, got %s %s", schedTicker, req.Method, req.Path)
		}
		if len(req.Query) != 0 {
			t.Fatalf("this endpoint takes no query; got %v", req.Query)
		}
		if req.Body != nil {
			t.Fatalf("a read must not carry a body; got %q", req.Body)
		}
		return Response{Status: 200, Body: marketBody(t, nil)}, nil
	}}

	r := NewClient(d).Schedule(context.Background(), schedTicker)
	if !r.Observed() {
		t.Fatalf("a well-formed response must land: %v", r.Err)
	}
	if r.Ticker != schedTicker {
		t.Errorf("Ticker = %q, want %q", r.Ticker, schedTicker)
	}
	if r.Status != MarketStatusActive {
		t.Errorf("Status = %q, want %q", r.Status, MarketStatusActive)
	}
	if !r.HasClose {
		t.Fatal("close_time was present and parseable; HasClose must be true")
	}
	want := time.Date(2026, 8, 7, 22, 0, 0, 0, time.UTC)
	if !r.CloseTime.Equal(want) {
		t.Errorf("CloseTime = %s, want %s", r.CloseTime, want)
	}
	if !r.CanCloseEarly {
		t.Error("can_close_early was true; H-CLOSE-4 depends on it surviving")
	}
	if r.TradingClosed {
		t.Error("an active market with no result is trading")
	}
	if len(r.Anomalies) != 0 {
		t.Errorf("a clean read must be silent, got %+v", r.Anomalies)
	}
	// The wiring the caller actually needs, computed here so it never touches
	// CloseTime directly.
	now := time.Date(2026, 8, 7, 21, 50, 0, 0, time.UTC)
	until, ok := r.UntilClose(now)
	if !ok || until != 10*time.Minute {
		t.Fatalf("UntilClose = (%s, %v), want (10m0s, true)", until, ok)
	}
}

// The WalkUnset argument, restated for this type: the zero value is the one a
// forgotten assignment or a closed channel produces, and every field of it
// reads as a market that trades forever and cannot close early.
func TestScheduleZeroValueIsNotAnObservation(t *testing.T) {
	var r ScheduleResult
	if r.Observed() {
		t.Fatal("the zero ScheduleResult must not report an observation")
	}
	if until, ok := r.UntilClose(time.Now()); ok {
		t.Fatalf("the zero value has no close; UntilClose gave (%s, true)", until)
	}
	if r.Outcome.String() != "unset" {
		t.Errorf("Outcome.String() = %q", r.Outcome.String())
	}
}

// A missing, empty or unparseable close_time is not a close far away and not a
// close in the past. HasClose is the only honest answer, and the zero CloseTime
// must stay unreachable as a fact.
func TestScheduleUnreadableCloseTimeIsNeverPresentedAsAFact(t *testing.T) {
	for name, v := range map[string]any{
		"absent":            omitField{},
		"null":              nil,
		"empty":             "",
		"garbage":           "not a timestamp",
		"date only":         "2026-08-07",
		"no offset":         "2026-08-07T22:00:00",
		"local-looking":     "2026-08-07 22:00:00",
		"unix seconds":      "1786000000",
		"the zero instant":  "0001-01-01T00:00:00Z",
		"epoch placeholder": "1970-01-01T00:00:00",
	} {
		t.Run(name, func(t *testing.T) {
			d := schedule200(t, marketBody(t, map[string]any{"close_time": v}))
			r := NewClient(d).Schedule(context.Background(), schedTicker)
			if !r.Observed() {
				t.Fatalf("the status was still readable, so the read lands: %v", r.Err)
			}
			if r.HasClose {
				t.Fatalf("close_time %#v must not yield HasClose (CloseTime = %s)",
					v, r.CloseTime)
			}
			if !r.CloseTime.IsZero() {
				t.Fatalf("an unread close_time must leave CloseTime zero, got %s",
					r.CloseTime)
			}
			if until, ok := r.UntilClose(time.Date(2026, 8, 7, 21, 50, 0, 0, time.UTC)); ok {
				t.Fatalf("UntilClose must refuse to answer, gave %s", until)
			}
			if len(r.Anomalies) != 1 ||
				r.Anomalies[0].Class != "MARKET_CLOSE_UNKNOWN" ||
				r.Anomalies[0].Sev != risk.SEV1 {
				t.Fatalf("want one SEV1 MARKET_CLOSE_UNKNOWN, got %+v", r.Anomalies)
			}
			if r.Anomalies[0].Ticker != schedTicker {
				t.Errorf("the anomaly must name the market, got %q", r.Anomalies[0].Ticker)
			}
		})
	}
}

// `1970-01-01T00:00:00Z` is a real instant and is NOT the zero instant, so it
// parses. This pins that the zero-instant guard is exactly that and has not
// grown into a range check that would reject a legitimate old timestamp.
func TestScheduleEpochCloseTimeIsAnInstantNotAPlaceholder(t *testing.T) {
	d := schedule200(t, marketBody(t, map[string]any{
		"close_time": "1970-01-01T00:00:00Z",
	}))
	r := NewClient(d).Schedule(context.Background(), schedTicker)
	if !r.Observed() || !r.HasClose {
		t.Fatalf("an explicit epoch timestamp is readable: observed=%v hasClose=%v err=%v",
			r.Observed(), r.HasClose, r.Err)
	}
	if !r.CloseTime.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("CloseTime = %s", r.CloseTime)
	}
}

// H-CLOSE-4. `false` is the claim that close_time is a guarantee, so it is only
// ever produced by the exchange saying so.
func TestScheduleCanCloseEarlyRoundTrips(t *testing.T) {
	for _, want := range []bool{true, false} {
		d := schedule200(t, marketBody(t, map[string]any{"can_close_early": want}))
		r := NewClient(d).Schedule(context.Background(), schedTicker)
		if !r.Observed() {
			t.Fatalf("can_close_early=%v: %v", want, r.Err)
		}
		if r.CanCloseEarly != want {
			t.Errorf("CanCloseEarly = %v, want %v", r.CanCloseEarly, want)
		}
	}
	for name, v := range map[string]any{"absent": omitField{}, "null": nil,
		"a string": "true", "a number": 1} {
		t.Run(name, func(t *testing.T) {
			d := schedule200(t, marketBody(t, map[string]any{"can_close_early": v}))
			r := NewClient(d).Schedule(context.Background(), schedTicker)
			if r.Observed() {
				t.Fatalf("can_close_early %#v must not default to false: %+v", v, r)
			}
			if r.Err == nil {
				t.Fatal("a failed read must carry its cause")
			}
		})
	}
}

// The status derivation, in both directions and including the case it refuses
// to answer. `active` is the only measured value; an unrecognised one is not
// silently mapped onto either boolean, because one direction abandons a live
// market in the terminal Closed state and the other hides an early close.
func TestScheduleStatusDerivation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   any
		result   any
		observed bool
		closed   bool
		anomaly  *risk.Severity
	}{
		{name: "active and unsettled is trading",
			status: MarketStatusActive, result: "", observed: true, closed: false},
		{name: "a settlement result is the close observed",
			status: "settled", result: "yes", observed: true, closed: true,
			anomaly: sev(risk.SEV2)},
		{name: "an unmeasured status with no result is refused",
			status: "closed", result: "", observed: false, anomaly: sev(risk.SEV1)},
		{name: "an unmeasured status is refused even when it looks harmless",
			status: "initialized", result: "", observed: false, anomaly: sev(risk.SEV1)},
		{name: "case is not normalised away",
			status: "ACTIVE", result: "", observed: false, anomaly: sev(risk.SEV1)},
		{name: "an absent status is refused, not assumed open",
			status: omitField{}, result: "", observed: false, anomaly: sev(risk.SEV1)},
		{name: "a null status is refused",
			status: nil, result: "", observed: false, anomaly: sev(risk.SEV1)},
		{name: "an empty status is refused",
			status: "", result: "", observed: false, anomaly: sev(risk.SEV1)},
		{name: "active with a settlement result is a contradiction",
			status: MarketStatusActive, result: "no", observed: false,
			anomaly: sev(risk.SEV1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := schedule200(t, marketBody(t, map[string]any{
				"status": tc.status, "result": tc.result,
			}))
			r := NewClient(d).Schedule(context.Background(), schedTicker)
			if r.Observed() != tc.observed {
				t.Fatalf("Observed() = %v, want %v (err %v)", r.Observed(),
					tc.observed, r.Err)
			}
			if r.TradingClosed != tc.closed {
				t.Fatalf("TradingClosed = %v, want %v", r.TradingClosed, tc.closed)
			}
			if !tc.observed {
				if r.Err == nil {
					t.Fatal("a refused read must carry its cause")
				}
				// A refused read carries no schedule at all: HasClose must not
				// be true over a CloseTime nobody is entitled to read.
				if r.HasClose || !r.CloseTime.IsZero() {
					t.Fatalf("a failed read must carry no schedule, got %+v", r)
				}
			}
			if tc.anomaly == nil {
				if len(r.Anomalies) != 0 {
					t.Fatalf("want silence, got %+v", r.Anomalies)
				}
				return
			}
			if len(r.Anomalies) != 1 || r.Anomalies[0].Class != "MARKET_STATUS_UNKNOWN" {
				t.Fatalf("want one MARKET_STATUS_UNKNOWN, got %+v", r.Anomalies)
			}
			if r.Anomalies[0].Sev != *tc.anomaly {
				t.Fatalf("Sev = %s, want %s", r.Anomalies[0].Sev, *tc.anomaly)
			}
			// The operator has to be able to add the constant, so the string
			// itself has to survive into the message.
			if s, ok := tc.status.(string); ok && s != "" &&
				!strings.Contains(r.Anomalies[0].Text, s) {
				t.Fatalf("the anomaly must name %q verbatim: %q", s,
					r.Anomalies[0].Text)
			}
		})
	}
}

func sev(s risk.Severity) *risk.Severity { return &s }

// A dropped response is not an open market with no close.
func TestScheduleTransportErrorIsNotAZeroSchedule(t *testing.T) {
	boom := errors.New("no answer received")
	d := &scriptedDoer{t: t, handle: func(_ int, _ Request) (Response, error) {
		return Response{}, boom
	}}
	r := NewClient(d).Schedule(context.Background(), schedTicker)
	if r.Observed() {
		t.Fatal("a transport error must not land")
	}
	if !errors.Is(r.Err, boom) {
		t.Fatalf("the cause must survive, got %v", r.Err)
	}
	if r.HasClose || !r.CloseTime.IsZero() || r.CanCloseEarly || r.TradingClosed {
		t.Fatalf("a failed read must carry no schedule, got %+v", r)
	}
	if until, ok := r.UntilClose(time.Now()); ok {
		t.Fatalf("UntilClose answered %s on a failed read", until)
	}
}

// A non-2xx is an answer, and it is reported as one. The body is snippeted into
// the error so a 404 on a retired ticker is distinguishable from a 500.
func TestScheduleNon2xxIsReportedRatherThanSilent(t *testing.T) {
	for _, status := range []int{400, 401, 404, 429, 500, 503, 301} {
		d := &scriptedDoer{t: t, handle: func(_ int, _ Request) (Response, error) {
			return Response{Status: status, Body: []byte(`{"error":"nope"}`)}, nil
		}}
		r := NewClient(d).Schedule(context.Background(), schedTicker)
		if r.Observed() {
			t.Fatalf("HTTP %d must not land", status)
		}
		if r.Err == nil || !strings.Contains(r.Err.Error(), "nope") {
			t.Fatalf("HTTP %d: want the body in the error, got %v", status, r.Err)
		}
		if r.HasClose {
			t.Fatalf("HTTP %d produced a close time", status)
		}
	}
}

// The `200 {}` family: a proxy reply, an error page with a 200, a changed
// schema. None of them is an empty schedule.
func TestScheduleMalformedBodiesDoNotDecodeIntoASchedule(t *testing.T) {
	for name, body := range map[string]string{
		"not json":         `<html>gateway</html>`,
		"empty object":     `{}`,
		"null market":      `{"market":null}`,
		"market is a list": `{"market":[]}`,
		"market is scalar": `{"market":"active"}`,
		"bare market":      `{"ticker":"` + schedTicker + `","status":"active"}`,
		"truncated":        `{"market":{"ticker":"`,
	} {
		t.Run(name, func(t *testing.T) {
			d := schedule200(t, []byte(body))
			r := NewClient(d).Schedule(context.Background(), schedTicker)
			if r.Observed() {
				t.Fatalf("%s must not land, got %+v", name, r)
			}
			if r.Err == nil {
				t.Fatal("a failed read must carry its cause")
			}
		})
	}
}

// A cached or misrouted response would otherwise run H-CLOSE-3's final cancel
// against another market's deadline.
func TestScheduleRefusesAResponseForAnotherMarket(t *testing.T) {
	d := schedule200(t, marketBody(t, map[string]any{"ticker": "KXOTHER-26AUG07-00"}))
	r := NewClient(d).Schedule(context.Background(), schedTicker)
	if r.Observed() {
		t.Fatal("a schedule for another market must not be adopted")
	}
	if len(r.Anomalies) != 1 || r.Anomalies[0].Class != "SCHEDULE_IDENTITY" ||
		r.Anomalies[0].Sev != risk.SEV1 {
		t.Fatalf("want one SEV1 SCHEDULE_IDENTITY, got %+v", r.Anomalies)
	}
	if r.Ticker != schedTicker {
		t.Errorf("the result must name the market we asked about, got %q", r.Ticker)
	}
}

// The ticker is interpolated into the path that is also signed, so a separator
// in it changes which endpoint is called — and `/markets/` is the LIST endpoint,
// which answers 200. None of these may reach the wire.
func TestScheduleRefusesAPathBearingTickerWithoutSending(t *testing.T) {
	for _, ticker := range []string{
		"", ".", "/", "..", "../portfolio/balance", "KX..OTHER", "-KXOTHER",
		"KX/OTHER", "KX?limit=1", "KX#frag", "KX OTHER", "KX%2FOTHER",
		"KX&status=active", "KX\n",
	} {
		d := &scriptedDoer{t: t, handle: func(_ int, req Request) (Response, error) {
			t.Fatalf("ticker %q reached the wire as %q", ticker, req.Path)
			return Response{}, nil
		}}
		r := NewClient(d).Schedule(context.Background(), ticker)
		if r.Observed() || r.Err == nil {
			t.Fatalf("ticker %q must be refused, got %+v", ticker, r)
		}
		if len(d.Calls()) != 0 {
			t.Fatalf("ticker %q was sent: %+v", ticker, d.Calls())
		}
	}
	// And the observed alphabet still passes, including the dotted-strike form
	// `KXUST10AD-26AUG05-T4.65` from §10.1's audited example.
	for _, ticker := range []string{schedTicker, "KXUST10AD-26AUG05-T4.65", "A_1"} {
		if err := validateTicker(ticker); err != nil {
			t.Fatalf("%q is a real ticker shape: %v", ticker, err)
		}
	}
}

// A layout without an offset reads the same string as a different instant on a
// host in a different zone, which moves close_time — and every lead computed
// from it — by the size of that offset.
func TestParseWireTimeRequiresAnExplicitOffset(t *testing.T) {
	want := time.Date(2026, 8, 7, 22, 0, 0, 0, time.UTC)
	for _, s := range []string{
		"2026-08-07T22:00:00Z",
		"2026-08-07T22:00:00.000Z",
		"2026-08-07T23:00:00+01:00",
		"2026-08-07T18:00:00-04:00",
	} {
		got, err := parseWireTime(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if !got.Equal(want) {
			t.Errorf("%q -> %s, want the same instant as %s", s, got, want)
		}
		// Normalised, so a logged deadline cannot be read in a zone nobody
		// configured.
		if got.Location() != time.UTC {
			t.Errorf("%q kept location %s", s, got.Location())
		}
	}
	for _, s := range []string{
		"2026-08-07T22:00:00",
		"2026-08-07 22:00:00Z",
		"2026-08-07",
		"07 Aug 2026 22:00:00 GMT",
		"1786000000",
		"",
		"0001-01-01T00:00:00Z",
	} {
		if got, err := parseWireTime(s); err == nil {
			t.Errorf("%q parsed as %s; it is not an unambiguous instant", s, got)
		}
	}
}

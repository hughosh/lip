package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"lip/harness/risk"
)

// M16 — "read only page one of every list endpoint", with the target on page
// two. This is `probebot.py:1100`'s incident as a unit test.
func TestWalkReachesPageTwo(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		switch n {
		case 0:
			if got := req.Query.Get("cursor"); got != "" {
				t.Fatalf("page 1 must be requested with no cursor, got %q", got)
			}
			return jsonPage(EpOrders, "CUR2", map[string][]any{
				"orders": {order("o1", "lipH-run1-000-yes-00000001", "T1", "yes", 0.58, "1.00")},
			}), nil
		case 1:
			if got := req.Query.Get("cursor"); got != "CUR2" {
				t.Fatalf("page 2 cursor must be sent exactly as received, got %q", got)
			}
			return jsonPage(EpOrders, "", map[string][]any{
				"orders": {order("o2", "lipH-run1-000-no-00000002", "T1", "no", 0.41, "1.00")},
			}), nil
		}
		t.Fatalf("unexpected call %d", n)
		return Response{}, nil
	}}

	w := NewClient(d).Walk(context.Background(), EpOrders, nil)
	if !w.Replaces() {
		t.Fatalf("walk must complete, got %s (%v)", w.Outcome, w.Err)
	}
	if w.Pages != 2 {
		t.Fatalf("want 2 pages, got %d", w.Pages)
	}
	recs := w.Records("orders")
	if len(recs) != 2 {
		t.Fatalf("want 2 orders across both pages, got %d", len(recs))
	}
	// The page-two record is the one a single-page read loses. Name it.
	if !strings.Contains(string(recs[1]), `"o2"`) {
		t.Fatalf("page two's order is missing from the walk: %s", recs[1])
	}
}

// M23 — "carry a corrupted cursor instead of abandoning the walk". The endpoint
// answers HTTP 200 with page one, so nothing errors; the walk has to notice by
// itself. V1.8b.
func TestWalkDetectsRewindAndDiscardsRecords(t *testing.T) {
	page1 := func(cursor string) Response {
		return jsonPage(EpFills, cursor, map[string][]any{
			"fills": {fill("f1", "t1", "o1", "T1", false)},
		})
	}
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		switch n {
		case 0:
			return page1("CUR2"), nil
		case 1:
			return jsonPage(EpFills, "CUR3", map[string][]any{
				"fills": {fill("f2", "t2", "o2", "T1", false)},
			}), nil
		case 2:
			// The rewind: a fresh, valid-looking cursor, HTTP 200, page one's
			// record again. Exactly what /portfolio/fills does when it cannot
			// parse the cursor it was given.
			return page1("CUR2b"), nil
		}
		t.Fatalf("walk did not terminate: it made call %d", n)
		return Response{}, nil
	}}

	w := NewClient(d).Walk(context.Background(), EpFills, nil)

	if w.Outcome != WalkRewound {
		t.Fatalf("want WalkRewound, got %s", w.Outcome)
	}
	if w.Replaces() {
		t.Fatal("a rewound walk must not replace state: H-PAGE-1 clause 2 " +
			"requires the prior state be preserved and marked stale")
	}
	// H-PAGE-1a clause 2: "Never emit the accumulated duplicates."
	if n := len(w.Records("fills")); n != 0 {
		t.Fatalf("a rewound walk emitted %d accumulated records; they contain "+
			"duplicates by construction and must be discarded", n)
	}
	if len(w.Anomalies) != 1 {
		t.Fatalf("want exactly one anomaly, got %d", len(w.Anomalies))
	}
	a := w.Anomalies[0]
	if a.Class != "CURSOR_REWIND" || a.Sev != risk.SEV2 {
		t.Fatalf("want SEV2 CURSOR_REWIND, got %s %s", a.Sev, a.Class)
	}
	// And it terminated. `scriptedDoer` fails the test on call 3, so reaching
	// here at all is the non-termination half of the assertion.
	if len(d.Calls()) != 3 {
		t.Fatalf("want 3 requests before abandoning, got %d", len(d.Calls()))
	}
}

// A rewind detected by the cursor repeating rather than by a record identity —
// the case an empty page would otherwise slip past.
func TestWalkDetectsRepeatedCursor(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n > 4 {
			t.Fatalf("walk did not terminate: call %d", n)
		}
		// Records-free pages that keep handing back the same cursor.
		return jsonPage(EpFills, "SAME", map[string][]any{"fills": {}}), nil
	}}

	w := NewClient(d).Walk(context.Background(), EpFills, nil)
	if w.Outcome != WalkRewound {
		t.Fatalf("want WalkRewound on a repeated cursor, got %s (%v)", w.Outcome, w.Err)
	}
	if w.Replaces() {
		t.Fatal("a rewound walk must not replace state")
	}
}

// H-PAGE-1a clause 3, and the reason EpPositions has two item keys: the walk
// must not stop because ONE array came back empty while the cursor is still
// non-empty. `probebot.py:1123` terminates on `not programs`, which on a
// dual-array endpoint silently drops the second array.
func TestWalkDoesNotTerminateOnOneEmptyArray(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		switch n {
		case 0:
			return jsonPage(EpPositions, "CUR2", map[string][]any{
				"market_positions": {position("T1", "3.00")},
				"event_positions":  {},
			}), nil
		case 1:
			// market_positions is exhausted; event_positions is not. A walker
			// that terminated on the first empty array never sees this page.
			return jsonPage(EpPositions, "", map[string][]any{
				"market_positions": {},
				"event_positions":  {map[string]any{"event_ticker": "E1"}},
			}), nil
		}
		t.Fatalf("unexpected call %d", n)
		return Response{}, nil
	}}

	w := NewClient(d).Walk(context.Background(), EpPositions, nil)
	if !w.Replaces() {
		t.Fatalf("want a complete walk, got %s (%v)", w.Outcome, w.Err)
	}
	if n := len(w.Records("market_positions")); n != 1 {
		t.Fatalf("want 1 market position, got %d", n)
	}
	if n := len(w.Records("event_positions")); n != 1 {
		t.Fatalf("want 1 event position from page two, got %d — the walk "+
			"terminated on market_positions going empty", n)
	}
}

// M22 — "read `next_cursor` from a `cursor` endpoint (or vice versa)". The
// production failure is silent: the wrong key reads as absent, the walker
// concludes it finished, and page one becomes the whole answer.
func TestWrongCursorFieldIsLoudNotSilent(t *testing.T) {
	// EpOrders declares `cursor`; this endpoint is the same one mis-declared
	// as reading `next_cursor`, which is what M22 does.
	mutated := EpOrders
	mutated.CursorField = "next_cursor"

	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		// The server's real answer: `cursor`, with more pages to come.
		return jsonPage(EpOrders, "CUR2", map[string][]any{
			"orders": {order("o1", "lipH-run1-000-yes-00000001", "T1", "yes", 0.58, "1.00")},
		}), nil
	}}

	w := NewClient(d).Walk(context.Background(), mutated, nil)
	if w.Outcome != WalkFailed {
		t.Fatalf("reading the wrong cursor key must fail loudly, got %s with "+
			"%d records — that is page one returned as the whole answer",
			w.Outcome, len(w.Records("orders")))
	}
	if w.Replaces() {
		t.Fatal("a misconfigured walk must not replace state")
	}
	if !strings.Contains(w.Err.Error(), "next_cursor") ||
		!strings.Contains(w.Err.Error(), "cursor") {
		t.Fatalf("the error must name both keys so the fix is obvious: %v", w.Err)
	}
}

// H-PAGE-1 clause 2: a failure at page k preserves prior state and marks it
// stale. It never produces an empty or partial replacement.
func TestWalkFailureAtPageTwoYieldsNoRecords(t *testing.T) {
	dropped := errors.New("connection reset")
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n == 0 {
			return jsonPage(EpOrders, "CUR2", map[string][]any{
				"orders": {order("o1", "c1", "T1", "yes", 0.58, "1.00")},
			}), nil
		}
		return Response{}, dropped
	}}

	w := NewClient(d).Walk(context.Background(), EpOrders, nil)
	if w.Outcome != WalkFailed {
		t.Fatalf("want WalkFailed, got %s", w.Outcome)
	}
	if w.Replaces() {
		t.Fatal("an incomplete walk must not replace state")
	}
	if n := len(w.Records("orders")); n != 0 {
		t.Fatalf("an incomplete walk returned %d records; page one is not an "+
			"answer, however long it is", n)
	}
	if !errors.Is(w.Err, dropped) {
		t.Fatalf("want the transport cause wrapped, got %v", w.Err)
	}
}

// A non-200 is an answer, but not a complete walk.
func TestWalkNon200DoesNotReplace(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{Status: 400, Body: []byte(`{"error":{"code":"bad_limit"}}`)}, nil
	}}
	w := NewClient(d).Walk(context.Background(), EpOrders, nil)
	if w.Replaces() {
		t.Fatal("an HTTP 400 must not replace state")
	}
	if !strings.Contains(w.Err.Error(), "400") {
		t.Fatalf("the error must carry the status: %v", w.Err)
	}
}

// The circuit breaker abandons; it does not truncate. H-PAGE-1a's closing line
// forbids the version that returns what it got.
func TestUnboundedWalkIsAbandonedNotTruncated(t *testing.T) {
	ep := EpOrders
	ep.MaxPages = 3
	seq := 0
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		seq++
		// Every page distinct, every cursor distinct: neither guard fires, so
		// only the breaker can stop this.
		return jsonPage(ep, "CUR"+itoa(seq+1), map[string][]any{
			"orders": {order("o"+itoa(seq), "c"+itoa(seq), "T1", "yes", 0.58, "1.00")},
		}), nil
	}}

	w := NewClient(d).Walk(context.Background(), ep, nil)
	if w.Outcome != WalkFailed {
		t.Fatalf("want WalkFailed, got %s", w.Outcome)
	}
	if n := len(w.Records("orders")); n != 0 {
		t.Fatalf("the breaker truncated the walk to %d records instead of "+
			"abandoning it; that converts an infinite loop into a silent "+
			"partial answer, which H-PAGE-1a forbids", n)
	}
	if len(w.Anomalies) != 1 || w.Anomalies[0].Class != "CURSOR_UNBOUNDED" {
		t.Fatalf("want one CURSOR_UNBOUNDED anomaly, got %+v", w.Anomalies)
	}
}

// The forward-progress guard must not degrade silently if the payload shape
// changes under it.
func TestMissingIdentityFieldFailsTheWalk(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {map[string]any{"ticker": "T1"}}, // no order_id
		}), nil
	}}
	w := NewClient(d).Walk(context.Background(), EpOrders, nil)
	if w.Outcome != WalkFailed {
		t.Fatalf("a record with no identity field leaves forward progress "+
			"unassertable and must fail the walk, got %s", w.Outcome)
	}
	if !strings.Contains(w.Err.Error(), "order_id") {
		t.Fatalf("the error must name the missing field: %v", w.Err)
	}
}

// V1.8c — the endpoint constants table itself, pinned. A new endpoint added
// without its cursor field measured fails here.
func TestEndpointConstantsArePinned(t *testing.T) {
	for _, tc := range []struct {
		ep       Endpoint
		cursor   string
		itemKeys []string
	}{
		{EpPositions, "cursor", []string{"market_positions", "event_positions"}},
		{EpOrders, "cursor", []string{"orders"}},
		{EpFills, "cursor", []string{"fills"}},
		{EpPrograms, "next_cursor", []string{"incentive_programs"}},
	} {
		if tc.ep.CursorField != tc.cursor {
			t.Errorf("%s: cursor field is %q, pagecontract §0 measured %q",
				tc.ep.Path, tc.ep.CursorField, tc.cursor)
		}
		if len(tc.ep.ItemKeys) != len(tc.itemKeys) {
			t.Errorf("%s: %d item keys, want %d", tc.ep.Path,
				len(tc.ep.ItemKeys), len(tc.itemKeys))
			continue
		}
		for i, k := range tc.itemKeys {
			if tc.ep.ItemKeys[i] != k {
				t.Errorf("%s: item key %d is %q, want %q", tc.ep.Path, i,
					tc.ep.ItemKeys[i], k)
			}
		}
		if tc.ep.MaxPages <= 0 {
			t.Errorf("%s: no circuit breaker configured", tc.ep.Path)
		}
		if err := tc.ep.ValidateLimit(tc.ep.PageLimit); err != nil {
			t.Errorf("%s: its own page limit is out of its own bounds: %v",
				tc.ep.Path, err)
		}
	}

	// The measured 1..1000 bound on the portfolio family. Outside it the
	// endpoint answers HTTP 400, so we refuse before sending.
	for _, ep := range []Endpoint{EpOrders, EpFills} {
		if err := ep.ValidateLimit(0); err == nil {
			t.Errorf("%s: limit=0 is HTTP 400 and must be refused", ep.Path)
		}
		if err := ep.ValidateLimit(1001); err == nil {
			t.Errorf("%s: limit=1001 is HTTP 400 and must be refused", ep.Path)
		}
		if err := ep.ValidateLimit(1000); err != nil {
			t.Errorf("%s: limit=1000 is valid: %v", ep.Path, err)
		}
	}

	// The fills identity is fill_id, not trade_id: V1.8 records them as two
	// distinct identities and trade_id is the join key the public trade stream
	// also carries.
	if EpFills.IDFields["fills"] != "fill_id" {
		t.Errorf("fills identity is %q, want fill_id",
			EpFills.IDFields["fills"])
	}
}

// H-PAGE-1a clause 4 — a misspelled status is not an error, it is an empty
// result, which is "absence is not evidence" in a new place.
func TestStatusStringsArePinned(t *testing.T) {
	if err := ValidateStatus("cancelled"); err == nil {
		t.Fatal("`cancelled` returns 0 rows and `canceled` returns 2; the " +
			"double-l spelling must be refused before it reaches the wire")
	}
	for _, s := range []string{StatusResting, StatusCanceled, StatusExecuted, ""} {
		if err := ValidateStatus(s); err != nil {
			t.Errorf("status %q must be accepted: %v", s, err)
		}
	}
	if StatusCanceled != "canceled" {
		t.Errorf("StatusCanceled is %q; US spelling, one l", StatusCanceled)
	}
}

// The cursor is sent under `cursor` on BOTH families even though the response
// field name differs. Getting this symmetric in either direction breaks one
// family silently.
func TestCursorRequestParamIsAlwaysCursor(t *testing.T) {
	q, err := pageQuery(EpPrograms, url.Values{"status": {"active"}}, "ABC")
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("cursor") != "ABC" {
		t.Fatalf("programs must be REQUESTED with `cursor` even though it "+
			"RESPONDS with `next_cursor`; got %v", q)
	}
	if q.Get("next_cursor") != "" {
		t.Fatalf("`next_cursor` is a response field, never a request one: %v", q)
	}
	if q.Get("status") != "active" {
		t.Fatalf("caller filters must survive into the page query: %v", q)
	}
}

// The measured page shape is REQUIRED, and every way of failing to send it must
// read as malformed rather than as an empty account.
//
// V1.8a sampled every page of all four endpoints and each carried its cursor key
// — "present but empty on the final page" — plus all of its declared arrays.
// So `200 {}`, a null cursor, an absent array and a null array are not terminal
// pages; they are responses that do not match the contract. Reading any of them
// as "the walk finished and there is nothing there" is how a truncated proxy
// reply, a 200-with-an-error-page, or a schema change silently overwrites a live
// position with flat and abandons its reducer.
func TestMalformedPageIsNeverAnEmptyAccount(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":       `{}`,
		"whole body null":    `null`,
		"null cursor":        `{"cursor":null,"orders":[{"order_id":"o1"}]}`,
		"absent array":       `{"cursor":""}`,
		"null array":         `{"cursor":"","orders":null}`,
		"absent cursor only": `{"orders":[{"order_id":"o1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: 200, Body: []byte(body)}, nil
			}}
			w := NewClient(d).Walk(context.Background(), EpOrders, nil)
			if w.Replaces() {
				t.Fatalf("%s was accepted as a complete walk with %d records; "+
					"an incomplete response is not an empty account",
					name, len(w.Records("orders")))
			}
			if w.Outcome != WalkFailed {
				t.Fatalf("want WalkFailed, got %s", w.Outcome)
			}
		})
	}
}

// The dual-array endpoint must reject a page missing EITHER array, not just the
// one it reads for q.
func TestPositionsRejectsAPageMissingEitherArray(t *testing.T) {
	for name, body := range map[string]string{
		"no event_positions":  `{"cursor":"","market_positions":[]}`,
		"no market_positions": `{"cursor":"","event_positions":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: 200, Body: []byte(body)}, nil
			}}
			w := NewClient(d).Walk(context.Background(), EpPositions, nil)
			if w.Replaces() {
				t.Fatalf("%s was accepted as complete", name)
			}
		})
	}
}

// A well-formed empty page IS terminal: an empty array is how the exchange says
// "no records", and that must still work.
func TestWellFormedEmptyPageIsTerminal(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n > 0 {
			t.Fatalf("an empty cursor terminates the walk; got call %d", n)
		}
		return Response{Status: 200, Body: []byte(`{"cursor":"","orders":[]}`)}, nil
	}}
	w := NewClient(d).Walk(context.Background(), EpOrders, nil)
	if !w.Replaces() {
		t.Fatalf("want a complete walk, got %s (%v)", w.Outcome, w.Err)
	}
	if len(w.Records("orders")) != 0 {
		t.Fatalf("want no records, got %d", len(w.Records("orders")))
	}
}

// A page with an empty array but a NON-empty cursor keeps walking. Only the
// cursor ends a walk (H-PAGE-1a clause 3).
func TestEmptyArrayWithLiveCursorKeepsWalking(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		switch n {
		case 0:
			return Response{Status: 200, Body: []byte(`{"cursor":"C2","orders":[]}`)}, nil
		case 1:
			return Response{Status: 200, Body: []byte(
				`{"cursor":"","orders":[{"order_id":"o9"}]}`)}, nil
		}
		t.Fatalf("unexpected call %d", n)
		return Response{}, nil
	}}
	w := NewClient(d).Walk(context.Background(), EpOrders, nil)
	if !w.Replaces() {
		t.Fatalf("want a complete walk, got %s (%v)", w.Outcome, w.Err)
	}
	recs := w.Records("orders")
	if len(recs) != 1 || !strings.Contains(string(recs[0]), "o9") {
		t.Fatalf("the record behind the empty page was dropped: %v", recs)
	}
}

// F6 — the zero value of every result type must license nothing.
//
// A receive from a closed response channel, or a struct returned on a path that
// forgot to set an outcome, yields the zero value. If that reported "complete",
// a walk that never happened would replace the position map with nothing and
// abandon every held market at once.
func TestZeroValuedResultsReplaceNothing(t *testing.T) {
	var w Walk
	if w.Replaces() {
		t.Error("zero Walk reports Replaces(); the zero outcome must license nothing")
	}
	if w.Outcome != WalkUnset {
		t.Errorf("zero WalkOutcome is %s, want unset", w.Outcome)
	}
	var o OrdersResult
	if o.Replaces() || len(o.Orders) != 0 {
		t.Error("zero OrdersResult must not replace the order map")
	}
	var p PositionsResult
	if p.Replaces() || len(p.ByTicker) != 0 {
		t.Error("zero PositionsResult must not replace positions")
	}
	var f FillsResult
	if f.Replaces() || len(f.Fills) != 0 {
		t.Error("zero FillsResult must not replace fills")
	}
	var s SweepResult
	if s.Clean {
		t.Error("zero SweepResult must not read as a clean sweep")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

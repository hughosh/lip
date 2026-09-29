package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// H-PAGE-1 / H-PAGE-1a — the guarded cursor walk
// ---------------------------------------------------------------------------
//
// This is the only way this package reads a list. There is no single-page read
// anywhere, and no `limit` large enough to count as one.
//
// `probebot.py:1100-1108` carries the incident report this exists to prevent:
// the active-program count grew past 1,000 mid-probe, a one-shot `limit=200`
// fetch pushed the live market outside the window, and a restart died claiming
// "no active liquidity program" while the program was still running. Its own
// conclusion — "a lookup that silently depends on position in an unsorted list
// is a correctness bug, not a tuning parameter."

// WalkOutcome is how a walk ended. The distinction is not cosmetic: only one of
// these three permits the caller to replace state (H-PAGE-1 clause 2).
type WalkOutcome uint8

const (
	// WalkUnset is the zero value, and it is NOT "complete".
	//
	// `WalkComplete` used to be the zero value, which made `var w Walk` — and
	// every unpopulated or zero-valued result embedding one — report
	// Replaces() == true. That is reachable without anyone writing an obviously
	// wrong line: a receive from a closed response channel yields the zero
	// value, as does a struct returned on a path that forgot to set the
	// outcome. The consequence is a walk that never happened replacing the
	// position map with nothing, which abandons every held market at once.
	//
	// The safe direction is for silence to mean "we do not know", so the zero
	// value is the one outcome that licenses nothing.
	WalkUnset WalkOutcome = iota
	// WalkComplete means the cursor was walked to exhaustion. This is the ONLY
	// outcome that may replace state.
	WalkComplete
	// WalkRewound means forward progress failed: a page repeated an identity
	// already seen. The accumulated records are duplicates and are discarded.
	WalkRewound
	// WalkFailed means the walk did not finish — transport error, non-200,
	// undecodable body, or a payload whose shape no longer matches the measured
	// contract.
	WalkFailed
)

func (o WalkOutcome) String() string {
	switch o {
	case WalkComplete:
		return "complete"
	case WalkRewound:
		return "rewound"
	case WalkFailed:
		return "failed"
	}
	return "unset"
}

// Walk is the result of one complete-or-abandoned cursor walk.
type Walk struct {
	Outcome WalkOutcome
	// Items are the accumulated records per item key. EMPTY unless Outcome is
	// WalkComplete: H-PAGE-1 clause 2 forbids a partial or empty replacement,
	// and H-PAGE-1a clause 2 requires that a rewound walk "never emit the
	// accumulated duplicates". Returning them and trusting the caller to check
	// Outcome first is precisely the mistake this type is shaped to prevent.
	Items map[string][]json.RawMessage
	Pages int
	// Anomalies carries `SEV2 CURSOR_REWIND` and its siblings (V1.8b).
	Anomalies []risk.Anomaly
	// Err is the underlying cause when Outcome is WalkFailed.
	Err error
}

// Replaces reports whether this result may overwrite state.
//
// Stated positively and as a method because "did the walk complete" is the one
// question every caller must ask, and `if w.Err == nil` is the wrong way to ask
// it — a rewound walk has no error and must not replace anything.
func (w Walk) Replaces() bool { return w.Outcome == WalkComplete }

// Records returns the accumulated records for one item key.
func (w Walk) Records(key string) []json.RawMessage { return w.Items[key] }

// knownCursorFields is every cursor key either family is known to use. It is
// used to tell "the walk is finished" apart from "we are reading the wrong key"
// — see decodePage.
var knownCursorFields = []string{"cursor", "next_cursor"}

// Walk performs a complete cursor walk of `ep` under `filters`.
//
// It never returns a partial answer. Every failure path preserves the caller's
// prior state by returning something that reports Replaces() == false.
func (c *Client) Walk(ctx context.Context, ep Endpoint, filters url.Values) Walk {
	items := make(map[string][]json.RawMessage, len(ep.ItemKeys))
	for _, k := range ep.ItemKeys {
		items[k] = nil
	}

	// seen holds the identity of every page's FIRST record, and every cursor we
	// have already sent. A repeat of either means the walk has doubled back.
	seen := make(map[string]bool)
	cursor := ""
	pages := 0

	for {
		if pages >= ep.MaxPages {
			// A circuit breaker, and deliberately NOT the page cap H-PAGE-1a
			// forbids. The forbidden one caps a walk and RETURNS WHAT IT GOT,
			// converting a silent infinite loop into a silent truncation. This
			// one abandons the walk and returns no answer at all, so the caller
			// keeps its prior state and starts the staleness clock. The
			// difference is whether a partial read can be mistaken for a
			// complete one, and here it cannot.
			return Walk{
				Outcome: WalkFailed,
				Pages:   pages,
				Err:     fmt.Errorf("%s: walk exceeded %d pages", ep.Path, ep.MaxPages),
				Anomalies: []risk.Anomaly{{
					Class: "CURSOR_UNBOUNDED", Sev: risk.SEV2,
					Text: fmt.Sprintf("%s did not terminate within %d pages; "+
						"walk abandoned, prior state preserved and stale",
						ep.Path, ep.MaxPages),
				}},
			}
		}

		q, err := pageQuery(ep, filters, cursor)
		if err != nil {
			return Walk{Outcome: WalkFailed, Pages: pages, Err: err}
		}
		resp, err := c.get(ctx, ep, q)
		if err != nil {
			return Walk{Outcome: WalkFailed, Pages: pages,
				Err: fmt.Errorf("%s page %d: %w", ep.Path, pages+1, err)}
		}
		if resp.Status != 200 {
			if resp.Status == 429 {
				return Walk{Outcome: WalkFailed, Pages: pages,
					Err: fmt.Errorf("%s page %d: %w", ep.Path, pages+1,
						rateLimitError(resp))}
			}
			return Walk{Outcome: WalkFailed, Pages: pages,
				Err: fmt.Errorf("%s page %d: HTTP %d: %s",
					ep.Path, pages+1, resp.Status, snippet(resp.Body))}
		}

		pg, err := decodePage(ep, resp.Body)
		if err != nil {
			return Walk{Outcome: WalkFailed, Pages: pages,
				Err: fmt.Errorf("%s page %d: %w", ep.Path, pages+1, err)}
		}
		pages++

		// --- H-PAGE-1a clause 2: assert forward progress -------------------
		//
		// The portfolio family does not reject a cursor it cannot parse. It
		// returns HTTP 200 and page one. Measured against /portfolio/fills:
		// garbage, an empty string, a truncated-but-valid cursor and a cursor
		// minted by /incentive_programs ALL returned the first record, while
		// the valid control advanced. So a walk cannot rely on the endpoint to
		// tell it that it has gone backwards; it has to notice by itself.
		id, err := firstIdentity(ep, pg)
		if err != nil {
			return Walk{Outcome: WalkFailed, Pages: pages,
				Err: fmt.Errorf("%s page %d: cannot assert forward progress: %w",
					ep.Path, pages, err)}
		}
		if id != "" {
			if seen[id] {
				return rewound(ep, pages, "a page repeated the first record "+
					"identity "+id)
			}
			seen[id] = true
		}

		for _, k := range ep.ItemKeys {
			items[k] = append(items[k], pg.items[k]...)
		}

		next := pg.cursor
		if next == "" {
			// The only legitimate terminal condition.
			//
			// NOT "the item array came back empty" — H-PAGE-1a clause 3
			// forbids that, because /portfolio/positions returns
			// market_positions AND event_positions under a single cursor and
			// whether they exhaust together is unestablished (pagecontract
			// §6.2). `probebot.py:1123` terminates on `not programs`, which on
			// a dual-array endpoint drops the second array entirely.
			return Walk{Outcome: WalkComplete, Items: items, Pages: pages}
		}
		// Clause 1: used exactly as received. Never repaired, truncated,
		// re-encoded, or carried across endpoints — all four of those return
		// HTTP 200 and page one rather than an error.
		if seen[cursorKey(next)] {
			return rewound(ep, pages, "a cursor already sent in this walk was "+
				"returned again")
		}
		seen[cursorKey(next)] = true
		cursor = next
	}
}

// rewound builds the abandon-the-walk result. The accumulated records are not
// attached: they contain duplicates by construction.
func rewound(ep Endpoint, pages int, why string) Walk {
	return Walk{
		Outcome: WalkRewound,
		Pages:   pages,
		Anomalies: []risk.Anomaly{{
			Class: "CURSOR_REWIND", Sev: risk.SEV2,
			Text: fmt.Sprintf("%s rewound at page %d: %s; walk abandoned, "+
				"records discarded, prior state preserved and marked stale",
				ep.Path, pages, why),
		}},
	}
}

// cursorKey namespaces a cursor inside `seen` so it cannot collide with a
// record identity.
func cursorKey(c string) string { return "cursor\x00" + c }

// ---------------------------------------------------------------------------
// Page decoding
// ---------------------------------------------------------------------------

type page struct {
	items  map[string][]json.RawMessage
	cursor string
}

// decodePage reads one page against the endpoint's measured contract.
func decodePage(ep Endpoint, body []byte) (page, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return page{}, fmt.Errorf("undecodable body: %w", err)
	}

	// --- V1.8c / M22: the cursor field name is per-endpoint ----------------
	//
	// Reading the wrong key does not error. It yields an empty cursor, so the
	// walker concludes it has finished and returns page one as the whole
	// answer — `probebot.py:1100`'s incident reproduced by a typo. Both halves
	// of the answer already lived in this repository, each believing it was
	// universal (`probebot.py:1123` reads next_cursor, `opportunity.py:54`
	// reads cursor).
	//
	// So: if OUR key is absent but the OTHER family's key is present, that is a
	// misconfiguration, not a completed walk. Fail loudly. This is what makes
	// the mistake detectable on page one, without needing a two-page fixture.
	ours, haveOurs := raw[ep.CursorField]
	if !haveOurs {
		for _, other := range knownCursorFields {
			if other == ep.CursorField {
				continue
			}
			if _, ok := raw[other]; ok {
				return page{}, fmt.Errorf(
					"%s: cursor field %q is absent but %q is present; reading "+
						"the wrong cursor key reports an empty cursor and "+
						"silently returns page one as the whole answer (V1.8c)",
					ep.Path, ep.CursorField, other)
			}
		}
		// --- The measured shape is REQUIRED, not merely expected ------------
		//
		// Every page of every endpoint sampled by V1.8a carried its cursor key,
		// "present but empty on the final page" — including the terminal page,
		// whose top-level keys were exactly {cursor, orders},
		// {cursor, fills}, {cursor, event_positions, market_positions} and
		// {next_cursor, incentive_programs}.
		//
		// So an ABSENT cursor key is not a terminal page. It is a response that
		// does not match the contract, and treating it as terminal is how
		// `200 {}` — a truncated proxy reply, an error page with a 200, a
		// changed schema — becomes "the walk finished and the account is
		// empty". Against positions that overwrites a live position with flat
		// and abandons its reducer; against orders it makes an ignored cancel
		// look swept while the order is still fillable.
		return page{}, fmt.Errorf("%s: cursor field %q is absent; every "+
			"measured page carries it, present but empty when terminal, so an "+
			"absent cursor is a malformed response and not an empty account",
			ep.Path, ep.CursorField)
	}
	if isJSONNull(ours) {
		return page{}, fmt.Errorf("%s: cursor field %q is null; the measured "+
			"terminal cursor is an empty string, and a null here is a shape we "+
			"have never observed rather than a walk that finished",
			ep.Path, ep.CursorField)
	}

	var cursor string
	if err := json.Unmarshal(ours, &cursor); err != nil {
		return page{}, fmt.Errorf("%s: cursor field %q is not a string: %w",
			ep.Path, ep.CursorField, err)
	}

	p := page{items: make(map[string][]json.RawMessage, len(ep.ItemKeys)), cursor: cursor}
	for _, k := range ep.ItemKeys {
		rawList, ok := raw[k]
		if !ok {
			// Same argument as the cursor: the declared arrays were present on
			// every measured page. An absent one is a malformed response, and
			// reading it as "no records" is the same silent-empty failure.
			return page{}, fmt.Errorf("%s: declared item array %q is absent; "+
				"every measured page carries it, empty when there are no "+
				"records, so its absence is a malformed response and not an "+
				"empty result", ep.Path, k)
		}
		if isJSONNull(rawList) {
			// encoding/json unmarshals a JSON null into a slice as a nil slice
			// with NO error, so this has to be checked explicitly or a null
			// array is indistinguishable from an empty one.
			return page{}, fmt.Errorf("%s: declared item array %q is null; "+
				"the measured empty page carries an empty array, not a null",
				ep.Path, k)
		}
		var list []json.RawMessage
		if err := json.Unmarshal(rawList, &list); err != nil {
			return page{}, fmt.Errorf("%s: item key %q is not an array: %w",
				ep.Path, k, err)
		}
		p.items[k] = list
	}
	return p, nil
}

// isJSONNull reports whether a raw message is the JSON literal null.
//
// It exists because encoding/json treats null as "no effect on the value and no
// error" for almost every target type: a null string stays "", a null bool
// stays false, a null slice stays nil, a null number stays 0. Every one of
// those is a plausible-looking answer, and three of them are answers this
// harness must never invent.
func isJSONNull(r json.RawMessage) bool {
	return string(bytes.TrimSpace(r)) == "null"
}

// firstIdentity is the value the forward-progress guard compares.
//
// It is taken from the first record of the first NON-EMPTY item key, in the
// endpoint's declared key order, using that key's measured identity field. An
// empty page has no identity and returns ("", nil) — that is not an error and
// not a rewind; it is handled by the cursor-repeat guard and the circuit
// breaker instead.
func firstIdentity(ep Endpoint, p page) (string, error) {
	for _, k := range ep.ItemKeys {
		recs := p.items[k]
		if len(recs) == 0 {
			continue
		}
		field := ep.IDFields[k]
		if field == "" {
			// No identity field was measured for this key. The record's own
			// bytes are a weaker but sound substitute: a rewind re-serves the
			// identical record, so a repeat is still detected. The weakness is
			// an in-place update between pages changing the bytes, which
			// produces a MISSED detection rather than a false one — and
			// pagecontract §6.4 already records mid-walk updates as
			// unestablished.
			return k + "\x00" + string(recs[0]), nil
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(recs[0], &rec); err != nil {
			return "", fmt.Errorf("record in %q is not an object: %w", k, err)
		}
		v, ok := rec[field]
		if !ok {
			// The measured payload shape has changed. Papering over this with
			// a fallback would silently disable the guard on the one endpoint
			// whose guard matters most.
			return "", fmt.Errorf("record in %q has no %q field; the measured "+
				"payload shape has changed and forward progress cannot be "+
				"asserted without it", k, field)
		}
		return k + "\x00" + string(v), nil
	}
	return "", nil
}

// snippet bounds an error body so a 4xx page of HTML cannot flood a log line.
func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// confidence: high

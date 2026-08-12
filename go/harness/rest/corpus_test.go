package rest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lip-rj5 — the decoders, against payloads the EXCHANGE wrote.
//
// Every other fixture in this package is a Go literal written by the same person
// who wrote the decoder it tests, with synthetic identifiers: `o1`, `T1`,
// `position_fp: "3.50"`. That is not a slip, it is the structural reason a
// strict parser meeting real data is the recurring defect shape in this tree.
// Nobody hand-writes the record shape they did not know existed -- not a
// six-decimal `fee_cost` (`lip-9tr`, which halted the first q01 attempt in three
// seconds), not a `volume` programme with no `target_size_fp`, not a market
// whose status is `finalized`. All three were found in one read-only sweep and
// none of them were reachable from the fixtures that existed.
//
// The records under `testdata/kalshi` are what the account actually returned on
// 2026-08-12, captured by `go/cmd/conform` and promoted by
// `scripts/make_rest_testdata.py`, which also documents exactly which fields
// were substituted. Nothing the decoder interprets was touched.
//
// THE ISOLATION MATTERS, and it is why each record gets its own walk below.
// Every decoder here is FIRST-ERROR-WINS by design: one undecodable order
// invalidates the whole walk. That is correct for production -- a
// partially-understood walk must never replace state -- but it means a
// whole-corpus test reports record 0 and hides records 1..N. Replaying each
// record alone gives every record its own verdict, so one regression does not
// mask the rest.

// The corpus sits at `harness/testdata` rather than under this package because
// the fields on a record are read from more than one package: `rest` decodes a
// fill, and `wsx.convertFills` is what parses the `fee_cost` on it. `lip-9tr`
// was in the second, so a corpus only `rest` could reach would have missed the
// defect that motivated recording one. See `harness/wsx/corpus_test.go`.
const corpusDir = "../testdata/kalshi"

// corpusDoer answers every request with the same page. It is how one raw record
// is fed back through the real typed decoder: the client walks, sees a single
// page with a terminal cursor, and decodes through exactly the production code.
type corpusDoer struct{ body []byte }

func (d corpusDoer) Do(_ context.Context, _ Request) (Response, error) {
	return Response{Status: 200, Body: d.body}, nil
}

// corpusPage builds the synthetic page.
//
// The cursor must be PRESENT and terminal: an absent cursor is a malformed
// response to `decodePage`, not the end of a walk, so omitting it would report
// every record as broken. Every declared item array must be present too, which
// is why the loop covers `ep.ItemKeys` rather than just the one under test --
// `/portfolio/positions` declares two arrays under one cursor.
func corpusPage(t *testing.T, ep Endpoint, itemKey string, recs []json.RawMessage) []byte {
	t.Helper()
	page := map[string]any{ep.CursorField: ""}
	for _, key := range ep.ItemKeys {
		if key == itemKey {
			page[key] = recs
		} else {
			page[key] = []json.RawMessage{}
		}
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("building the synthetic %s page: %v", itemKey, err)
	}
	return body
}

func corpusRecords(t *testing.T, file string) []json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, file))
	if err != nil {
		t.Fatalf("reading the recorded corpus: %v", err)
	}
	var recs []json.RawMessage
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatalf("%s is not an array of records: %v", file, err)
	}
	if len(recs) == 0 {
		t.Fatalf("%s is empty; a fixture that asserts nothing passes forever", file)
	}
	return recs
}

// corpusEndpoint binds a recorded file to the endpoint it came from and to the
// PRODUCTION decoder that reads it. `decode` calls the same exported method the
// harness calls, with the same arguments, so a record accepted here is a record
// production accepts.
type corpusEndpoint struct {
	name    string
	file    string
	ep      Endpoint
	itemKey string
	decode  func(*Client) Walk
	// typed reports how many typed records the decoder produced, or -1 when the
	// decoder does not expose a count that a fixture can predict.
	typed func(*Client) int
}

func corpusEndpoints() []corpusEndpoint {
	return []corpusEndpoint{
		{
			name: "fills", file: "fills.json", ep: EpFills, itemKey: "fills",
			// Zero `since` and no ticker: the shape the LIVE poller uses, and
			// the one that walks all history and met `lip-9tr`'s bad fill.
			decode: func(c *Client) Walk { return c.Fills(context.Background(), "", time.Time{}).Walk },
			typed:  func(c *Client) int { return len(c.Fills(context.Background(), "", time.Time{}).Fills) },
		},
		{
			name: "orders", file: "orders.json", ep: EpOrders, itemKey: "orders",
			decode: func(c *Client) Walk { return c.Orders(context.Background(), "", "").Walk },
			typed:  func(c *Client) int { return len(c.Orders(context.Background(), "", "").Orders) },
		},
		{
			name: "positions.market", file: "market_positions.json",
			ep: EpPositions, itemKey: "market_positions",
			decode: func(c *Client) Walk { return c.Positions(context.Background()).Walk },
			// ByTicker drops zero positions, and the recorded market position
			// IS zero, so its size is not a fact about decoding.
			typed: func(*Client) int { return -1 },
		},
		{
			// The harness reads `market_positions` for q, but the walk carries
			// both arrays and a decoder defect in either one fails it.
			name: "positions.event", file: "event_positions.json",
			ep: EpPositions, itemKey: "event_positions",
			decode: func(c *Client) Walk { return c.Positions(context.Background()).Walk },
			typed:  func(*Client) int { return -1 },
		},
		{
			name: "programs", file: "incentive_programs.json",
			ep: EpPrograms, itemKey: "incentive_programs",
			decode: func(c *Client) Walk { return c.Programs(context.Background()).Walk },
			typed:  func(c *Client) int { return len(c.Programs(context.Background()).ByTarget) },
		},
	}
}

// Each recorded record, alone, through the real decoder.
func TestRecordedKalshiPayloadsDecodeOneRecordAtATime(t *testing.T) {
	for _, ep := range corpusEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			recs := corpusRecords(t, ep.file)
			for i, rec := range recs {
				body := corpusPage(t, ep.ep, ep.itemKey, []json.RawMessage{rec})
				w := ep.decode(NewClient(corpusDoer{body: body}))
				if !w.Replaces() {
					t.Errorf("record %d of %s, exactly as the exchange sent it, "+
						"was refused by the production decoder: %s after %d "+
						"page(s): %v\nrecord: %s\n\nThis is `lip-9tr`'s shape: "+
						"a strict parser meeting a real field. First-error-wins "+
						"means production would discard the WHOLE walk over it.",
						i, ep.file, w.Outcome, w.Pages, w.Err, rec)
				}
			}
		})
	}
}

// The same records as production actually meets them: one walk, all of them.
//
// This is the assertion the isolated pass cannot make. First-error-wins is a
// property of the BATCH, and a decoder that accepts every record alone but
// rejects the set -- a duplicate identity tripping the forward-progress guard,
// a cross-record invariant -- would pass above and fail here.
func TestRecordedKalshiPayloadsDecodeAsOneWalk(t *testing.T) {
	for _, ep := range corpusEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			recs := corpusRecords(t, ep.file)
			body := corpusPage(t, ep.ep, ep.itemKey, recs)
			client := NewClient(corpusDoer{body: body})
			if w := ep.decode(client); !w.Replaces() {
				t.Fatalf("the recorded %s walk (%d records) was refused whole: "+
					"%s after %d page(s): %v", ep.name, len(recs), w.Outcome,
					w.Pages, w.Err)
			}
			if got := ep.typed(client); got >= 0 && got != len(recs) {
				t.Errorf("the %s walk decoded %d typed record(s) from %d "+
					"recorded one(s); a decoder that ACCEPTS a walk while "+
					"silently dropping records replaces state with less than "+
					"the exchange reported", ep.name, got, len(recs))
			}
		})
	}
}

// The other half of the corpus, and the reason this is not an acceptance-only
// fixture set.
//
// The active-programme walk this account sees is 100% `liquidity` with a
// `target_size_fp` on every record, so a fixture set built only from what the
// harness reads today would rebuild exactly the blind spot `lip-rj5` exists to
// remove. These are real `volume` programmes from the unfiltered endpoint --
// 22,318 of them exist and not one carries a Target Size.
//
// The refusal is CORRECT and the test pins it as such: `Qualifies()`'s threshold
// has no defensible default, so inventing one would silently misreport whether a
// market was quotable. What the test also pins is the blast radius. `Programs`
// is first-error-wins and `exchangeOver` turns its failure into a refusal to
// START, so the day the active set includes one of these, the harness does not
// boot -- and it will look like an outage, not a schema change. That is
// `lip-44s`, and this is the fixture that will still be here when it fires.
func TestRecordedVolumeProgrammesAreRefusedForTheirTargetSize(t *testing.T) {
	recs := corpusRecords(t, "incentive_programs_refused.json")
	for i, rec := range recs {
		body := corpusPage(t, EpPrograms, "incentive_programs", []json.RawMessage{rec})
		result := NewClient(corpusDoer{body: body}).Programs(context.Background())
		if result.Replaces() {
			t.Errorf("record %d of the refused corpus was ACCEPTED: %s\n"+
				"A programme with no target_size_fp has no Target Size, and "+
				"`Qualifies()` measures against it. Accepting one means a "+
				"zero threshold, which reports every interval as qualifying.",
				i, rec)
			continue
		}
		if !strings.Contains(result.Err.Error(), "target_size_fp") {
			t.Errorf("record %d was refused for the wrong reason: %v\n"+
				"The refusal has to name the missing Target Size, or the "+
				"operator meeting this on a boot failure learns nothing.",
				i, result.Err)
		}
	}
}

package wsx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/rest"
)

// lip-rj5 — the fee path, against payloads the EXCHANGE wrote.
//
// THIS IS THE TEST THAT COVERS `lip-9tr`, and it is in `wsx` rather than in
// `rest` for a reason worth stating: the walk decoder does NOT parse the fee.
// `rest.Fill.FeeCost` is carried as the raw string, and `convertFills` below is
// what turns it into money. So a real-payload fixture wired only into
// `harness/rest` would decode all 15 recorded fills, pass, and prove nothing
// about the defect that actually took the harness to a durable WINDING_DOWN 2.3
// seconds after launch on the first q01 attempt.
//
// The path here is the whole production one, on real bytes: the recorded fills
// go through `rest.Client.Fills` -- the same walk, cursor and decode machinery
// the live poller uses -- and the `[]rest.Fill` that comes out goes straight
// into `convertFills`. Both halves must accept, and the fee has to arrive as the
// exact value the exchange sent.

const wsxCorpusDir = "../testdata/kalshi"

// corpusDoer answers every request with one canned page, so a recorded record
// can be replayed through the real decoder.
type corpusDoer struct{ body []byte }

func (d corpusDoer) Do(_ context.Context, _ rest.Request) (rest.Response, error) {
	return rest.Response{Status: 200, Body: d.body}, nil
}

func recordedFills(t *testing.T) []rest.Fill {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(wsxCorpusDir, "fills.json"))
	if err != nil {
		t.Fatalf("reading the recorded fills: %v", err)
	}
	var recs []json.RawMessage
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatalf("the recorded fills are not an array: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("the recorded fills are empty; a fixture that asserts nothing passes forever")
	}
	// The cursor must be present and terminal, or the walk reads the page as
	// malformed rather than as complete.
	body, err := json.Marshal(map[string]any{
		rest.EpFills.CursorField: "",
		"fills":                  recs,
	})
	if err != nil {
		t.Fatalf("building the synthetic fills page: %v", err)
	}
	result := rest.NewClient(corpusDoer{body: body}).
		Fills(context.Background(), "", time.Time{})
	if !result.Replaces() {
		t.Fatalf("the recorded fills walk was refused: %s after %d page(s): %v",
			result.Outcome, result.Pages, result.Err)
	}
	if len(result.Fills) != len(recs) {
		t.Fatalf("decoded %d fill(s) from %d recorded record(s)",
			len(result.Fills), len(recs))
	}
	return result.Fills
}

// Every recorded fill, through the real fee parser.
func TestRecordedFillsConvertWithTheirRealFees(t *testing.T) {
	fills := recordedFills(t)
	events, err := convertFills(fills)
	if err != nil {
		t.Fatalf("convertFills refused the account's own recorded fills: %v\n\n"+
			"This is `lip-9tr` exactly: `fee_cost` arrives with SIX decimals on "+
			"every one of these records, including the zero fee as \"0.000000\", "+
			"and a parser at the wrong quantum discards the entire walk. A fee "+
			"that cannot be read is one of H-ORD-8's two independent witnesses "+
			"going silent.", err)
	}
	if len(events) != len(fills) {
		t.Fatalf("converted %d event(s) from %d fill(s)", len(events), len(fills))
	}

	// Not merely "no error": the VALUE has to survive. A parser that accepted
	// six decimals by truncating to four would pass the check above and lose
	// $0.0072 of every fee it read.
	var checked int
	for i, event := range events {
		want, err := rest.ParseFee6(fills[i].FeeCost)
		if err != nil {
			t.Fatalf("fee %q on a recorded fill does not parse: %v",
				fills[i].FeeCost, err)
		}
		if event.Fee != want {
			t.Errorf("fill %d carried fee_cost %q and converted to %v, want %v",
				i, fills[i].FeeCost, event.Fee, want)
		}
		if fills[i].FeeCost != "0.000000" {
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("every recorded fee was zero, so nothing above distinguishes a " +
			"working parser from one that returns zero; re-record the corpus")
	}
}

// The measured quantum, pinned to a specific recorded fill.
//
// The histogram over all 15 recorded fills is {6 decimals: 15} with no other
// width present, so this is not a lucky record -- it is the shape of the field.
// H-ORD-8 turns on it: maker fees are $0.00, so a non-zero fee means a TAKER
// fill whatever `is_taker` claims, and the two detectors fail independently.
func TestRecordedSixDecimalFeeSurvivesTheWholeConversion(t *testing.T) {
	fills := recordedFills(t)
	events, err := convertFills(fills)
	if err != nil {
		t.Fatalf("convertFills: %v", err)
	}

	var found bool
	for i, event := range events {
		if fills[i].FeeCost != "0.017200" {
			continue
		}
		found = true
		// 0.017200 dollars in Money's 1e-6 quantum, written out rather than
		// computed, so a change to the scale has to be noticed here.
		if want := num.Money(17200); event.Fee != want {
			t.Fatalf("fee_cost %q converted to %v, want %v",
				fills[i].FeeCost, event.Fee, want)
		}
		if !event.IsTaker {
			t.Errorf("the recorded fill carrying a non-zero fee is not marked a "+
				"taker fill; H-ORD-8 reads the fee as the independent witness "+
				"that it is (fee %v)", event.Fee)
		}
	}
	if !found {
		t.Fatal("the recorded fill carrying fee_cost \"0.017200\" is gone from " +
			"the corpus; it is the one this assertion is about")
	}
}

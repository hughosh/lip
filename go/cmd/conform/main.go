// conform is the read-only conformance sweep: it pulls COMPLETE walks from
// every endpoint the harness reads and pushes every record through the
// harness's OWN decoders, one record at a time.
//
// WHY IT EXISTS. `lip-9tr` was found by running the live harness against the
// real account and watching it halt in the first three seconds: a historical
// fill carries `fee_cost "0.017200"`, `rest.ParsePrice4` refuses more than four
// decimals, `convertFills` fails, and the whole walk is discarded. That cost a
// full operator session and a ~5h gate round to learn ONE fact about the wire.
// This program learns the same class of fact for every field of every endpoint
// in seconds, for the price of some GETs.
//
// THE ISOLATION IS THE POINT, and it is why this is not simply a call to
// `Client.Fills`. Every decoder in `rest` is FIRST-ERROR-WINS by design -- one
// undecodable order invalidates the whole walk (`read.go:182-198`), one bad
// fill returns `nil, err` for the batch (`wsx/portfolio.go:556-583`). That is
// correct for production, where a partially-understood walk must never replace
// state. It is useless for discovery: it reports record 0 and hides records 1..N,
// so a survey built on it finds exactly one defect per run and needs a fresh
// live run for each. The known defects are therefore not just blockers, they are
// OCCLUDERS, and the whole reason the live surface is under-mapped.
//
// So the sweep separates the two jobs the production path fuses:
//
//	Phase 1 walks the endpoint with the real transport and keeps the RAW
//	records, using `Client.Walk` -- which is the same pagination, cursor and
//	rewind machinery production uses, so a walk defect still surfaces here.
//
//	Phase 2 replays each raw record through the real typed decoder ALONE, by
//	handing it back through a `Doer` that answers with a synthetic one-record
//	page. The decoder is the production decoder, byte-identical in behaviour;
//	only the batch boundary moves. A record that fails here is a record that
//	would have failed in production, and every OTHER record still gets its
//	verdict.
//
// READ-ONLY IS STRUCTURAL, not a convention. `getOnly` sits ABOVE the transport
// and refuses any non-GET before a socket is touched, so no code path in this
// program -- present or future -- can write to the account. That mirrors
// `scripts/pagecontract.py`'s `dry_run=True`, which is the precedent for
// probing this account safely.
//
// NOT GATED. `loop/gates.sh` scopes `go vet`, `gofmt` and `go test -race` to
// `./harness/...` and `./cmd/harness/...`, so this package is compiled by
// `go build ./...` and nothing more. It is a diagnostic instrument, not
// production code, and it must never be imported by the harness.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"lip/feed"
	"lip/harness/rest"
	"lip/harness/wsx"
	"lip/tape"
)

const restTimeout = 20 * time.Second

// failureSamples bounds how many refused records are kept PER ENDPOINT. The
// count is always exact; only the stored evidence is sampled.
const failureSamples = 25

// ---------------------------------------------------------------------------
// The two Doers
// ---------------------------------------------------------------------------

// getOnly refuses everything that is not a GET, above the transport.
//
// The refusal is an `error` rather than a synthetic 4xx `Response` on purpose:
// `rest.Doer`'s contract is that a `Response` means the exchange answered, and
// a caller that reads a fabricated 403 as "the exchange rejected us" would be
// drawing a conclusion about an account this program never contacted.
type getOnly struct{ next rest.Doer }

func (d getOnly) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if req.Method != "GET" {
		return rest.Response{}, fmt.Errorf(
			"conform is read-only and refuses %s %s before the transport",
			req.Method, req.Path)
	}
	return d.next.Do(ctx, req)
}

// onePage answers every request with the same canned body. It is how phase 2
// feeds a single raw record back through the real typed decoder: the decoder
// walks, sees one page with one record and a terminal cursor, and decodes it
// through exactly the code production uses.
type onePage struct{ body []byte }

func (d onePage) Do(_ context.Context, _ rest.Request) (rest.Response, error) {
	return rest.Response{Status: 200, Body: d.body}, nil
}

// ---------------------------------------------------------------------------
// The probe table
// ---------------------------------------------------------------------------

// probe is one endpoint's sweep: how to walk it, and how to decode one of its
// records in isolation.
type probe struct {
	name string
	ep   rest.Endpoint
	// itemKey is the array this probe reads records from. `positions` has two,
	// so it appears in the table twice.
	itemKey string
	// siblings are the other arrays that must be present in a synthetic page
	// for `decodePage` to accept it. `decodePage` refuses a declared item array
	// that is absent (page.go:311-314), and the positions endpoint declares two.
	siblings []string
	// filters MUST match what the production caller sends, or the sweep judges
	// the decoder against records production never sees. `Programs` sends
	// `status=active` (read.go:631-632); the unfiltered walk returns 137,569
	// records including every expired `incentive_type: "volume"` programme,
	// none of which carry `target_size_fp`. Reporting those as live refusals
	// would be a fabricated defect.
	filters url.Values
	// latent marks a probe that deliberately walks WIDER than production, to
	// show what the decoder would meet if the server-side filter ever stopped
	// protecting it. Its refusals are a risk, not a live defect, and the report
	// keeps the two apart.
	latent bool
	// decode runs the real typed decoder over a client whose Doer is `onePage`,
	// and reports why the record was refused. nil error means the record
	// decodes exactly as production would decode it.
	decode func(ctx context.Context, c *rest.Client) error
}

func probes() []probe {
	fills := func(ctx context.Context, c *rest.Client) error {
		// Zero `since` and no ticker: the shape the LIVE poller uses, which is
		// the one that walks all history and met the bad fill.
		r := c.Fills(ctx, "", time.Time{})
		if !r.Replaces() {
			return r.Err
		}
		return nil
	}
	orders := func(ctx context.Context, c *rest.Client) error {
		// Empty status is legitimate and unfiltered (client.go:233-235).
		r := c.Orders(ctx, "", "")
		if !r.Replaces() {
			return r.Err
		}
		return nil
	}
	positions := func(ctx context.Context, c *rest.Client) error {
		r := c.Positions(ctx)
		if !r.Replaces() {
			return r.Err
		}
		return nil
	}
	programs := func(ctx context.Context, c *rest.Client) error {
		r := c.Programs(ctx)
		if !r.Replaces() {
			return r.Err
		}
		return nil
	}
	return []probe{
		{name: "fills", ep: rest.EpFills, itemKey: "fills", decode: fills},
		{name: "orders", ep: rest.EpOrders, itemKey: "orders", decode: orders},
		{
			name: "positions.market", ep: rest.EpPositions,
			itemKey: "market_positions", siblings: []string{"event_positions"},
			decode: positions,
		},
		{
			// The harness reads `market_positions` for q, but the walk carries
			// both arrays and a decoder defect in either one fails it.
			name: "positions.event", ep: rest.EpPositions,
			itemKey: "event_positions", siblings: []string{"market_positions"},
			decode: positions,
		},
		{
			// Exactly what the harness sends at startup (read.go:631-632).
			name: "programs", ep: rest.EpPrograms,
			itemKey: "incentive_programs", decode: programs,
			filters: url.Values{"status": []string{"active"}},
		},
		{
			// The same decoder against the whole endpoint. `Programs` is
			// first-error-wins and `runtime.go:328` turns its failure into a
			// refusal to START, so anything this finds is a latent boot
			// failure waiting for the active set to include one such record.
			name: "programs.unfiltered", ep: rest.EpPrograms,
			itemKey: "incentive_programs", decode: programs,
			latent: true,
		},
	}
}

// synthPage builds the one-record page phase 2 replays through the decoder.
//
// The cursor field must be PRESENT and terminal: `decodePage` treats an absent
// cursor as a malformed response rather than as the end of a walk
// (page.go:286-289), so omitting it would report every record as broken.
func synthPage(p probe, rec json.RawMessage) ([]byte, error) {
	page := map[string]any{
		p.ep.CursorField: "",
		p.itemKey:        []json.RawMessage{rec},
	}
	for _, sib := range p.siblings {
		page[sib] = []json.RawMessage{}
	}
	return json.Marshal(page)
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

type recordFailure struct {
	Endpoint string          `json:"endpoint"`
	Index    int             `json:"index"`
	Reason   string          `json:"reason"`
	Record   json.RawMessage `json:"record"`
}

type endpointReport struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Latent marks a probe that walked wider than production. Its refusals are
	// a risk to be judged, not a defect firing today.
	Latent     bool     `json:"latent"`
	Outcome    string   `json:"walk_outcome"`
	Pages      int      `json:"pages"`
	Records    int      `json:"records"`
	Failed     int      `json:"failed"`
	WalkErr    string   `json:"walk_error,omitempty"`
	WalkAnoms  []string `json:"walk_anomalies,omitempty"`
	FullWalkOK bool     `json:"full_walk_decodes"`
	FullWalkNo string   `json:"full_walk_error,omitempty"`
	// Shapes counts records by the fields that decide whether the decoder
	// accepts them, so a latent probe reports a handful of shapes rather than
	// tens of thousands of identical refusals.
	Shapes map[string]int `json:"record_shapes,omitempty"`
}

// shapeHisto buckets incentive-programme records by the two fields that decide
// the decoder's verdict: the programme type, and whether the Target Size the
// decoder requires is actually present.
func shapeHisto(recs []json.RawMessage) map[string]int {
	h := map[string]int{}
	for _, rec := range recs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec, &m); err != nil {
			h["UNDECODABLE"]++
			continue
		}
		typ := scalar(m["incentive_type"])
		if typ == "" {
			typ = "(no incentive_type)"
		}
		has := "has target_size_fp"
		if scalar(m["target_size_fp"]) == "" {
			has = "NO target_size_fp"
		}
		h[typ+" / "+has]++
	}
	return h
}

type report struct {
	RanAt     string           `json:"ran_at"`
	Endpoints []endpointReport `json:"endpoints"`
	Failures  []recordFailure  `json:"failures"`
	Fees      *feeReport       `json:"fees,omitempty"`
	Prices    *priceReport     `json:"prices,omitempty"`
	Markets   *marketReport    `json:"markets,omitempty"`
	Balance   string           `json:"balance"`
	Tapes     []tapeReport     `json:"tapes,omitempty"`
}

// feeReport answers lip-9tr's open question directly: what IS the measured
// quantum of `fee_cost`, and how many fills would trip H-ORD-8's taker rule
// once the field becomes readable at all.
type feeReport struct {
	Fills          int            `json:"fills"`
	DecimalsHisto  map[string]int `json:"fee_decimal_places"`
	MaxDecimals    int            `json:"max_decimals"`
	NonZeroFee     int            `json:"nonzero_fee"`
	IsTakerTrue    int            `json:"is_taker_true"`
	WouldTripTaker int            `json:"would_trip_taker_fill"`
	DistinctFees   []string       `json:"distinct_fee_values"`
	Unparseable    []string       `json:"fee_values_ParsePrice4_refuses"`
}

// priceReport measures the complement invariant at read.go:320-323, which
// refuses any record carrying both price fields unless they sum to exactly
// $1.0000. It is first-error-wins over the whole walk, so a single violating
// record anywhere in account history would behave exactly like lip-9tr.
type priceReport struct {
	RecordsWithBothFields int      `json:"records_with_both_price_fields"`
	Violations            int      `json:"complement_violations"`
	Examples              []string `json:"examples"`
}

type marketReport struct {
	Sampled          int `json:"sampled"`
	Observed         int `json:"observed"`
	Failed           int `json:"failed"`
	ActiveWithResult int `json:"status_active_with_result"`
	NoCloseTime      int `json:"close_time_unreadable"`
	// Statuses is the measured vocabulary: every `status` seen, split by
	// whether a settlement result had been published yet. The pair is what
	// decides deriveTradingClosed's branch, and its refusing default is
	// reachable only from a non-active status with no result.
	Statuses map[string]int `json:"status_vocabulary"`
	Reasons  []string       `json:"distinct_failure_reasons"`
}

// tapeReport is the websocket half of the sweep: every RECORDED frame pushed
// through the harness's own frame classifier.
//
// This is the only way to judge the wsx decode surface without a live socket,
// and it is worth doing separately from the REST sweep because the two surfaces
// fail differently: REST refuses a record and discards a walk, while a frame
// wsx cannot classify is one it will not DELIVER, which quarantines a book.
//
// `wsx.InspectFrame` is used rather than the full session state machine because
// it is the pure part -- raw bytes in, classification out, no socket -- and it
// is where the granularity and decode refusals live.
type tapeReport struct {
	Path      string `json:"path"`
	Frames    int    `json:"frames"`
	Truncated bool   `json:"truncated_tape"`
	// NoFrame is envelopes that carried no frame bytes at all.
	NoFrame int            `json:"envelopes_without_frame"`
	Kinds   map[string]int `json:"frame_kinds"`
	// Undeliverable is the count wsx would refuse to pass to core. Non-zero
	// means a book would have been quarantined on real recorded traffic.
	Undeliverable int `json:"undeliverable"`
	// Granularity is H-CO-3a: a resting book price that is not an exact integer
	// cent. The comment at frame.go:62-66 calls this "the exchange changing its
	// tick size", which would invalidate every price comparison in the harness.
	Granularity int            `json:"granularity_violations"`
	Anomalies   map[string]int `json:"anomaly_classes"`
	Examples    []string       `json:"examples"`
}

func sweepTape(path string, limit int) (*tapeReport, error) {
	tr := &tapeReport{
		Path: path, Kinds: map[string]int{}, Anomalies: map[string]int{},
	}
	stop := fmt.Errorf("frame limit reached")
	n, truncated, err := tape.Frames(path, func(env tape.Envelope) error {
		if limit > 0 && tr.Frames >= limit {
			return stop
		}
		tr.Frames++
		if len(env.M) == 0 {
			// An envelope carrying no frame is not a frame wsx failed to
			// decode. The tape format allows it -- `tape.Envelope` has a
			// `Universe` field alongside `M` -- and pushing it through
			// InspectFrame would manufacture a FRAME_UNDECODABLE that no
			// consumer would ever see. Counted, not judged.
			tr.NoFrame++
			return nil
		}
		info := wsx.InspectFrame(env.M)
		tr.Kinds[info.Kind.String()]++
		if !info.Deliver {
			tr.Undeliverable++
			if len(tr.Examples) < 10 {
				tr.Examples = append(tr.Examples, trim(string(env.M)))
			}
		}
		if info.Granularity {
			tr.Granularity++
		}
		for _, a := range info.Anomalies {
			tr.Anomalies[a.Class]++
		}
		return nil
	})
	// A truncated tape is "still being written", not corrupt (tape.go:45-48),
	// and hitting our own limit is not a failure either.
	if err != nil && err != stop {
		return nil, err
	}
	tr.Frames = n
	tr.Truncated = truncated
	return tr, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func trim(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "conform: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "notes/conform.json", "where to write the JSON report")
	corpus := flag.String("corpus", "", "optional: write every raw record here, as a real-payload fixture corpus")
	sampleMarkets := flag.Int("markets", 200, "how many active markets to read schedules for (0 disables)")
	tapes := flag.String("tapes", "", "comma-separated recorded tapes to push through wsx.InspectFrame")
	tapeLimit := flag.Int("tape-limit", 0, "stop after this many frames per tape (0 = all)")
	doREST := flag.Bool("rest", true, "run the live REST sweep; -rest=false makes this offline-only")
	flag.Parse()

	rep := report{RanAt: time.Now().UTC().Format(time.RFC3339)}
	for _, path := range strings.Split(*tapes, ",") {
		if strings.TrimSpace(path) == "" {
			continue
		}
		fmt.Fprintf(os.Stderr, "replaying %s through wsx.InspectFrame...\n", path)
		tr, terr := sweepTape(strings.TrimSpace(path), *tapeLimit)
		if terr != nil {
			return fmt.Errorf("replaying %s: %w", path, terr)
		}
		rep.Tapes = append(rep.Tapes, *tr)
	}
	if !*doREST {
		blob, merr := json.MarshalIndent(rep, "", "  ")
		if merr != nil {
			return fmt.Errorf("encoding report: %w", merr)
		}
		if werr := os.WriteFile(*out, blob, 0o644); werr != nil {
			return fmt.Errorf("writing %s: %w", *out, werr)
		}
		summarise(rep, *out)
		return nil
	}

	signer, err := feed.NewSigner()
	if err != nil {
		return fmt.Errorf("loading credentials: %w", err)
	}
	doer := getOnly{next: rest.NewHTTPDoer(signer, restTimeout)}
	live := rest.NewClient(doer)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	raw := map[string][]json.RawMessage{}

	for _, p := range probes() {
		fmt.Fprintf(os.Stderr, "walking %s (%s %v)...\n", p.name, p.ep.Path, p.filters)
		filters := p.filters
		if filters == nil {
			filters = url.Values{}
		}
		w := live.Walk(ctx, p.ep, filters)
		er := endpointReport{
			Name: p.name, Path: p.ep.Path, Latent: p.latent,
			Outcome: fmt.Sprintf("%v", w.Outcome), Pages: w.Pages,
		}
		for _, a := range w.Anomalies {
			er.WalkAnoms = append(er.WalkAnoms, a.Class+": "+a.Text)
		}
		if w.Err != nil {
			er.WalkErr = w.Err.Error()
		}
		if !w.Replaces() {
			// A walk that does not complete carries no records by design
			// (page.go's Items comment), so there is nothing to isolate.
			rep.Endpoints = append(rep.Endpoints, er)
			continue
		}

		recs := w.Records(p.itemKey)
		er.Records = len(recs)
		raw[p.name] = recs
		if p.latent {
			// A latent probe's job is to characterise the shapes the decoder
			// would meet, not to enumerate 22,000 copies of the same one.
			er.Shapes = shapeHisto(recs)
		}

		// Phase 2. Every record gets a verdict, and one failure never hides
		// the next -- which is the whole reason this program exists.
		for i, rec := range recs {
			body, merr := synthPage(p, rec)
			if merr != nil {
				return fmt.Errorf("%s record %d: building synthetic page: %w",
					p.name, i, merr)
			}
			if derr := p.decode(ctx, rest.NewClient(onePage{body: body})); derr != nil {
				er.Failed++
				// Count every refusal, keep a bounded sample of the records.
				// A latent probe can refuse tens of thousands of records that
				// are all the same shape, and an unbounded report would be
				// mostly duplicate evidence.
				if er.Failed <= failureSamples {
					rep.Failures = append(rep.Failures, recordFailure{
						Endpoint: p.name, Index: i,
						Reason: derr.Error(), Record: rec,
					})
				}
			}
		}

		// And the batch verdict, for the record: this is what production sees.
		// It should be exactly "failed iff any record failed", and if it ever
		// is not, the batch boundary itself has a defect worth knowing about.
		full, ferr := fullWalkVerdict(ctx, p, recs)
		er.FullWalkOK = ferr == nil && full
		if ferr != nil {
			er.FullWalkNo = ferr.Error()
		}
		rep.Endpoints = append(rep.Endpoints, er)
	}

	rep.Fees = analyseFees(raw["fills"])
	rep.Prices = analysePrices(raw["fills"], raw["orders"])

	bal, berr := live.Balance(ctx)
	if berr != nil {
		rep.Balance = "REFUSED: " + berr.Error()
	} else {
		rep.Balance = fmt.Sprintf("%d cents decoded cleanly", bal.Cents)
	}

	if *sampleMarkets > 0 {
		// Sample from the UNFILTERED programme set when it was walked. The
		// active set is all live markets and therefore all `status: "active"`,
		// which tells us nothing about the vocabulary a market passes through
		// as it closes -- and the refusing branch of deriveTradingClosed is
		// reachable only from those other statuses.
		from := raw["programs.unfiltered"]
		if len(from) == 0 {
			from = raw["programs"]
		}
		rep.Markets = sweepMarkets(ctx, live, from, *sampleMarkets)
	}

	if *corpus != "" {
		if err := writeCorpus(*corpus, raw); err != nil {
			return fmt.Errorf("writing corpus: %w", err)
		}
	}

	blob, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", *out, err)
	}
	summarise(rep, *out)
	return nil
}

// fullWalkVerdict replays the WHOLE record set through the decoder in one page,
// reproducing production's batch boundary.
func fullWalkVerdict(ctx context.Context, p probe, recs []json.RawMessage) (bool, error) {
	page := map[string]any{p.ep.CursorField: "", p.itemKey: recs}
	for _, sib := range p.siblings {
		page[sib] = []json.RawMessage{}
	}
	body, err := json.Marshal(page)
	if err != nil {
		return false, err
	}
	if derr := p.decode(ctx, rest.NewClient(onePage{body: body})); derr != nil {
		return false, derr
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Field-level measurements
// ---------------------------------------------------------------------------

func analyseFees(fills []json.RawMessage) *feeReport {
	if len(fills) == 0 {
		return nil
	}
	fr := &feeReport{Fills: len(fills), DecimalsHisto: map[string]int{}}
	seen := map[string]bool{}
	refused := map[string]bool{}
	for _, rec := range fills {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec, &m); err != nil {
			continue
		}
		fee := scalar(m["fee_cost"])
		taker := string(m["is_taker"]) == "true"
		if taker {
			fr.IsTakerTrue++
		}
		if fee == "" {
			continue
		}
		if !seen[fee] {
			seen[fee] = true
			fr.DistinctFees = append(fr.DistinctFees, fee)
		}
		decimals := 0
		if _, frac, ok := strings.Cut(fee, "."); ok {
			decimals = len(frac)
		}
		fr.DecimalsHisto[fmt.Sprintf("%d", decimals)]++
		if decimals > fr.MaxDecimals {
			fr.MaxDecimals = decimals
		}
		if _, err := rest.ParsePrice4(fee); err != nil && !refused[fee] {
			refused[fee] = true
			fr.Unparseable = append(fr.Unparseable, fee)
		}
		// A fee that is present and not all-zero is a non-zero fee. Compared
		// as text so this measurement does not depend on the very parser the
		// sweep is here to judge.
		if strings.Trim(strings.ReplaceAll(fee, ".", ""), "0") != "" {
			fr.NonZeroFee++
			if !taker {
				fr.WouldTripTaker++
			}
		}
	}
	// Every taker fill trips the rule too; count them once.
	fr.WouldTripTaker += fr.IsTakerTrue
	sort.Strings(fr.DistinctFees)
	sort.Strings(fr.Unparseable)
	return fr
}

func analysePrices(sets ...[]json.RawMessage) *priceReport {
	pr := &priceReport{}
	for _, set := range sets {
		for _, rec := range set {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(rec, &m); err != nil {
				continue
			}
			yes, no := scalar(m["yes_price_dollars"]), scalar(m["no_price_dollars"])
			if yes == "" || no == "" {
				continue
			}
			pr.RecordsWithBothFields++
			y, yerr := rest.ParsePrice4(yes)
			n, nerr := rest.ParsePrice4(no)
			if yerr != nil || nerr != nil || int64(y)+int64(n) != 10000 {
				pr.Violations++
				if len(pr.Examples) < 20 {
					pr.Examples = append(pr.Examples,
						fmt.Sprintf("yes=%s no=%s", yes, no))
				}
			}
		}
	}
	return pr
}

// sweepMarkets reads real schedules, which is where `deriveTradingClosed`'s
// contradiction refusal (`status=="active"` with a non-empty `result`) and
// `parseWireTime`'s strict RFC3339 rule meet real markets.
func sweepMarkets(ctx context.Context, c *rest.Client,
	programs []json.RawMessage, limit int) *marketReport {

	tickers := make([]string, 0, len(programs))
	for _, rec := range programs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec, &m); err != nil {
			continue
		}
		if t := scalar(m["market_ticker"]); t != "" {
			tickers = append(tickers, t)
		}
	}
	sort.Strings(tickers)
	tickers = dedupe(tickers)
	// Stride rather than truncate. The list is sorted, so taking the first N
	// samples one alphabetical neighbourhood -- which for Kalshi tickers means
	// one event series, on one day, in one state.
	if limit > 0 && len(tickers) > limit {
		stride := len(tickers) / limit
		spread := make([]string, 0, limit)
		for i := 0; i < len(tickers) && len(spread) < limit; i += stride {
			spread = append(spread, tickers[i])
		}
		tickers = spread
	}

	mr := &marketReport{Sampled: len(tickers), Statuses: map[string]int{}}
	reasons := map[string]bool{}
	for i, t := range tickers {
		if i%25 == 0 {
			fmt.Fprintf(os.Stderr, "  schedules %d/%d\n", i, len(tickers))
		}
		r := c.Schedule(ctx, t)
		if !r.Observed() {
			mr.Failed++
			if r.Err != nil && !reasons[r.Err.Error()] {
				reasons[r.Err.Error()] = true
				mr.Reasons = append(mr.Reasons, r.Err.Error())
			}
			continue
		}
		mr.Observed++
		key := r.Status
		if r.Result == "" {
			key += " / no result yet"
		} else {
			key += " / has result"
		}
		mr.Statuses[key]++
		if r.Status == "active" && r.Result != "" {
			mr.ActiveWithResult++
		}
		if !r.HasClose {
			mr.NoCloseTime++
		}
		for _, a := range r.Anomalies {
			key := a.Class + ": " + a.Text
			if !reasons[key] {
				reasons[key] = true
				mr.Reasons = append(mr.Reasons, key)
			}
		}
	}
	sort.Strings(mr.Reasons)
	return mr
}

// scalar unwraps a JSON string or number to its text, and returns "" for an
// absent or null field. It deliberately mirrors `rest`'s own `scalar`.
func scalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// writeCorpus saves the real payloads the harness actually reads, so the
// decoders can finally be tested against records the exchange wrote rather than
// records their own author wrote. The latent probe is excluded: 137,569
// programme records are not a fixture, they are a download.
func writeCorpus(path string, raw map[string][]json.RawMessage) error {
	keep := make(map[string][]json.RawMessage, len(raw))
	for name, recs := range raw {
		if name == "programs.unfiltered" {
			continue
		}
		keep[name] = recs
	}
	blob, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o644)
}

func summarise(rep report, out string) {
	fmt.Printf("\n===CONFORM===\n")
	live, latent := 0, 0
	for _, e := range rep.Endpoints {
		status := "ok"
		if e.Failed > 0 {
			status = fmt.Sprintf("%d REFUSED", e.Failed)
			if e.Latent {
				status += " (LATENT: wider than production)"
				latent += e.Failed
			} else {
				live += e.Failed
			}
		}
		if e.WalkErr != "" {
			status = "WALK FAILED: " + e.WalkErr
		}
		fmt.Printf("%-20s %-24s pages=%-3d records=%-6d %s\n",
			e.Name, e.Path, e.Pages, e.Records, status)
		for shape, n := range e.Shapes {
			fmt.Printf("%-20s   %6d  %s\n", "", n, shape)
		}
	}
	fmt.Printf("\nrefusals: %d against what production reads, %d only beyond its filter\n",
		live, latent)
	if f := rep.Fees; f != nil {
		fmt.Printf("\nfee_cost over %d fills: max %d decimals, histogram %v\n",
			f.Fills, f.MaxDecimals, f.DecimalsHisto)
		fmt.Printf("  ParsePrice4 refuses %d distinct value(s): %v\n",
			len(f.Unparseable), f.Unparseable)
		fmt.Printf("  is_taker=true %d, non-zero fee %d, would trip TAKER_FILL %d\n",
			f.IsTakerTrue, f.NonZeroFee, f.WouldTripTaker)
	}
	if p := rep.Prices; p != nil {
		fmt.Printf("\ncomplement invariant: %d record(s) carry both price fields, %d violate\n",
			p.RecordsWithBothFields, p.Violations)
	}
	if m := rep.Markets; m != nil {
		fmt.Printf("\nschedules: sampled %d, observed %d, failed %d, active-with-result %d, no close_time %d\n",
			m.Sampled, m.Observed, m.Failed, m.ActiveWithResult, m.NoCloseTime)
		for k, v := range m.Statuses {
			fmt.Printf("  %6d  status %s\n", v, k)
		}
		for _, r := range m.Reasons {
			fmt.Printf("  %s\n", r)
		}
	}
	for _, t := range rep.Tapes {
		fmt.Printf("\ntape %s: %d frames%s\n", t.Path, t.Frames,
			map[bool]string{true: " (tape truncated: still being written)"}[t.Truncated])
		fmt.Printf("  kinds %v\n", t.Kinds)
		fmt.Printf("  undeliverable %d, granularity violations %d\n",
			t.Undeliverable, t.Granularity)
		if len(t.Anomalies) > 0 {
			fmt.Printf("  anomalies %v\n", t.Anomalies)
		}
		for _, e := range t.Examples {
			fmt.Printf("    %s\n", e)
		}
	}
	if rep.Balance != "" {
		fmt.Printf("\nbalance: %s\n", rep.Balance)
	}
	fmt.Printf("full report at %s\n", out)
}

// confidence: high

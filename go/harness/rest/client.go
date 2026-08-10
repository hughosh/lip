package rest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"lip/feed"
)

// ---------------------------------------------------------------------------
// The transport seam
// ---------------------------------------------------------------------------

// Request is one signed call. Path is the API path WITHOUT the `/trade-api/v2`
// prefix and WITHOUT the query string, because that is exactly what the
// signature covers (`kalshi.py:95` — "the signature covers the path WITHOUT the
// query string"). Keeping Query separate from Path means a caller cannot
// accidentally sign a query and get a 401 that looks like a credential problem.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// Response is an answer the exchange actually gave us.
type Response struct {
	Status int
	Body   []byte
}

// Doer is the whole I/O surface of this package, and the seam the scenario
// exchange (pilot-plan §3) substitutes at.
//
// **The seam is deliberately at the HTTP level — status code and raw bytes —
// rather than at a typed `Orders() ([]Order, error)` level.** Every failure this
// package exists to survive is invisible above that line:
//
//   - H-PAGE-1a's rewind is an HTTP 200 carrying page one again. A typed fake
//     would return the right records and the guard would never be exercised.
//   - V1.8c's cursor-field-name trap (M22) is a JSON key. A typed fake has no
//     JSON.
//   - H-ORD-2b's 409 `order_already_exists` is a status code that must read as
//     positive identification rather than as an error.
//   - V1.7a's "ACKs then drops the response" is the *absence* of a Response.
//
// A seam drawn above the wire would make the tests pass by construction while
// leaving all four bugs reachable in production.
//
// **The error/Response split is the load-bearing part of this signature.** A
// non-2xx status is a `Response`, never an `error`: the exchange answered, and
// H-ORD-2 turns on whether it answered. `error` is returned only when the
// outcome is genuinely unknown — timeout, connection reset, a body that cannot
// be read. That is the distinction between `REJECTED` (definite, 4xx) and
// `UNKNOWN` (ambiguous), and encoding it in the type means a caller cannot
// collapse the two by writing `if err != nil`.
type Doer interface {
	Do(ctx context.Context, req Request) (Response, error)
}

// NotSent wraps an error for a request that PROVABLY never reached the
// exchange: it could not be constructed, or it could not be signed.
//
// This exists because "error" would otherwise mean two different things, and
// only one of them is dangerous. A request that never left the process cannot
// have created an order, so classifying it as `UNKNOWN` would be false
// caution with a real cost: an unresolved create keeps its full size in every
// aggregate cap (H-ORD-2 clause 6), so a broken signer or a malformed URL would
// silently consume the pilot's capital budget with orders that do not exist and
// never did. A definite "no" is available here, and withholding it is not
// conservative, just wrong.
//
// It is deliberately narrow. Anything that touched the network — a timeout, a
// reset, a 5xx, a truncated body — is NOT this, because the exchange may have
// acted before we lost the answer.
type NotSent struct{ Err error }

func (e *NotSent) Error() string { return "not sent: " + e.Err.Error() }
func (e *NotSent) Unwrap() error { return e.Err }

// WasSent reports whether a Doer error leaves the request's outcome ambiguous.
// True means "we do not know what happened" — H-ORD-2's `UNKNOWN`.
//
// `WriteRefused` joins `NotSent` here (H-VER-1). A guarded refusal happens
// BEFORE the wrapped Doer is called, so it is the strongest possible "no": there
// was no request, not a request whose answer we lost. Reading it as ambiguous
// would make a read-only rehearsal manufacture `UNKNOWN` orders, and an
// unresolved create keeps its full size in every aggregate cap (H-ORD-2 clause
// 6) — so the process that is provably not trading would exhaust the pilot's
// capital budget with orders that never existed.
func WasSent(err error) bool {
	var ns *NotSent
	if errors.As(err, &ns) {
		return false
	}
	var wr *WriteRefused
	return !errors.As(err, &wr)
}

// ---------------------------------------------------------------------------
// V1.8c — the endpoint constants table
// ---------------------------------------------------------------------------

// Endpoint pins everything about a paginated read that `notes/pagecontract.md`
// measured and that cannot be inferred from the response.
//
// It exists as data rather than as code at each call site because the whole
// finding of V1.8a was that **there is no single contract** and this repository
// already held both halves of the answer while believing each was universal:
// `probebot.py:1123` reads `next_cursor`, `scripts/opportunity.py:54` reads
// `cursor`. Each is correct for its own endpoint and silently wrong for the
// other, and the wrong one does not error — it reports an empty cursor, so the
// walker concludes it finished and returns page one as the whole answer.
type Endpoint struct {
	// Path is the API path after the `/trade-api/v2` prefix.
	Path string
	// CursorField is the JSON key carrying the next cursor. PER-ENDPOINT.
	CursorField string
	// ItemKeys are the JSON keys carrying records. `/portfolio/positions`
	// returns TWO arrays under ONE cursor, which is why this is a slice.
	ItemKeys []string
	// IDFields maps an item key to the field that uniquely identifies a record
	// within a walk. It is what H-PAGE-1a clause 2's forward-progress guard
	// compares. A key with no entry falls back to the record's own bytes.
	//
	// For fills this is `fill_id`, NOT `trade_id`. Both are present and V1.8
	// records them as two distinct identities; `trade_id` is the join key
	// against `rig.db` (H-ORD-6) and is the one the public trade stream also
	// carries, so it is not guaranteed unique per fill row.
	IDFields map[string]string
	// MinLimit and MaxLimit bound `limit`. Outside them the portfolio family
	// returns HTTP 400 (pagecontract §3), so we refuse to send it rather than
	// discover it live.
	MinLimit, MaxLimit int
	// PageLimit is the limit this harness actually sends.
	PageLimit int
	// MaxPages is a last-resort circuit breaker, not a page cap. Exceeding it
	// ABANDONS the walk and returns no answer; it never truncates one. See the
	// comment at its use site.
	MaxPages int
}

// The measured table. `notes/pagecontract.md` §0 and §3.
var (
	EpPositions = Endpoint{
		Path:        "/portfolio/positions",
		CursorField: "cursor",
		// Both arrays, under one cursor. pagecontract §6.2: whether they
		// paginate together is UNKNOWN and untestable at one row each, so the
		// walk must not terminate on market_positions alone going empty.
		ItemKeys: []string{"market_positions", "event_positions"},
		// `event_positions` has no measured identity field, so it falls back to
		// record bytes. The harness reads `market_positions` for q.
		IDFields: map[string]string{"market_positions": "ticker"},
		// positions accepted 0 and 5000 without complaint, so its bounds are
		// unestablished (pagecontract §6.1). We stay inside the range its
		// siblings enforce rather than rely on the leniency.
		MinLimit: 1, MaxLimit: 1000, PageLimit: 200, MaxPages: 1000,
	}
	EpOrders = Endpoint{
		Path:        "/portfolio/orders",
		CursorField: "cursor",
		ItemKeys:    []string{"orders"},
		IDFields:    map[string]string{"orders": "order_id"},
		MinLimit:    1, MaxLimit: 1000, PageLimit: 1000, MaxPages: 1000,
	}
	EpFills = Endpoint{
		Path:        "/portfolio/fills",
		CursorField: "cursor",
		ItemKeys:    []string{"fills"},
		IDFields:    map[string]string{"fills": "fill_id"},
		MinLimit:    1, MaxLimit: 1000, PageLimit: 1000, MaxPages: 1000,
	}
	// EpPrograms is the odd one out and the reason this table exists.
	EpPrograms = Endpoint{
		Path:        "/incentive_programs",
		CursorField: "next_cursor",
		ItemKeys:    []string{"incentive_programs"},
		// No measured identity field: the programs payload was not sampled by
		// V1.8a at record level, so this key falls back to record bytes rather
		// than guessing a field that may not exist.
		//
		// No upper bound observed; limit=5000 returned all 4,170 in one page.
		// We still walk: "no bound observed" is not "no bound".
		MinLimit: 1, MaxLimit: 5000, PageLimit: 1000, MaxPages: 1000,
	}
)

// ValidateLimit refuses a limit the endpoint would answer with HTTP 400.
func (e Endpoint) ValidateLimit(limit int) error {
	if limit < e.MinLimit || limit > e.MaxLimit {
		return fmt.Errorf("limit %d is outside %s's measured range %d..%d",
			limit, e.Path, e.MinLimit, e.MaxLimit)
	}
	return nil
}

// ---------------------------------------------------------------------------
// H-PAGE-1a clause 4 — status strings are pinned, because a typo reads as empty
// ---------------------------------------------------------------------------

// The exchange does not reject an unrecognised `status`; it answers "nothing".
// Measured on this account: `status=cancelled` returned 0 rows and
// `status=canceled` returned 2. That is "absence is not evidence" (H-ORD-2a)
// reappearing as a spelling mistake, and it is silent.
//
// US spelling. One `l`. The double-l spelling is the natural one to type and is
// the one that returns an empty result forever.
const (
	StatusResting  = "resting"
	StatusCanceled = "canceled"
	StatusExecuted = "executed"
)

// validStatuses is every filter value this harness is permitted to send.
var validStatuses = map[string]bool{
	StatusResting:  true,
	StatusCanceled: true,
	StatusExecuted: true,
}

// ValidateStatus rejects a status string the exchange would silently answer
// with an empty list. It is called on the send path, not just in tests: a
// misspelling that reaches the wire is indistinguishable from a real absence,
// and an absence is what H-ORD-2a spent a whole section refusing to trust.
func ValidateStatus(status string) error {
	if status == "" {
		return nil // unfiltered is legitimate: 9 = 2 canceled + 7 executed
	}
	if !validStatuses[status] {
		return fmt.Errorf("status %q is not a measured filter value: the "+
			"endpoint does not reject an unknown status, it answers "+
			"'nothing', so this would read as an empty result forever "+
			"(pagecontract §6.5). Valid: %q, %q, %q",
			status, StatusResting, StatusCanceled, StatusExecuted)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The live transport
// ---------------------------------------------------------------------------

// APIPrefix is what the signature and the URL are both built over.
const APIPrefix = "/trade-api/v2"

// HTTPDoer is the production Doer: net/http plus feed.Signer.
//
// It has no retry loop. Retry policy is H-ORD-2's business and depends on
// whether the write was a create or a cancel (H-ORD-4a), which is a distinction
// this layer cannot see. A transport that quietly retried would convert a
// resolvable `UNKNOWN` into a duplicate order.
type HTTPDoer struct {
	Client *http.Client
	Signer *feed.Signer
	Host   string
	// Now is injected so the signature timestamp is testable and so no clock
	// read hides inside the I/O path.
	Now func() time.Time
}

// NewHTTPDoer builds the live transport against the real exchange, on Go's
// default transport. Production does NOT use this -- see
// `NewHTTPDoerWithTransport` -- but component tests do, and a default that
// works is what keeps them from having to compose a resolver.
func NewHTTPDoer(signer *feed.Signer, timeout time.Duration) *HTTPDoer {
	return newHTTPDoer(signer, timeout, nil)
}

// NewHTTPDoerWithTransport is the production constructor (F6).
//
// The transport carries `netx.CachedDialer`, so every REST call resolves
// through the one-hour last-known-good cache and survives the system resolver
// wedging. It is a SEPARATE `http.Transport` from the websocket's even though
// both share one dialer: H-FAIL-2 requires the two to be distinct transports,
// because a connection-pool fault that took out one must not take out the
// other -- REST is how cancels still reach the exchange when the feed is gone.
//
// `M-7ZT-RESTBYPASS` builds production REST on the default transport, which
// leaves F6 implemented, tested, and on no path the harness uses.
func NewHTTPDoerWithTransport(signer *feed.Signer, timeout time.Duration,
	rt http.RoundTripper) *HTTPDoer {

	return newHTTPDoer(signer, timeout, rt)
}

func newHTTPDoer(signer *feed.Signer, timeout time.Duration,
	rt http.RoundTripper) *HTTPDoer {

	return &HTTPDoer{
		Client: &http.Client{
			Transport: rt,
			Timeout:   timeout,
			// Redirects are NOT followed. `net/http` follows them by default,
			// and for a write that is a duplicate-order hazard: a 307 or 308
			// preserves the method and the body, so a redirected create is a
			// SECOND create — issued to a host we did not sign for, and
			// invisible to H-ORD-2b's same-coid recovery because we would only
			// ever see the final response.
			//
			// ErrUseLastResponse surfaces the 3xx as an ordinary Response.
			// `Create` then classifies it under its default branch as UNKNOWN,
			// which is correct: a redirect we refused to follow leaves the
			// order's existence genuinely undetermined.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Signer: signer,
		Host:   feed.RestHost,
		Now:    time.Now,
	}
}

// Do issues one signed request.
//
// Every return path is either a Response (the exchange answered, whatever it
// said) or an error (we do not know what happened). Nothing in between.
func (d *HTTPDoer) Do(ctx context.Context, req Request) (Response, error) {
	if d.Signer == nil {
		// Guarded rather than left to panic. feed.Signer.Headers dereferences
		// its key, so a nil signer takes the process down — and this is the
		// write path, so the process it takes down is the one holding
		// inventory and running the monitor. An unsendable request is a
		// definite "nothing was sent"; a panic is an abandoned position.
		return Response{}, &NotSent{Err: fmt.Errorf("no signer configured")}
	}
	fullPath := APIPrefix + req.Path
	u := d.Host + fullPath
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}

	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		// A malformed request never left the process, so its outcome is not
		// ambiguous -- but the caller cannot tell that from an error value
		// alone, so say it.
		return Response{}, &NotSent{Err: err}
	}

	// The signature covers the prefixed path WITHOUT the query string.
	headers, err := d.Signer.Headers(d.Now().UnixMilli(), req.Method, fullPath)
	if err != nil {
		return Response{}, &NotSent{Err: err}
	}
	for k, v := range headers {
		hr.Header.Set(k, v)
	}
	if req.Body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.Client.Do(hr)
	if err != nil {
		// Timeout, connection reset, DNS. The request may or may not have been
		// executed by the exchange. This is exactly H-ORD-2's `UNKNOWN`.
		return Response{}, fmt.Errorf("%s %s: no answer received: %w",
			req.Method, req.Path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// We have a status code but a truncated body. The exchange did act, and
		// for a create that means an order may exist -- so this is `UNKNOWN`
		// too, not a status we can classify.
		return Response{}, fmt.Errorf("%s %s: HTTP %d with an unreadable body: %w",
			req.Method, req.Path, resp.StatusCode, err)
	}
	return Response{Status: resp.StatusCode, Body: raw}, nil
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client is the harness's exchange surface. It holds no mutable state: every
// read returns a fresh result and every write is classified by its caller, so
// two Clients over the same Doer cannot disagree about anything.
type Client struct {
	Doer Doer
}

// NewClient wraps a Doer.
func NewClient(d Doer) *Client { return &Client{Doer: d} }

// get issues a paginated-endpoint GET for one page.
func (c *Client) get(ctx context.Context, ep Endpoint, q url.Values) (Response, error) {
	return c.Doer.Do(ctx, Request{Method: "GET", Path: ep.Path, Query: q})
}

// pageQuery builds one page's query: the caller's filters, the endpoint's
// limit, and the cursor exactly as received.
//
// H-PAGE-1a clause 1: a cursor is used exactly as received or the walk is
// abandoned. It is never repaired, truncated, re-encoded, or carried across
// endpoints -- the portfolio family answers HTTP 200 with page one to all four
// of those, so a "fixed up" cursor does not fail, it silently rewinds.
func pageQuery(ep Endpoint, filters url.Values, cursor string) (url.Values, error) {
	if err := ep.ValidateLimit(ep.PageLimit); err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, vs := range filters {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("limit", strconv.Itoa(ep.PageLimit))
	if cursor != "" {
		// The REQUEST parameter is `cursor` on both families. Only the RESPONSE
		// field name differs (`cursor` vs `next_cursor`), which is why
		// Endpoint.CursorField governs the read and not the write.
		// `probebot.py:1113-1123` is the worked example: it sends `&cursor=` to
		// `/incentive_programs` and reads `next_cursor` back. Making this
		// symmetric in either direction breaks one family silently.
		q.Set("cursor", cursor)
	}
	return q, nil
}

// confidence: high

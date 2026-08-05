package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"lip/core"
)

// urlopen(..., timeout=20) in score.py:22.
const restTimeout = 20 * time.Second

// socketTimeoutConn gives every Read and Write its own 20-second deadline,
// refreshed per call. That is what a Python socket timeout does; a deadline set
// once would be a total-transfer limit, which is a different thing.
type socketTimeoutConn struct{ net.Conn }

func (c *socketTimeoutConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(restTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *socketTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(restTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// Universe returns the active LIP markets and their Target Sizes.
// Port of rig.py:630-633 via score.py:26-27.
//
// The endpoint is public, so no signature is attached — the only credential use
// in this rig is the websocket handshake, which Kalshi requires even for public
// channels.
//
// `limit=200` is carried across from the Python verbatim, and there is no
// pagination follow-up there either. The response currently returns exactly 200
// programs, so the cap is live rather than theoretical; both implementations
// truncate at the same place, which is what the differential comparison needs.
// Widening it would change which markets are subscribed and is Phase B work.
// The returned slice is the tickers in RESPONSE order, deduplicated on first
// sight. Python's universe() builds a dict, and `list(self.books)` at
// rig.py:412 and 531 is therefore REST order — which is what reaches the
// subscribe and resnapshot payloads, and what the tape header records. Go map
// range is randomized and sorting is a divergence, so the order is carried
// explicitly.
func Universe(ctx context.Context) (map[string]float64, []string, error) {
	return universeFrom(ctx, RestHost+"/trade-api/v2/incentive_programs?status=active&limit=200")
}

func universeFrom(ctx context.Context, url string) (map[string]float64, []string, error) {
	// urlopen(timeout=20) sets the timeout on the SOCKET, so it bounds every
	// individual recv — including body reads — and each one refreshes it. A
	// total request deadline would abort a slow but progressing response that
	// Python completes; dial and header timeouts alone would let a trickling
	// body hang forever where Python times out.
	//
	// Wrapping the connection so each Read and Write pushes the deadline out by
	// 20s reproduces the socket semantics exactly.
	dialer := &net.Dialer{Timeout: restTimeout}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &socketTimeoutConn{Conn: c}, nil
		},
		TLSHandshakeTimeout: restTimeout,
	}}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("incentive_programs: HTTP %d", resp.StatusCode)
	}

	var body struct {
		Programs []struct {
			MarketTicker string `json:"market_ticker"`
			TargetSizeFP string `json:"target_size_fp"`
		} `json:"incentive_programs"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&body); err != nil {
		return nil, nil, err
	}
	// json.load() consumes the whole stream and raises "Extra data" on anything
	// after the first value. Decoder.More() is NOT sufficient: it answers "is
	// there another element in the enclosing array or object", so it returns
	// false when the next byte is `}` or `]` — `{"incentive_programs":[…]}}`
	// sails straight through it. A second Decode returning exactly io.EOF is the
	// real check.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, nil, fmt.Errorf("incentive_programs: extra data after the JSON value")
	}

	out := make(map[string]float64, len(body.Programs))
	order := make([]string, 0, len(body.Programs))
	for _, p := range body.Programs {
		// target_size_fp is a decimal STRING ("1000.00"). Parsing it with the
		// same correctly-rounded parser the wire sizes use keeps the target
		// bit-identical to Python's float(), which matters because Qualifies()
		// compares an accumulated float against it with >=.
		t, err := core.ParseSize(p.TargetSizeFP)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p.MarketTicker, err)
		}
		// A repeated ticker takes the LAST program's target but keeps its FIRST
		// position, which is CPython dict-assignment semantics — the same rule
		// as P22.
		if _, seen := out[p.MarketTicker]; !seen {
			order = append(order, p.MarketTicker)
		}
		out[p.MarketTicker] = t
	}
	return out, order, nil
}

// confidence: high

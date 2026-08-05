package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// Subscribe payloads. Port of rig.py:411-421. See port-spec.md P27.
//
// The asymmetry is deliberate and is NOT a tidy-up opportunity: orderbook_delta
// is filtered to our universe, `trade` is not filtered at all. One `trade`
// subscription yields the whole exchange tape, which core.Rig filters down —
// discarding roughly 98.6% of decoded trade frames, which is the standing sanity
// check that the subscription is what we think it is. Adding a market filter
// would change no `fill` row and would silently stop the capture being a
// whole-exchange capture.
func subscribeDelta(tickers []string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": 1, "cmd": "subscribe",
		"params": map[string]any{
			"channels":       []string{"orderbook_delta"},
			"market_tickers": tickers,
		},
	})
}

func subscribeTrade() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": 2, "cmd": "subscribe",
		"params": map[string]any{"channels": []string{"trade"}},
	})
}

func resnapshotRequest(sids []int64, tickers []string) ([]byte, error) {
	if sids == nil {
		sids = []int64{}
	}
	return json.Marshal(map[string]any{
		"id": 3, "cmd": "update_subscription",
		"params": map[string]any{
			"sids":           sids,
			"action":         "get_snapshot",
			"market_tickers": tickers,
		},
	})
}

// HandshakeError is a handshake the server ANSWERED and that then failed
// validation — a rejected status, or a 101 whose upgrade headers,
// Sec-WebSocket-Accept, extensions or subprotocol are wrong.
//
// It is fatal. Python raises InvalidHandshake for all of these, and
// InvalidHandshake is neither ConnectionClosed nor OSError, so rig.py:548 does
// not catch it and the process exits. Only transport-level failures — the
// OSError family — are retried.
type HandshakeError struct {
	StatusCode int
	Err        error
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("handshake rejected: HTTP %d: %v", e.StatusCode, e.Err)
}

func (e *HandshakeError) Unwrap() error { return e.Err }

// ErrCleanClose reports a close with status 1000 or 1001.
//
// This is NOT a reconnect in Python's sense and must not be treated as one. The
// websockets async iterator catches ConnectionClosedOK and returns, so
// rig.py:505's `async for` ends normally, the `try` completes, and the
// `except (ConnectionClosed, OSError)` block at rig.py:548 never runs. No
// commit, no backoff, and — the part that changes rows — no book reset. See
// port-spec.md P25a.
var ErrCleanClose = errors.New("websocket closed normally")

// IsCleanClose reports whether err is a normal-closure end of stream.
func IsCleanClose(err error) bool {
	if errors.Is(err, ErrCleanClose) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return false
}

// Conn is one live websocket session. It is created by Dial, used until Read
// returns an error, then discarded — reconnect makes a new one, because a
// reconnect also has to reset the rig's state (P25) and reusing the object
// would invite forgetting that.
type Conn struct {
	ws *websocket.Conn
}

// Dial opens the socket, performs the signed handshake and sends both
// subscriptions.
//
// No client-side keepalive is configured, matching Python's
// `ping_interval=None`: the server drives the heartbeat, and both libraries
// answer server pings inside their read path regardless. Nor is there a read
// deadline. That is a faithful reproduction of a real weakness — a half-open
// socket stalls both rigs — and introducing one here would make Go reconnect
// where Python hangs, which is exactly the kind of divergence the shadow run is
// supposed to be able to interpret.
//
// nowMs is the caller's clock, so the signer stays clock-free.
func Dial(ctx context.Context, s *Signer, nowMs int64, tickers []string) (*Conn, error) {
	hdr, err := s.WSHeaders(nowMs)
	if err != nil {
		// A signing failure is FATAL, not reconnectable. Python catches only
		// ConnectionClosed and OSError around the connect; a cryptography error
		// propagates and stops the process. Retrying it would have Go reconnect
		// and keep writing rows after Python had exited.
		return nil, &HandshakeError{StatusCode: 0, Err: err}
	}
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}

	// websockets' open_timeout defaults to 10 seconds and bounds the HANDSHAKE
	// only, never the session. Applying it to the session context instead would
	// disconnect us every ten seconds.
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(dialCtx, WSURL, &websocket.DialOptions{
		HTTPHeader: h,
		// websockets negotiates permessage-deflate by default;
		// coder/websocket does not. That changes both the handshake's
		// Sec-WebSocket-Extensions field and delivery timing.
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		// Python catches only ConnectionClosed and OSError around the connect.
		// A server that ANSWERS and then fails validation — 401 on a bad
		// signature, 403, 429, or a 101 with broken upgrade headers — raises
		// InvalidHandshake and kills the rig outright, while a network-level
		// failure is retried. Preserve that split: retrying a 401 for seven days
		// is the worst possible response to it.
		//
		// The test is `resp != nil`, not the status code: a malformed 101 also
		// carries a response, and Python treats it as fatal too.
		if resp != nil {
			return nil, &HandshakeError{StatusCode: resp.StatusCode, Err: err}
		}
		return nil, err
	}
	// websockets' max_size default. A larger frame kills Python's connection,
	// so accepting one here would let Go tape and handle a message Python never
	// sees.
	ws.SetReadLimit(1 << 20)

	c := &Conn{ws: ws}
	delta, err := subscribeDelta(tickers)
	if err != nil {
		c.Close()
		return nil, err
	}
	trade, err := subscribeTrade()
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := c.write(ctx, delta); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.write(ctx, trade); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) write(ctx context.Context, b []byte) error {
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// Resnapshot asks the exchange to re-send a snapshot for every market on every
// live subscription. rig.py:527-532.
func (c *Conn) Resnapshot(ctx context.Context, sids []int64, tickers []string) error {
	b, err := resnapshotRequest(sids, tickers)
	if err != nil {
		return err
	}
	return c.write(ctx, b)
}

func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

// Read blocks for the next frame.
//
// ctx must NOT carry a per-read deadline: coder/websocket fails the whole
// connection when a Read context expires, so a deadline here would be a
// disconnect, not a timeout.
func (c *Conn) Read(ctx context.Context) ([]byte, error) {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText && typ != websocket.MessageBinary {
		return nil, fmt.Errorf("unexpected frame type %v", typ)
	}
	return data, nil
}

// Pump reads frames onto `out` until the socket fails, then reports why.
//
// It runs on its own goroutine so that the goroutine owning the books never
// blocks on the network: that owner selects over this channel and its timers,
// which is what reproduces asyncio's single-threaded model.
//
// It does NOT write the tape. Python tapes the frame it is about to handle,
// immediately before handling it (rig.py:505-523), so taping here would let the
// pump run thousands of frames ahead of the owner: on shutdown the tape would
// contain frames that were never handled, and replaying it would produce rows
// the live run never wrote. The owner does the tape write. See port-spec.md P26.
//
// `out` is closed on exit, so the owner's receive-with-ok is a reliable
// disconnect signal.
func (c *Conn) Pump(ctx context.Context, out chan<- []byte) error {
	defer close(out)
	for {
		data, err := c.Read(ctx)
		if err != nil {
			if IsCleanClose(err) {
				return ErrCleanClose
			}
			return err
		}
		select {
		case out <- data:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// confidence: high

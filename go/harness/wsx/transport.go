package wsx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"lip/feed"
)

// ---------------------------------------------------------------------------
// The seams
// ---------------------------------------------------------------------------

// Signer produces the websocket handshake headers.
//
// It is an interface over `feed.Signer` rather than the concrete type so that
// tests can sign against a locally generated key and a local server, and so
// that nothing here can reach the private key. `nowMs` is passed in because
// the signature is over a timestamp and the clock belongs to the caller.
type Signer interface {
	WSHeaders(nowMs int64) (map[string]string, error)
}

// Socket is one live websocket connection, at the frame level.
//
// Frame level and not message level, deliberately. A typed seam -- one that
// handed the caller decoded snapshots and deltas -- could not express a
// truncated frame, a compression failure, or a close that arrives between a
// ping and its pong, and those are the failures this package exists to survive.
// It is the same argument that put `rest.Doer` at the HTTP level.
//
// Ping is separate from Write because a ping is a control frame: it does not
// queue behind application data and its answer is handled inside the library's
// read path.
type Socket interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, b []byte) error
	Ping(ctx context.Context) error
	Close() error
}

// Dialer opens a Socket. The scenario exchange of pilot-plan.md §3 substitutes
// here.
type Dialer interface {
	Dial(ctx context.Context, url string, h http.Header) (Socket, error)
}

// Stamp is one reading of both clocks, taken together.
//
// Two clocks, because they answer different questions and substituting either
// for the other is a live defect:
//
//   - Mono is monotonic and is the ONLY thing compared for age. A wall-clock
//     step (F21) must not be readable as staleness -- an NTP correction of two
//     minutes would otherwise age out every portfolio endpoint at once and stop
//     all placement, including the reducer's.
//   - WallMs is what the request signature is computed over and what rows and
//     heartbeats carry. It is never compared for freshness.
type Stamp struct {
	WallMs int64
	Mono   time.Duration
}

// Timer is the subset of time.Timer this package uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Clock is the injected clock. Every deadline in this package is measured
// against it, so a test can drive an hour of ping/pong ladder in microseconds
// and a scenario can replay a disconnect at an exact offset.
type Clock interface {
	Now() Stamp
	NewTimer(d time.Duration) Timer
}

// ---------------------------------------------------------------------------
// The live adapters
// ---------------------------------------------------------------------------

// systemClock is the production Clock. It is private and reachable only
// through the Clock interface, so no caller can hold a concrete clock and
// reach past the seam every deadline in this package is measured against.
type systemClock struct{ start time.Time }

// NewSystemClock returns the production Clock. Its Mono readings are durations
// since it was constructed.
func NewSystemClock() Clock { return &systemClock{start: time.Now()} }

func (c *systemClock) Now() Stamp {
	now := time.Now()
	return Stamp{WallMs: now.UnixMilli(), Mono: now.Sub(c.start)}
}

func (c *systemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{t: time.NewTimer(d)}
}

type systemTimer struct{ t *time.Timer }

func (t *systemTimer) C() <-chan time.Time        { return t.t.C }
func (t *systemTimer) Stop() bool                 { return t.t.Stop() }
func (t *systemTimer) Reset(d time.Duration) bool { return t.t.Reset(d) }

// liveDialer is the production Dialer over github.com/coder/websocket.
type liveDialer struct{ transport http.RoundTripper }

// NewLiveDialer returns the production Dialer.
//
// Three settings are load-bearing and none of them is a default:
//
//   - CompressionContextTakeover. `websockets` negotiates permessage-deflate by
//     default and `coder/websocket` does not, so omitting this changes both the
//     handshake's Sec-WebSocket-Extensions field and delivery timing relative
//     to the Python rig we are measured against.
//   - A 1 MiB read limit, which is `websockets`' max_size default. A larger
//     frame kills the Python connection, so accepting one here would let this
//     process act on a message the shadow never saw.
//   - Redirects REFUSED. This is the harness's own defect, found while testing
//     `rest`: a 307 preserves method and body, so a redirected request is a
//     second request to a host our signature does not cover. On a websocket the
//     consequence is worse than a wasted round trip -- it is a signed handshake
//     completing against an unsigned host, and then a book we trade on.
func NewLiveDialer() Dialer { return liveDialer{} }

// NewLiveDialerWithTransport is the production constructor (F6).
//
// The transport carries `netx.CachedDialer`, so the handshake resolves through
// the one-hour last-known-good cache and a reconnect survives the system
// resolver wedging -- which is when a reconnect is most likely to be needed,
// because a wedge takes the socket down in the first place.
//
// It is a DISTINCT `http.Transport` from the REST one even though both share a
// dialer. H-FAIL-2 requires two transports: REST is how cancels still reach the
// exchange when the feed is gone, and a pool fault shared between them would
// take both out at once. `M-7ZT-WSBYPASS` builds this on the default transport.
func NewLiveDialerWithTransport(rt http.RoundTripper) Dialer {
	return liveDialer{transport: rt}
}

func (d liveDialer) Dial(ctx context.Context, url string, h http.Header) (Socket, error) {
	hc := &http.Client{
		Transport: d.transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("refusing redirect to %s: the handshake is "+
				"signed for one host and a redirect completes it against "+
				"another", req.URL.Host)
		},
	}
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient:      hc,
		HTTPHeader:      h,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		if resp != nil {
			return nil, &HandshakeError{StatusCode: resp.StatusCode, Err: err}
		}
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	return &liveSocket{ws: ws}, nil
}

type liveSocket struct{ ws *websocket.Conn }

// Read blocks for the next frame.
//
// ctx must NOT carry a per-read deadline: coder/websocket fails the whole
// connection when a Read context expires, so a deadline here would BE a
// disconnect rather than a timeout. The read backstop is a separate timer in
// the session, which is why it can distinguish "nothing arrived" from "the
// socket broke".
func (s *liveSocket) Read(ctx context.Context) ([]byte, error) {
	typ, data, err := s.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText && typ != websocket.MessageBinary {
		return nil, fmt.Errorf("unexpected frame type %v", typ)
	}
	return data, nil
}

func (s *liveSocket) Write(ctx context.Context, b []byte) error {
	return s.ws.Write(ctx, websocket.MessageText, b)
}

func (s *liveSocket) Ping(ctx context.Context) error { return s.ws.Ping(ctx) }

func (s *liveSocket) Close() error {
	return s.ws.Close(websocket.StatusNormalClosure, "")
}

// ---------------------------------------------------------------------------
// Handshake and close classification
// ---------------------------------------------------------------------------

// HandshakeError is a handshake the server ANSWERED and that then failed:
// a rejected status, or a 101 whose upgrade headers are wrong.
//
// It is kept distinct from a transport failure because the right response
// differs. Retrying a 401 every second for a week is the worst possible answer
// to a bad signature, and it is what an undifferentiated reconnect loop does.
type HandshakeError struct {
	StatusCode int
	Err        error
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("handshake rejected: HTTP %d: %v", e.StatusCode, e.Err)
}

func (e *HandshakeError) Unwrap() error { return e.Err }

// ErrCleanClose reports a close with status 1000 or 1001.
var ErrCleanClose = errors.New("websocket closed normally")

// IsCleanClose reports whether err is a normal-closure end of stream.
//
// It decides only the BACKOFF, never the quarantine. H-FAIL-5 makes every
// disconnect quarantine the book however clean it was, so this classification
// cannot reach that decision -- which is the mistake the shadow rig makes
// legitimately (port-spec P25a) and that a trading harness cannot afford.
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

// WSURL is the exchange's websocket endpoint, and WSPath is the path the
// handshake signature is computed over -- with no query string, which is what
// the signer expects and what the exchange verifies.
const (
	WSURL  = feed.WSURL
	WSPath = feed.WSPath
)

// confidence: high

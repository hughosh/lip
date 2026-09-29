package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"lip/harness/rest"
	"lip/harness/wsx"
)

// disconnectDialer keeps the existing seam exchange's wire frames and REST
// transport, but gives each supervisor generation its own failure point.
type disconnectDialer struct {
	mu            sync.Mutex
	base          *seamDialer
	reconnectGate chan struct{}
	sockets       []*disconnectSocket
}

func (d *disconnectDialer) Dial(ctx context.Context, _ string, _ http.Header) (wsx.Socket, error) {
	d.mu.Lock()
	reconnecting := len(d.sockets) > 0
	d.mu.Unlock()
	if reconnecting {
		select {
		case <-d.reconnectGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s := &disconnectSocket{base: d.base, frames: make(chan []byte, 2),
		readErr: make(chan error, 1), pinged: make(chan struct{}, 1)}
	d.mu.Lock()
	if len(d.sockets) == 0 {
		// newSeamHarness preloads the opening snapshot. Transfer it to the
		// first socket so a dying generation cannot steal a later one.
		s.frames <- <-d.base.frames
	}
	d.sockets = append(d.sockets, s)
	d.mu.Unlock()
	return s, nil
}

func (d *disconnectDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sockets)
}

func (d *disconnectDialer) first() *disconnectSocket {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sockets[0]
}

func (d *disconnectDialer) latest() *disconnectSocket {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sockets[len(d.sockets)-1]
}

type disconnectSocket struct {
	base     *seamDialer
	frames   chan []byte
	readErr  chan error
	pinged   chan struct{}
	halfOpen atomic.Bool
}

func (s *disconnectSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case err := <-s.readErr:
		return nil, err
	case frame := <-s.frames:
		return frame, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *disconnectSocket) Write(ctx context.Context, b []byte) error {
	return (&seamSocket{d: s.base}).Write(ctx, b)
}

func (s *disconnectSocket) Ping(ctx context.Context) error {
	if !s.halfOpen.Load() {
		return nil
	}
	select {
	case s.pinged <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *disconnectSocket) Close() error { return nil }

// TestDisconnectRecoveryRequiresNewSnapshotAndCurrentPortfolioTruth composes
// the F1/F2/F3 detector with the serving owner, independent REST poller and
// dispatcher. An inventory change during the gap would require an exit; it
// cannot be placed until BOTH recovery inputs arrive on the new generation.
func TestDisconnectRecoveryRequiresNewSnapshotAndCurrentPortfolioTruth(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		half         bool
		snapshotLate bool
	}{
		{"close 1000", websocket.CloseError{Code: websocket.StatusNormalClosure}, false, true},
		{"close 1001", websocket.CloseError{Code: websocket.StatusGoingAway}, false, false},
		{"abnormal read failure", errors.New("connection reset by peer"), false, false},
		{"half-open pong timeout", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{ListCreated: true})
			// A close inside close_lead but beyond final_lead keeps a flat
			// market from posting opening adds. When inventory appears, the
			// required placement is unambiguously its capped exit.
			h.ex.closeAt = time.Now().Add(30 * time.Minute)
			d := &disconnectDialer{base: h.ws, reconnectGate: make(chan struct{})}
			sup, err := wsx.NewSupervisor(seamSigner{}, d, h.clk, h.cfg.Params,
				[]string{seamTicker})
			if err != nil {
				t.Fatal(err)
			}
			h.rig.sup = sup
			var positionGETs atomic.Int64
			h.ex.beforeDo = func(req rest.Request) {
				if req.Method == "GET" && req.Path == "/portfolio/positions" {
					positionGETs.Add(1)
				}
			}
			h.start()
			h.awaitActionable()
			h.awaitTicks(3)
			baselineCreates := h.ex.createCount()
			if baselineCreates != 0 {
				t.Fatalf("flat settling control already dispatched %d orders", baselineCreates)
			}

			// Hold the redial after this fault so the independent REST poller
			// can learn inventory while the websocket remains down.
			beforePoll := positionGETs.Load()
			first := d.first()
			if tc.half {
				first.halfOpen.Store(true)
				h.clk.Advance(h.cfg.Params.WSPingInterval)
				select {
				case <-first.pinged:
				case <-time.After(seamBudget):
					t.Fatal("the half-open socket was never pinged")
				}
				h.clk.Advance(h.cfg.Params.PongTimeout)
			} else {
				first.readErr <- tc.err
			}

			h.await("the disconnect to quarantine the published book", func() bool {
				m, ok := h.market()
				return ok && !m.BookActionable
			})
			h.ex.setPosition(seamTicker, "1.00")
			for i := 0; i < 3; i++ {
				h.clk.Advance(h.cfg.Params.PositionPoll)
				h.awaitTicks(2)
				m, ok := h.market()
				if ok && m.Q.Wire() == "1.00" {
					break
				}
			}
			m, ok := h.market()
			if !ok || m.Q.Wire() != "1.00" || positionGETs.Load() <= beforePoll {
				t.Fatalf("the independent poller did not learn inventory during the gap: market=%+v, position GETs=%d before=%d",
					m, positionGETs.Load(), beforePoll)
			}
			if n := h.ex.createCount(); n != baselineCreates {
				t.Fatalf("%d new placements against a disconnected book", n-baselineCreates)
			}
			if !tc.snapshotLate {
				h.ex.breakPositions(true)
			}
			beforeReconnectPoll := positionGETs.Load()
			close(d.reconnectGate)

			// Clean closes redial immediately; abnormal closes and the
			// half-open failure must first traverse supervisor backoff.
			if tc.half || tc.err != nil && !wsx.IsCleanClose(tc.err) {
				h.clk.Advance(time.Second)
			}
			h.await("the supervisor to establish a new generation", func() bool {
				return d.count() >= 2
			})
			if tc.snapshotLate {
				h.await("current-generation REST reconciliation before the snapshot", func() bool {
					return positionGETs.Load() > beforeReconnectPoll
				})
			}
			h.awaitTicks(2)
			if m, ok := h.market(); !ok || m.BookActionable || h.ex.createCount() != baselineCreates {
				t.Fatalf("reconnect reused the old book: market=%+v, present=%v, creates=%d",
					m, ok, h.ex.createCount())
			}

			frame, err := h.ws.frame("orderbook_snapshot", seamBook())
			if err != nil {
				t.Fatal(err)
			}
			d.latest().frames <- frame
			h.awaitTicks(2)
			if !tc.snapshotLate {
				if m, ok := h.market(); !ok || m.BookActionable || h.ex.createCount() != baselineCreates {
					t.Fatalf("fresh snapshot bypassed missing positions truth: market=%+v, present=%v, creates=%d",
						m, ok, h.ex.createCount())
				}

				h.ex.breakPositions(false)
				// The disconnect and reconnect tokens can coalesce with a cadence
				// tick. Advance until a complete cycle begun after the repair is
				// published; each step is still below the read backstop.
				for i := 0; i < 3; i++ {
					h.clk.Advance(h.cfg.Params.PositionPoll)
					h.awaitTicks(2)
					m, ok := h.market()
					if ok && m.BookActionable {
						break
					}
				}
				m, ok = h.market()
				if !ok || !m.BookActionable {
					t.Fatalf("fresh snapshot and repaired poll did not reopen A13: market=%+v, present=%v, position GETs=%d, sockets=%d",
						m, ok, positionGETs.Load(), d.count())
				}
			} else {
				h.awaitActionable()
			}
			m, _ = h.market()
			h.await("the recovered owner to dispatch the inventory exit", func() bool {
				return h.ex.createCount() > baselineCreates
			})
			m, _ = h.market()
			if m.Q.Wire() != "1.00" {
				t.Fatalf("recovered placement used q=%s, want 1.00", m.Q.Wire())
			}
		})
	}
}

// A full poller channel used to discard the reconnect token permanently.
// This fixes the scheduling race behind the composed test's occasional failure
// to reopen A13, even though REST kept polling and the book had resnapshotted.
func TestFullReconcileChannelEventuallyDeliversNewestGeneration(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	tokens := make(chan wsx.ReconcileToken, 1)
	first := h.rig.gate.OnConnect(h.clk.Now()).Token
	o.offerToken(tokens, first)
	disconnected := h.rig.gate.ApplyDisconnect(h.clk.Now(), false).Token
	o.offerToken(tokens, disconnected)
	latest := h.rig.gate.OnConnect(h.clk.Now()).Token
	o.offerToken(tokens, latest)

	select {
	case got := <-tokens:
		if got != first {
			t.Fatal("the preexisting token unexpectedly changed while queued")
		}
	default:
		t.Fatal("the initial connect token was not queued")
	}
	o.retryReconcile()
	select {
	case got := <-tokens:
		if got != latest {
			t.Fatal("the poller received an old disconnect generation after reconnect")
		}
	default:
		t.Fatal("the newest reconnect token was dropped when the channel was full")
	}
}

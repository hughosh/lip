package wsx

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"lip/harness/cfg"
)

// backoffLadder is the reconnect delay after an ABNORMAL disconnect, in
// seconds: 1, 2, 4, 8, 16, 32, 60 and then 60 forever.
//
// A clean close does not use it at all. The exchange closing a socket politely
// is routine -- a deploy, a rolling restart -- and waiting a minute to come
// back from one would leave every market non-actionable for a minute over an
// event that cost nothing.
var backoffLadder = [...]time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	16 * time.Second, 32 * time.Second, 60 * time.Second,
}

func backoffAt(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(backoffLadder) {
		return backoffLadder[len(backoffLadder)-1]
	}
	return backoffLadder[attempt]
}

// Supervisor owns the socket's whole lifetime: dial, subscribe, run, fail,
// back off, dial again, forever.
//
// It never gives up and never returns because of a socket. Run exits only when
// the PROCESS context is cancelled. A supervisor that returned on a handshake
// rejection would take the book feed down permanently over a clock skew or a
// rotated key, and the harness would keep trading against the last book it saw
// -- which is the failure the whole package is arranged to prevent.
type Supervisor struct {
	signer  Signer
	dialer  Dialer
	clk     Clock
	p       cfg.Params
	tickers []string
	url     string
}

// NewSupervisor validates everything that fails silently at runtime.
//
// The ticker checks are here rather than at dial time because both of their
// failure modes are silences: an empty subscription is accepted and delivers
// nothing, and a duplicated ticker delivers every delta for it twice.
func NewSupervisor(signer Signer, dialer Dialer, clock Clock, params cfg.Params,
	tickers []string) (*Supervisor, error) {

	if signer == nil {
		return nil, fmt.Errorf("no signer")
	}
	if dialer == nil {
		return nil, fmt.Errorf("no dialer")
	}
	if clock == nil {
		return nil, fmt.Errorf("no clock")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateTickers(tickers); err != nil {
		return nil, err
	}
	return &Supervisor{
		signer:  signer,
		dialer:  dialer,
		clk:     clock,
		p:       params,
		tickers: append([]string(nil), tickers...),
		url:     WSURL,
	}, nil
}

// Run supervises the socket until `ctx` is cancelled.
//
// # H-FAIL-2, structurally
//
// Every session gets its own context, derived here and cancelled here. The
// context the portfolio poller runs on is the caller's and is never touched.
// That is what makes "a socket failure must not stop position monitoring" a
// property of the code's shape rather than of anyone remembering it: there is
// no cancel function in this file that the poller is reachable from.
func (s *Supervisor) Run(ctx context.Context, cmds <-chan Command,
	events chan<- Event) error {

	attempt := 0
	downSince := s.clk.Now().Mono
	reduceSent := false

	emit := func(e Event) error {
		select {
		case events <- e:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		sock, err := s.dial(ctx)
		if err != nil {
			if e := emit(Event{Kind: EventDisconnected, At: s.clk.Now(),
				Clean: false, Cause: err}); e != nil {
				return e
			}
			if e := s.wait(ctx, backoffAt(attempt), &downSince, &reduceSent,
				emit); e != nil {
				return e
			}
			attempt++
			continue
		}

		attempt = 0
		reduceSent = false
		if e := emit(Event{Kind: EventConnected, At: s.clk.Now()}); e != nil {
			sock.Close()
			return e
		}

		sessCtx, cancel := context.WithCancel(ctx)
		sess := &session{sock: sock, clk: s.clk, p: s.p}
		runErr := sess.run(sessCtx, cmds, events)
		cancel()
		sock.Close()

		if ctx.Err() != nil {
			return ctx.Err()
		}

		clean := IsCleanClose(runErr)
		downSince = s.clk.Now().Mono
		if e := emit(Event{Kind: EventDisconnected, At: s.clk.Now(),
			Clean: clean, Cause: runErr}); e != nil {
			return e
		}
		if clean {
			// Straight back around: no backoff, no ladder position consumed.
			continue
		}
		if e := s.wait(ctx, backoffAt(attempt), &downSince, &reduceSent,
			emit); e != nil {
			return e
		}
		attempt++
	}
}

// wait sleeps out the backoff while keeping F4's disconnect clock running.
//
// The reduce threshold is measured over the WHOLE outage, not over one backoff
// step. Five failed dials at eight seconds each is a forty-second outage, and a
// per-step timer would never reach sixty. The event is emitted once per outage;
// reconnect attempts and the independent REST poller carry on either way.
func (s *Supervisor) wait(ctx context.Context, d time.Duration,
	downSince *time.Duration, reduceSent *bool, emit func(Event) error) error {

	deadline := s.clk.Now().Mono + d
	for {
		now := s.clk.Now()
		down := now.Mono - *downSince

		if !*reduceSent && down >= s.p.DisconnectReduce {
			*reduceSent = true
			if err := emit(Event{Kind: EventDisconnectReduce, At: now,
				Down: down}); err != nil {
				return err
			}
		}
		remaining := deadline - now.Mono
		if remaining <= 0 {
			return nil
		}
		// Wake at whichever comes first: the end of the backoff, or the moment
		// F4's threshold is crossed.
		step := remaining
		if !*reduceSent {
			toReduce := s.p.DisconnectReduce - down
			if toReduce > 0 && toReduce < step {
				step = toReduce
			}
		}
		t := s.clk.NewTimer(step)
		select {
		case <-t.C():
			t.Stop()
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// dial performs the signed handshake and sends both subscriptions.
//
// The signature covers the websocket PATH with no query string, which is what
// the exchange verifies. The wall clock comes from the injected Clock, so a
// test signs against a known timestamp and a scenario replays one.
func (s *Supervisor) dial(ctx context.Context) (Socket, error) {
	hdr, err := s.signer.WSHeaders(s.clk.Now().WallMs)
	if err != nil {
		return nil, fmt.Errorf("sign handshake: %w", err)
	}
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}

	// The handshake gets its own bound. Applying it to the session would
	// disconnect us every ten seconds.
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	sock, err := s.dialer.Dial(dialCtx, s.url, h)
	if err != nil {
		return nil, err
	}

	delta, err := subscribeDelta(s.tickers)
	if err != nil {
		sock.Close()
		return nil, err
	}
	trade, err := subscribeTrade()
	if err != nil {
		sock.Close()
		return nil, err
	}
	// Both subscriptions, or neither. A connection carrying only the filtered
	// delta stream looks healthy, answers pings and produces books -- and never
	// delivers a single trade, so every fill-adjacent measurement silently
	// reads zero.
	if err := sock.Write(ctx, delta); err != nil {
		sock.Close()
		return nil, fmt.Errorf("subscribe orderbook_delta: %w", err)
	}
	if err := sock.Write(ctx, trade); err != nil {
		sock.Close()
		return nil, fmt.Errorf("subscribe trade: %w", err)
	}
	return sock, nil
}

// confidence: high

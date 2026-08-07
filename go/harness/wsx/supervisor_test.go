package wsx

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitEvent drains events until one of `kind` arrives or the test times out.
func waitEvent(t *testing.T, events <-chan Event, kind EventKind) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %v event", kind)
		}
	}
}

// runSupervisor starts a supervisor over a scripted dialer and returns the
// pieces a test drives it with.
func runSupervisor(t *testing.T, clk *fakeClock, d *scriptedDialer) (
	context.CancelFunc, chan Command, chan Event, <-chan error) {

	t.Helper()
	sup, err := NewSupervisor(&fakeSigner{}, d, clk, testParams(),
		[]string{fxTicker})
	if err != nil {
		t.Fatal(err)
	}
	cmds := make(chan Command, 4)
	events := make(chan Event, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx, cmds, events) }()
	return cancel, cmds, events, done
}

// TestSupervisorPingPongReadDeadlineLadder is F1's three clocks, each shown to
// catch a failure the other two cannot.
//
//   - ping_interval_s MANUFACTURES evidence on an idle socket. Nothing detects
//     anything without it.
//   - pong_timeout_s catches a peer that is gone while the TCP connection is
//     still established -- the half-open case, which a read deadline alone
//     takes read_deadline_s to notice.
//   - read_deadline_s is the backstop for a peer that is answering pings and no
//     longer publishing. The library answers pings from inside its read loop,
//     so that peer looks perfectly healthy to the first two clocks.
//
// The last sub-test is the one that makes the third clock load-bearing rather
// than decorative: pongs succeed throughout, and the session still fails.
func TestSupervisorPingPongReadDeadlineLadder(t *testing.T) {
	p := testParams()

	t.Run("a ping is sent every ping_interval_s", func(t *testing.T) {
		clk := newFakeClock()
		d := newScriptedDialer()
		sock := newScriptedSocket()
		d.push(sock, nil)
		cancel, _, events, _ := runSupervisor(t, clk, d)
		defer cancel()

		waitEvent(t, events, EventConnected)
		clk.BlockUntilTimers(2) // ping + read backstop

		// Each iteration: queue a pong, fire the ping clock, and wait for the
		// session to RESOLVE that ping -- which it signals by stopping its pong
		// deadline. Advancing again before that would leave a stale five-second
		// deadline armed and the next advance would trip it, which is a race in
		// the test and not a defect in the ladder.
		for i := 1; i <= 3; i++ {
			sock.pongs <- nil
			clk.Advance(p.WSPingInterval)
			clk.BlockUntilStops(i)
		}
		select {
		case e := <-events:
			if e.Kind == EventDisconnected {
				t.Fatalf("an answered ping killed the session: %v", e.Cause)
			}
		default:
		}
	})

	t.Run("an unanswered ping fails the session at pong_timeout_s", func(t *testing.T) {
		clk := newFakeClock()
		d := newScriptedDialer()
		first, second := newScriptedSocket(), newScriptedSocket()
		d.push(first, nil)
		d.push(second, nil)
		cancel, _, events, _ := runSupervisor(t, clk, d)
		defer cancel()

		waitEvent(t, events, EventConnected)
		clk.BlockUntilTimers(2)

		// No pong is queued, so Ping blocks on its context.
		clk.Advance(p.WSPingInterval)
		clk.BlockUntilTimers(3) // ping + read + pong deadline

		// The read backstop is nowhere near due; only the pong deadline is.
		clk.Advance(p.PongTimeout)
		down := waitEvent(t, events, EventDisconnected)
		if down.Clean {
			t.Fatal("an unanswered ping was classified as a clean close")
		}
		if down.Cause == nil ||
			!contains(down.Cause.Error(), "pong_timeout_s") {
			t.Fatalf("cause = %v, want the pong timeout", down.Cause)
		}
		if p.PongTimeout >= p.ReadDeadline {
			t.Fatal("this test asserts the pong clock fires FIRST; the " +
				"parameters no longer make that true")
		}
	})

	t.Run("read_deadline_s fires while pongs still succeed", func(t *testing.T) {
		clk := newFakeClock()
		d := newScriptedDialer()
		first, second := newScriptedSocket(), newScriptedSocket()
		d.push(first, nil)
		d.push(second, nil)
		cancel, _, events, _ := runSupervisor(t, clk, d)
		defer cancel()

		waitEvent(t, events, EventConnected)
		clk.BlockUntilTimers(2)

		// Answer every ping. The peer is heartbeating and publishing nothing.
		elapsed := time.Duration(0)
		for i := 1; elapsed < p.ReadDeadline; i++ {
			first.pongs <- nil
			clk.Advance(p.WSPingInterval)
			elapsed += p.WSPingInterval
			if elapsed < p.ReadDeadline {
				clk.BlockUntilStops(i)
			}
		}

		down := waitEvent(t, events, EventDisconnected)
		if down.Cause == nil ||
			!contains(down.Cause.Error(), "read_deadline_s") {
			t.Fatalf("cause = %v, want the read backstop -- a peer that "+
				"answers pings and publishes nothing is invisible to the "+
				"first two clocks", down.Cause)
		}
	})

	t.Run("a delivered data frame resets the backstop", func(t *testing.T) {
		clk := newFakeClock()
		d := newScriptedDialer()
		sock := newScriptedSocket()
		d.push(sock, nil)
		cancel, _, events, _ := runSupervisor(t, clk, d)
		defer cancel()

		waitEvent(t, events, EventConnected)
		clk.BlockUntilTimers(2)

		// One second before the backstop is due, a data frame arrives.
		clk.Advance(p.ReadDeadline - time.Second)
		sock.frames <- []byte(fxDelta)
		waitEvent(t, events, EventFrame)

		// The old deadline passes with nothing happening.
		clk.Advance(2 * time.Second)
		select {
		case e := <-events:
			if e.Kind == EventDisconnected {
				t.Fatalf("the backstop fired despite a delivered frame: %v",
					e.Cause)
			}
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// TestSupervisorReconnectsCleanImmediatelyAndAbnormalWithBackoff is the
// asymmetry between the two close classes.
//
// A clean close is routine -- a deploy, a rolling restart -- and waiting a
// minute to come back from one leaves every market non-actionable for a minute
// over an event that cost nothing. An abnormal close may be a rejected
// signature or a wedged network, and reconnecting to those a thousand times a
// second is the worst available response.
//
// Neither classification reaches the quarantine: that is H-FAIL-5's, and it is
// unconditional. This test asserts only what `clean` is allowed to decide.
func TestSupervisorReconnectsCleanImmediatelyAndAbnormalWithBackoff(t *testing.T) {
	t.Run("clean close reconnects with no backoff", func(t *testing.T) {
		clk := newFakeClock()
		d := newScriptedDialer()
		first, second := newScriptedSocket(), newScriptedSocket()
		d.push(first, nil)
		d.push(second, nil)
		cancel, _, events, _ := runSupervisor(t, clk, d)
		defer cancel()

		waitEvent(t, events, EventConnected)
		clk.BlockUntilTimers(2)

		first.readErr <- ErrCleanClose
		down := waitEvent(t, events, EventDisconnected)
		if !down.Clean {
			t.Fatalf("a 1000/1001 close was classified abnormal: %v", down.Cause)
		}
		// No clock movement at all: the reconnect must already have happened.
		waitEvent(t, events, EventConnected)
		if got := d.attemptCount(); got != 2 {
			t.Fatalf("dial attempts = %d, want 2 with no time having passed",
				got)
		}
	})

	t.Run("abnormal close climbs the ladder", func(t *testing.T) {
		clk := newFakeClock()
		d := newScriptedDialer()
		first := newScriptedSocket()
		d.push(first, nil)
		// Three failed dials, then a socket.
		for i := 0; i < 3; i++ {
			d.push(nil, errors.New("connection refused"))
		}
		d.push(newScriptedSocket(), nil)
		cancel, _, events, _ := runSupervisor(t, clk, d)
		defer cancel()

		waitEvent(t, events, EventConnected)
		clk.BlockUntilTimers(2)

		first.readErr <- errors.New("broken pipe")
		down := waitEvent(t, events, EventDisconnected)
		if down.Clean {
			t.Fatal("a broken pipe was classified as a clean close")
		}

		// The first backoff step is one second, and nothing happens before it.
		clk.BlockUntilTimers(1)
		if got := d.attemptCount(); got != 1 {
			t.Fatalf("the supervisor redialled during the backoff (attempts "+
				"= %d)", got)
		}

		// 1s, 2s, 4s: each failed dial takes the next rung.
		for _, step := range []time.Duration{time.Second, 2 * time.Second,
			4 * time.Second} {
			clk.Advance(step)
			waitEvent(t, events, EventDisconnected)
			clk.BlockUntilTimers(1)
		}
		clk.Advance(8 * time.Second)
		waitEvent(t, events, EventConnected)
		if got := d.attemptCount(); got != 5 {
			t.Fatalf("dial attempts = %d, want 5", got)
		}
	})

	t.Run("the ladder is bounded", func(t *testing.T) {
		if backoffAt(0) != time.Second {
			t.Fatalf("first step = %v, want 1s", backoffAt(0))
		}
		if backoffAt(6) != 60*time.Second {
			t.Fatalf("seventh step = %v, want 60s", backoffAt(6))
		}
		if backoffAt(400) != 60*time.Second {
			t.Fatalf("the ladder is unbounded: %v", backoffAt(400))
		}
	})
}

// TestSocketFailureDoesNotCancelPortfolioPoller is H-FAIL-2, made structural.
//
// REST and the websocket are distinct transports. The probe's defect was an
// observer that stopped; the cheapest way to be sure this one does not is for
// the thing that fails to have no way of reaching it. Every session runs on a
// context derived and cancelled inside the supervisor, and the poller runs on
// the caller's -- so there is no cancel function in the socket path that the
// poller is reachable from.
//
// The test kills the socket repeatedly and requires the poller to keep
// answering on its own cadence throughout.
func TestSocketFailureDoesNotCancelPortfolioPoller(t *testing.T) {
	clk := newFakeClock()
	p := testParams()

	src := newScriptedPortfolio()
	pol, err := NewPoller(src, clk, p.PositionPoll)
	if err != nil {
		t.Fatal(err)
	}
	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	reads := make(chan PortfolioRead, 64)
	pollDone := make(chan error, 1)
	go func() { pollDone <- pol.Run(pollCtx, nil, reads) }()
	clk.BlockUntilTimers(1)

	d := newScriptedDialer()
	first := newScriptedSocket()
	d.push(first, nil)
	d.push(nil, errors.New("connection refused"))
	d.push(nil, errors.New("connection refused"))
	supCancel, _, events, _ := runSupervisor(t, clk, d)
	defer supCancel()

	waitEvent(t, events, EventConnected)
	clk.BlockUntilTimers(3) // poller + ping + read backstop

	// Kill the socket.
	first.readErr <- errors.New("connection reset by peer")
	waitEvent(t, events, EventDisconnected)

	// The poller must still be polling, with the socket down and staying down.
	for i := 1; i <= 3; i++ {
		clk.Advance(p.PositionPoll)
		select {
		case r := <-reads:
			if r.Seq() != uint64(i) {
				t.Fatalf("poll %d arrived with seq %d", i, r.Seq())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the portfolio poller stopped after poll %d; a socket "+
				"failure cancelled a context it must not be able to reach", i)
		}
	}

	select {
	case err := <-pollDone:
		t.Fatalf("the poller returned while its own context was live: %v", err)
	default:
	}
}

// TestDisconnectThresholdReducesWithoutStoppingRESTOrSupervisor is F4.
//
// Three things happen at disconnect_reduce_s and all three matter:
//
//   - every market goes to REDUCING, once, with a SEV1;
//   - the supervisor KEEPS RECONNECTING. The reduction is not a shutdown;
//   - the portfolio poller keeps polling, because it never depended on the
//     socket in the first place.
//
// The threshold is measured over the WHOLE outage, not over one backoff step:
// five failed dials at eight seconds each is a forty-second outage, and a
// per-step timer would never reach sixty.
func TestDisconnectThresholdReducesWithoutStoppingRESTOrSupervisor(t *testing.T) {
	clk := newFakeClock()
	p := testParams()

	src := newScriptedPortfolio()
	pol, err := NewPoller(src, clk, p.PositionPoll)
	if err != nil {
		t.Fatal(err)
	}
	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	reads := make(chan PortfolioRead, 64)
	go func() { _ = pol.Run(pollCtx, nil, reads) }()
	clk.BlockUntilTimers(1)

	d := newScriptedDialer()
	first := newScriptedSocket()
	d.push(first, nil)
	for i := 0; i < 20; i++ {
		d.push(nil, errors.New("connection refused"))
	}
	d.push(newScriptedSocket(), nil)
	supCancel, _, events, _ := runSupervisor(t, clk, d)
	defer supCancel()

	waitEvent(t, events, EventConnected)
	clk.BlockUntilTimers(3)

	first.readErr <- errors.New("connection reset by peer")
	waitEvent(t, events, EventDisconnected)

	// Walk the clock past the threshold in poll-sized steps, so the poller is
	// exercised over the same window.
	var reduce Event
	deadline := p.DisconnectReduce + 30*time.Second
	for elapsed := time.Duration(0); elapsed <= deadline; elapsed += p.PositionPoll {
		clk.Advance(p.PositionPoll)
		select {
		case e := <-events:
			if e.Kind == EventDisconnectReduce {
				reduce = e
			}
		case <-time.After(20 * time.Millisecond):
		}
		if reduce.Kind == EventDisconnectReduce {
			break
		}
	}
	if reduce.Kind != EventDisconnectReduce {
		t.Fatal("the socket was down past disconnect_reduce_s and no reduce " +
			"was emitted")
	}
	if reduce.Down < p.DisconnectReduce {
		t.Fatalf("reduce reported %v of downtime, less than "+
			"disconnect_reduce_s %v", reduce.Down, p.DisconnectReduce)
	}

	// The gate records it, once, and stickily.
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	connectAndReconcile(g, fxTicker, at(0))
	g.ApplyDisconnect(at(1), false)
	ge := g.NoteDisconnectSustained(reduce.Down, reduce.At)
	if len(ge.Reduce) != 1 || ge.Reduce[0] != fxTicker {
		t.Fatalf("gate reduce = %v", ge.Reduce)
	}
	if !g.Reducing(fxTicker) {
		t.Fatal("the reduction is not sticky")
	}

	// The supervisor is still trying, and the poller is still polling.
	before := d.attemptCount()
	clk.Advance(120 * time.Second)
	deadlineT := time.After(5 * time.Second)
	for d.attemptCount() <= before {
		select {
		case <-deadlineT:
			t.Fatal("the supervisor stopped reconnecting after the reduce; " +
				"F4 reduces exposure, it does not give up on the feed")
		default:
			clk.Advance(60 * time.Second)
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-reads:
	case <-time.After(5 * time.Second):
		t.Fatal("the portfolio poller stopped after the reduce")
	}
}

// TestSupervisorSubscribesToBothStreamsOrNeither covers the dial path.
//
// A connection carrying only the filtered delta stream looks healthy, answers
// pings and produces books -- and never delivers a single trade, so every
// fill-adjacent measurement silently reads zero.
func TestSupervisorSubscribesToBothStreamsOrNeither(t *testing.T) {
	clk := newFakeClock()
	d := newScriptedDialer()
	sock := newScriptedSocket()
	d.push(sock, nil)
	cancel, cmds, events, _ := runSupervisor(t, clk, d)
	defer cancel()

	waitEvent(t, events, EventConnected)
	if got := sock.writtenCount(); got != 2 {
		t.Fatalf("wrote %d subscriptions, want 2 (filtered delta and "+
			"unfiltered trade)", got)
	}
	if s := string(sock.written(0)); !contains(s, `"id":1`) ||
		!contains(s, "orderbook_delta") || !contains(s, fxTicker) {
		t.Fatalf("first write = %s", s)
	}
	if s := string(sock.written(1)); !contains(s, `"id":2`) ||
		!contains(s, `"trade"`) {
		t.Fatalf("second write = %s", s)
	}

	// A resnapshot command reaches the socket as id 3.
	cmds <- Command{Kind: CmdResnapshot, Sids: []int64{7},
		Tickers: []string{fxTicker}}
	deadline := time.After(5 * time.Second)
	for sock.writtenCount() < 3 {
		select {
		case <-deadline:
			t.Fatal("the resnapshot command never reached the socket")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if s := string(sock.written(2)); !contains(s, `"id":3`) ||
		!contains(s, "update_subscription") || !contains(s, `"sids":[7]`) {
		t.Fatalf("resnapshot write = %s", s)
	}

	// A zero-valued command is not executable as anything.
	if err := (Command{}).Validate(); err == nil {
		t.Fatal("the zero Command validated")
	}
}

// TestSupervisorRunOutlivesEveryTransportFailure is the property that keeps the
// book feed alive.
//
// A supervisor that returned on a handshake rejection would take the feed down
// permanently over a clock skew or a rotated key, and the harness would keep
// trading against the last book it saw. Run exits for one reason only.
func TestSupervisorRunOutlivesEveryTransportFailure(t *testing.T) {
	clk := newFakeClock()
	d := newScriptedDialer()
	for i := 0; i < 8; i++ {
		d.push(nil, &HandshakeError{StatusCode: 401,
			Err: errors.New("bad signature")})
	}
	cancel, _, events, done := runSupervisor(t, clk, d)

	for i := 0; i < 4; i++ {
		waitEvent(t, events, EventDisconnected)
		select {
		case err := <-done:
			t.Fatalf("Run returned on a handshake rejection: %v", err)
		default:
		}
		clk.BlockUntilTimers(1)
		clk.Advance(60 * time.Second)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its process context was cancelled")
	}
}

// TestSupervisorRejectsArgumentsThatFailSilently covers construction.
func TestSupervisorRejectsArgumentsThatFailSilently(t *testing.T) {
	clk := newFakeClock()
	d := newScriptedDialer()
	p := testParams()

	if _, err := NewSupervisor(nil, d, clk, p, []string{"A"}); err == nil {
		t.Fatal("a nil signer was accepted")
	}
	if _, err := NewSupervisor(&fakeSigner{}, nil, clk, p, []string{"A"}); err == nil {
		t.Fatal("a nil dialer was accepted")
	}
	if _, err := NewSupervisor(&fakeSigner{}, d, nil, p, []string{"A"}); err == nil {
		t.Fatal("a nil clock was accepted")
	}
	if _, err := NewSupervisor(&fakeSigner{}, d, clk, p, nil); err == nil {
		t.Fatal("an empty market set was accepted")
	}
	if _, err := NewSupervisor(&fakeSigner{}, d, clk, p, []string{"A", "A"}); err == nil {
		t.Fatal("a duplicated ticker was accepted")
	}
	bad := p
	bad.PongTimeout = p.WSPingInterval
	if _, err := NewSupervisor(&fakeSigner{}, d, clk, bad, []string{"A"}); err == nil {
		t.Fatal("a params set the harness's own validator rejects was accepted")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

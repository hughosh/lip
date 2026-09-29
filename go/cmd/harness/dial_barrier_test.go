package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/rest"
	"lip/harness/wsx"
)

type closeDialResolver struct {
	failed atomic.Bool
	calls  atomic.Int32
}

func (r *closeDialResolver) LookupHost(context.Context, string) ([]string, error) {
	r.calls.Add(1)
	if r.failed.Load() {
		return nil, errors.New("injected resolver failure")
	}
	return []string{"192.0.2.10"}, nil
}

func TestServingCloseWaitsForDetachedDialReport(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	resolver := &closeDialResolver{}
	var advanced atomic.Bool
	var block atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	nt, err := newF6Net(h.rig.anom, resolver, func(context.Context, string, string) (net.Conn, error) {
		if block.Load() {
			close(entered)
			<-release
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}, func() time.Time {
		at := time.Unix(1_700_000_000, 0)
		if advanced.Load() {
			return at.Add(2 * time.Hour)
		}
		return at
	})
	if err != nil {
		t.Fatal(err)
	}
	// Populate the actual shared resolver cache. The next, expired lookup fails
	// and reports through the production fallback reporter only after dial ends.
	conn, err := nt.rest.DialContext(context.Background(), "tcp", "example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	advanced.Store(true)
	resolver.failed.Store(true)
	block.Store(true)
	quiesceReturned := make(chan struct{})
	h.rig.ex.quiesce = func(ctx context.Context) error {
		err := nt.dials.pause(ctx)
		close(quiesceReturned)
		return err
	}
	h.rig.ex.resume = nt.dials.resume
	sd := startServingClose(t, h, &servingCloseStepper{})
	h.awaitActionable()
	dialDone := make(chan error, 1)
	go func() {
		conn, err := nt.rest.DialContext(context.Background(), "tcp", "example.test:443")
		if conn != nil {
			_ = conn.Close()
		}
		dialDone <- err
	}()
	<-entered
	stopDone := make(chan error, 1)
	go func() { stopDone <- sd.stop(context.Background()) }()
	h.await("transport final-close fence", func() bool { nt.dials.mu.Lock(); defer nt.dials.mu.Unlock(); return nt.dials.paused })
	select {
	case err := <-stopDone:
		t.Fatalf("close overtook detached fallback: %v", err)
	default:
	}
	// A Transport goroutine scheduled after the fence may not reach the dialer.
	calls := resolver.calls.Load()
	if conn, err := nt.ws.DialContext(context.Background(), "tcp", "late.test:443"); err == nil {
		_ = conn.Close()
		t.Fatal("late scheduled dial crossed final-close fence")
	}
	if resolver.calls.Load() != calls {
		t.Fatal("late dial invoked resolver after pause")
	}
	// Keep the accepted callback outstanding long enough to observe the
	// barrier itself. An immediate stopDone poll only tests scheduler order;
	// final alert/store work can conceal a missing transport join.
	select {
	case <-quiesceReturned:
		t.Fatal("transport quiesce returned with a detached reporter outstanding")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-dialDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if got := servingCloseAnomalyCount(t, h.cfg.Paths.DB, "DNS_FALLBACK"); got != 1 {
		t.Fatalf("detached fallback durable rows=%d, want 1", got)
	}
}

// The test above installs quiesce and resume by hand, so it cannot see whether
// production wires them. This one builds the exchange through exchangeOver and
// dials through the Doer and Dialer it returns. Without the two fields the
// orderly stop skips the fence and the store can stop under a detached dial
// report; M-9NQ-FENCE-UNWIRED deletes them and still builds.
func TestProductionExchangeFencesDialsOnBothTransports(t *testing.T) {
	f6NoDefaultTransport(t)
	f := newF6Fixture(t)
	_, nt := f6Compose(t, f)
	signer := f6Signer(t)
	ctx := context.Background()

	ex, err := exchangeOver(ctx, f6Config(t), signer, nt, nil)
	if err != nil {
		t.Fatalf("exchangeOver: %v", err)
	}
	if ex.quiesce == nil || ex.resume == nil {
		t.Fatal("exchangeOver left the final-close dial fence unwired")
	}
	// The programs walk left a pooled connection; only a new dial is fenced.
	nt.rest.CloseIdleConnections()
	fenced := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "quiescing for final close")
	}

	if err := ex.quiesce(ctx); err != nil {
		t.Fatalf("quiesce with no dial outstanding: %v", err)
	}
	before := f.snapshot()
	if _, err := rest.NewClient(ex.Doer).Balance(ctx); !fenced(err) {
		t.Fatalf("REST read under the fence: err=%v, want the fence refusal", err)
	}
	sock, err := ex.Dialer.Dial(ctx, wsx.WSURL, f6WSHeaders(t, signer))
	if sock != nil {
		_ = sock.Close()
	}
	if !fenced(err) {
		t.Fatalf("websocket dial under the fence: err=%v, want the fence refusal", err)
	}
	if got := f.snapshot(); len(got.dialed) != len(before.dialed) ||
		got.lookups[f6RESTHost] != before.lookups[f6RESTHost] ||
		got.lookups[f6WSHost] != before.lookups[f6WSHost] {
		t.Fatalf("the fence let a dial or lookup through: dialed %v -> %v",
			before.dialed, got.dialed)
	}

	ex.resume()
	if _, err := rest.NewClient(ex.Doer).Balance(ctx); err != nil {
		t.Fatalf("REST read after resume: %v", err)
	}
	sock, err = ex.Dialer.Dial(ctx, wsx.WSURL, f6WSHeaders(t, signer))
	if err != nil {
		t.Fatalf("websocket dial after resume: %v", err)
	}
	if err := sock.Close(); err != nil {
		t.Fatal(err)
	}
	after := f.snapshot()
	if after.dialsTo(f6RESTAddr) != before.dialsTo(f6RESTAddr)+1 ||
		after.dialsTo(f6WSAddr) != before.dialsTo(f6WSAddr)+1 {
		t.Fatalf("resume did not readmit one dial per transport: dialed %v -> %v",
			before.dialed, after.dialed)
	}
	if after.balances != before.balances+1 || after.handshakes != before.handshakes+1 {
		t.Fatalf("after resume: balances %d -> %d, handshakes %d -> %d; want one each",
			before.balances, after.balances, before.handshakes, after.handshakes)
	}
}

func TestDialFenceRefusalResumesWithoutOverlappingActiveDial(t *testing.T) {
	var b dialBarrier
	if !b.enter() {
		t.Fatal("initial registration refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.pause(ctx); err == nil {
		t.Fatal("pause claimed outstanding dial joined")
	}
	if b.enter() {
		t.Fatal("pause allowed another dial")
	}
	b.resume()
	if !b.enter() {
		t.Fatal("refused pause did not restore admission")
	}
	b.leave()
	b.leave()
	if err := b.pause(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// lip-tdz: the owner calls the final-close fence unconditionally, so a rig
// refuses an exchange without it rather than skipping it at close.
func TestRigRefusesAnExchangeWithoutTheDialFence(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	for _, missing := range []string{"quiesce", "resume"} {
		x := h.xch
		if missing == "quiesce" {
			x.quiesce = nil
		} else {
			x.resume = nil
		}
		if _, err := seamNewRig(t, h.ctx, h.cfg, false, x); err == nil ||
			!strings.Contains(err.Error(), missing) {
			t.Fatalf("an exchange without %s built a rig: %v", missing, err)
		}
	}
}

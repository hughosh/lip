package netx

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeResolver answers from a script and counts how often it was asked.
type fakeResolver struct {
	mu    sync.Mutex
	addrs []string
	err   error
	calls int
}

func (r *fakeResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.addrs, nil
}

func (r *fakeResolver) wedge(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *fakeResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// recordingDial records every address it was asked for and hands back a pipe.
type recordingDial struct {
	mu    sync.Mutex
	seen  []string
	fail  bool
	conns []net.Conn
}

func (d *recordingDial) fn(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, address)
	if d.fail {
		return nil, errors.New("connection refused")
	}
	a, b := net.Pipe()
	d.conns = append(d.conns, a, b)
	return a, nil
}

func (d *recordingDial) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.seen))
	copy(out, d.seen)
	return out
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFixture(t *testing.T) (*CachedDialer, *fakeResolver, *recordingDial,
	*testClock, *[]Fallback) {

	t.Helper()
	r := &fakeResolver{addrs: []string{"203.0.113.7"}}
	d := &recordingDial{}
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	var mu sync.Mutex
	reports := []Fallback{}
	cd, err := NewCachedDialer(r, d.fn, clk.now, func(f Fallback) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, f)
	})
	if err != nil {
		t.Fatalf("NewCachedDialer: %v", err)
	}
	t.Cleanup(func() {
		for _, c := range d.conns {
			c.Close()
		}
	})
	return cd, r, d, clk, &reports
}

// TestCachedDialerFallsBackOnlyAfterPriorSuccessfulResolution is F6's rule and
// its limit in one test.
//
// The RULE: once a host has resolved, a later resolver failure must not become
// a connection failure -- the last-known-good address carries the harness
// through the wedge.
//
// The LIMIT, which matters more: with NO prior success there is nothing to fall
// back TO, and a dialer that invented something -- a guess, a hard-coded
// address, another host's answer -- would be connecting somewhere it has never
// verified. The lookup error is returned unchanged.
//
// `M-7ZT-NOFALLBACK` returns the lookup error even when a cached answer exists,
// which is F6 deleted while leaving the cache in place to look like it works.
func TestCachedDialerFallsBackOnlyAfterPriorSuccessfulResolution(t *testing.T) {
	cd, r, d, _, reports := newFixture(t)
	ctx := context.Background()

	// COLD, and wedged: nothing to fall back to.
	r.wedge(errors.New("lookup api.elections.kalshi.com: i/o timeout"))
	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err == nil {
		t.Fatal("a cold dialer invented a fallback; with no prior resolution " +
			"there is no last-known-good address, and connecting anyway would " +
			"be a connection to somewhere never verified")
	}
	if len(d.addresses()) != 0 {
		t.Fatalf("a cold wedged dial reached the network: %v", d.addresses())
	}
	if len(*reports) != 0 {
		t.Fatal("a fallback was reported when none happened")
	}

	// Seed a real answer.
	r.wedge(nil)
	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err != nil {
		t.Fatalf("the seeding dial failed: %v", err)
	}

	// Now wedge it again. The cache carries it.
	r.wedge(errors.New("lookup api.example.test: i/o timeout"))
	// Past the floor, so the resolver is genuinely consulted and genuinely
	// fails -- otherwise this would pass on the cache never being refreshed.
	cd.entries["api.example.test"] = entry{addrs: []string{"203.0.113.7"},
		resolvedAt: time.Unix(0, 0)}

	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err != nil {
		t.Fatalf("the wedge was not survived: %v", err)
	}
	got := d.addresses()
	if len(got) != 2 || got[1] != "203.0.113.7:443" {
		t.Fatalf("dialled %v, want the last-known-good 203.0.113.7:443", got)
	}
	if len(*reports) != 1 {
		t.Fatalf("%d fallbacks reported, want exactly 1", len(*reports))
	}
	f := (*reports)[0]
	if f.Host != "api.example.test" || !f.Connected {
		t.Fatalf("fallback report is %+v, want the host named and Connected", f)
	}
	if f.LookupErr == nil {
		t.Fatal("the fallback report carries no lookup error; the operator " +
			"question is 'is this the 2.5-hour wedge or is my network gone', " +
			"and only the error text answers it")
	}
}

// TestCachedDialerKeepsResolvedAddressForAtLeastOneHour is the FLOOR, and the
// floor is what makes the wedge survivable cheaply.
//
// Inside the hour the resolver is not consulted at all, so a wedge that begins
// during the window is invisible. `M-7ZT-SHORTTTL` shortens it, and the
// resulting defect is quiet: the harness goes back to asking a wedged resolver
// while holding a perfectly good answer.
func TestCachedDialerKeepsResolvedAddressForAtLeastOneHour(t *testing.T) {
	cd, r, _, clk, reports := newFixture(t)
	ctx := context.Background()

	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err != nil {
		t.Fatalf("seeding dial: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("%d lookups for the first dial, want 1", r.count())
	}

	// One second inside the floor, with the resolver wedged. Neither the lookup
	// nor a fallback alert may happen: as far as this dialer is concerned
	// nothing is wrong yet.
	//
	// THE DURATION IS WRITTEN OUT, and not derived from `FloorTTL`. Advancing
	// by `FloorTTL - time.Second` would step the clock in units of the very
	// constant under test, so shortening the floor would shorten the step with
	// it and every assertion below would keep passing against a cache that now
	// re-asks a wedged resolver every minute. `M-7ZT-SHORTTTL` survived exactly
	// that version of this test. §F6 says one hour, so the test says one hour.
	clk.advance(59*time.Minute + 59*time.Second)
	r.wedge(errors.New("lookup api.example.test: i/o timeout"))

	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err != nil {
		t.Fatalf("a dial inside the one-hour floor failed: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("the resolver was consulted %d times; inside the floor the "+
			"cached answer is used WITHOUT asking, which is what makes a wedge "+
			"beginning in this window invisible", r.count())
	}
	if len(*reports) != 0 {
		t.Fatalf("%d fallback alerts inside the floor, want 0: nothing had "+
			"failed", len(*reports))
	}

	// Past the boundary, a refresh is attempted -- and now the wedge is real,
	// so the cache stands in and says so.
	clk.advance(2 * time.Second)
	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err != nil {
		t.Fatalf("the dial past the floor failed: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("the resolver was consulted %d times, want 2: past the floor "+
			"a refresh must be attempted", r.count())
	}
	if len(*reports) != 1 {
		t.Fatalf("%d fallback alerts past the floor, want 1", len(*reports))
	}
}

// TestLiteralAddressesBypassTheCache keeps the cache from keying an entry on a
// string that is already its own answer.
func TestLiteralAddressesBypassTheCache(t *testing.T) {
	cd, r, d, _, _ := newFixture(t)
	if _, err := cd.DialContext(context.Background(), "tcp",
		"203.0.113.9:443"); err != nil {
		t.Fatalf("dialling a literal address: %v", err)
	}
	if r.count() != 0 {
		t.Fatalf("a literal address was resolved %d times", r.count())
	}
	if got := d.addresses(); len(got) != 1 || got[0] != "203.0.113.9:443" {
		t.Fatalf("dialled %v, want the literal unchanged", got)
	}
}

// TestAFailedCachedSetIsReResolvedEarly is the one case §F6 allows the floor to
// be cut short: the cached addresses stopped working, so they are stale in fact
// whatever the clock says.
func TestAFailedCachedSetIsReResolvedEarly(t *testing.T) {
	cd, r, d, _, _ := newFixture(t)
	ctx := context.Background()

	if _, err := cd.DialContext(ctx, "tcp", "api.example.test:443"); err != nil {
		t.Fatalf("seeding dial: %v", err)
	}
	// The cached address stops accepting connections, well inside the floor.
	d.mu.Lock()
	d.fail = true
	d.mu.Unlock()

	_, _ = cd.DialContext(ctx, "tcp", "api.example.test:443")

	if r.count() != 2 {
		t.Fatalf("the resolver was consulted %d times, want 2: a cached set "+
			"that will not connect is stale in fact, whatever the clock says",
			r.count())
	}
}

// TestCachedDialerRequiresEveryCollaborator refuses a dialer whose fallback
// path could not be exercised.
func TestCachedDialerRequiresEveryCollaborator(t *testing.T) {
	d := &recordingDial{}
	clk := &testClock{}
	for _, tc := range []struct {
		name   string
		r      Resolver
		dial   DialFunc
		now    func() time.Time
		report Reporter
	}{
		{"no resolver", nil, d.fn, clk.now, func(Fallback) {}},
		{"no dial", &fakeResolver{}, nil, clk.now, func(Fallback) {}},
		{"no clock", &fakeResolver{}, d.fn, nil, func(Fallback) {}},
		{"no reporter", &fakeResolver{}, d.fn, clk.now, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCachedDialer(tc.r, tc.dial, tc.now,
				tc.report); err == nil {
				t.Fatal("accepted; a nil collaborator is a fallback path that " +
					"cannot be exercised, which is the same as not having one")
			}
		})
	}
}

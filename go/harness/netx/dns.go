// Package netx is F6: surviving a resolver that stops answering.
//
// THE FAILURE IS MEASURED, NOT HYPOTHETICAL. On the machine this harness runs
// on, `getaddrinfo` wedges system-wide roughly every 2.5 hours, and `nslookup`
// keeps working straight through it -- which is exactly why it went unnoticed
// long enough to be characterised. `nslookup` talks to the DNS server directly;
// the process resolves through the system stack, and it is the system stack
// that stops. Any diagnostic that reaches for `nslookup` will report a healthy
// resolver during the outage.
//
// H-DEP-1 calls Go's pure-Go resolver's immunity to this a HYPOTHESIS. It is
// not established, and a hypothesis is not a fallback: a read-only
// qualification run is 4-6 hours, so it meets the wedge once or twice by
// arithmetic, and without this package the harness's answer is to fail every
// connection until the machine recovers.
//
// WHAT THE SPEC ASKS FOR (§F6): fall back to the last-known-good IP with SNI
// PRESERVED, hold cached resolutions for a floor of one hour, and raise a
// queued SEV2. The SNI clause is the whole subtlety and it is why this is a
// dialer rather than a URL rewrite -- see `DialContext`.
package netx

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// FloorTTL is §F6's one-hour floor on a cached resolution.
//
// It is a FLOOR and not an expiry: inside the hour the cached addresses are
// used without consulting the resolver at all, so a wedge that begins during
// that window is invisible to us, which is the point. `M-7ZT-SHORTTTL` reduces
// it, and the failure that produces is subtle -- the harness starts asking a
// wedged resolver again while it still holds a perfectly good answer.
const FloorTTL = time.Hour

// Resolver is the lookup surface, injected so a test can wedge it.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DialFunc is the underlying TCP dial, injected for the same reason.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Fallback is one report of the cache standing in for a resolver that failed.
//
// It carries the lookup error rather than a summary of it because the operator
// question -- "is this the 2.5-hour wedge, or is my network gone?" -- is
// answered by the error text and by nothing else.
type Fallback struct {
	Host string
	// Address is the cached destination that was dialled, host:port.
	Address string
	// LookupErr is why the resolver could not answer.
	LookupErr error
	// Connected reports whether the fallback dial actually succeeded. A
	// fallback that also failed is a different operational fact from one that
	// carried the harness through, and collapsing them would hide the case
	// where the cached address has genuinely gone away.
	Connected bool
}

// Reporter receives every fallback. It MUST NOT block: it is called from inside
// the dial path, so a slow reporter is a slow connection, and F6's whole
// purpose is to keep connecting while something else is broken.
type Reporter func(Fallback)

type entry struct {
	addrs      []string
	resolvedAt time.Time
}

// CachedDialer resolves through `resolver`, remembers the answer, and dials the
// remembered answer when the resolver stops answering.
type CachedDialer struct {
	resolver Resolver
	dial     DialFunc
	now      func() time.Time
	report   Reporter

	mu      sync.Mutex
	entries map[string]entry
}

// NewCachedDialer requires every collaborator. There are no defaults, because
// each of them is a thing a test has to be able to break: a resolver that
// cannot be wedged, a clock that cannot be advanced past the floor, or a dial
// that cannot fail would each make one of F6's clauses untestable.
func NewCachedDialer(r Resolver, d DialFunc, now func() time.Time,
	report Reporter) (*CachedDialer, error) {

	if r == nil || d == nil || now == nil || report == nil {
		return nil, fmt.Errorf("a cached dialer needs a resolver, a dial, a " +
			"clock and a reporter; a nil one would be a fallback path that " +
			"cannot be exercised, which is the same as not having one")
	}
	return &CachedDialer{resolver: r, dial: d, now: now, report: report,
		entries: make(map[string]entry)}, nil
}

// DialContext is the `net.Dialer.DialContext` shape, so it drops into
// `http.Transport.DialContext` unchanged.
//
// **IT SUBSTITUTES THE TCP DESTINATION AND NOTHING ELSE.** That is the entire
// reason F6 is implemented here rather than by rewriting a URL to an IP. The
// request's `Host` header and the TLS `ServerName` are derived by
// `net/http` and `crypto/tls` from the URL, which this never sees -- so the
// certificate is still verified against the real hostname and SNI still carries
// it. Rewriting the URL would send the numeric address as both, and the
// handshake would fail against a certificate that was never issued for an IP.
// `M-7ZT-IPHOST` makes exactly that substitution.
func (c *CachedDialer) DialContext(ctx context.Context, network,
	address string) (net.Conn, error) {

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	// A literal address was never resolved and has nothing to fall back to.
	// Passing it through the cache would key an entry on a string that is
	// already its own answer.
	if net.ParseIP(host) != nil {
		return c.dial(ctx, network, address)
	}

	cached, held := c.lookupCache(host)

	// INSIDE THE FLOOR: use what we have and do not ask. A resolver that wedges
	// during this window never gets consulted, which is the cheapest possible
	// form of surviving it.
	if held && c.now().Sub(cached.resolvedAt) < FloorTTL {
		conn, err := c.dialAll(ctx, network, cached.addrs, port)
		if err == nil {
			return conn, nil
		}
		// The cached set stopped working, so the answer is stale in fact
		// whatever the clock says. This is the one case §F6 allows an early
		// re-resolve.
	}

	addrs, lookupErr := c.resolver.LookupHost(ctx, host)
	if lookupErr == nil && len(addrs) > 0 {
		c.store(host, addrs)
		return c.dialAll(ctx, network, addrs, port)
	}
	if lookupErr == nil {
		lookupErr = fmt.Errorf("resolver returned no addresses for %s", host)
	}

	// THE FALLBACK, and it requires a PRIOR SUCCESS. With no cached answer
	// there is nothing to fall back TO, and inventing one -- a guess, a
	// hard-coded address, a different host -- would be a connection to
	// somewhere we have never verified. The lookup error is returned as-is.
	// `M-7ZT-NOFALLBACK` returns it even when a cached answer exists.
	if !held {
		return nil, lookupErr
	}

	conn, dialErr := c.dialAll(ctx, network, cached.addrs, port)
	c.report(Fallback{
		Host:      host,
		Address:   net.JoinHostPort(firstOr(cached.addrs, ""), port),
		LookupErr: lookupErr,
		Connected: dialErr == nil,
	})
	if dialErr != nil {
		return nil, fmt.Errorf("resolution failed (%w) and the last-known-good "+
			"address for %s did not connect either: %v", lookupErr, host, dialErr)
	}
	return conn, nil
}

// dialAll tries each address in order and returns the first connection.
func (c *CachedDialer) dialAll(ctx context.Context, network string,
	addrs []string, port string) (net.Conn, error) {

	var last error
	for _, a := range addrs {
		conn, err := c.dial(ctx, network, net.JoinHostPort(a, port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no addresses to dial")
	}
	return nil, last
}

func (c *CachedDialer) lookupCache(host string) (entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[host]
	return e, ok
}

func (c *CachedDialer) store(host string, addrs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]string, len(addrs))
	copy(cp, addrs)
	c.entries[host] = entry{addrs: cp, resolvedAt: c.now()}
}

func firstOr(s []string, d string) string {
	if len(s) == 0 {
		return d
	}
	return s[0]
}

// SystemResolver is the pure-Go resolver H-DEP-1 hypothesises is immune.
//
// `PreferGo` is set because the cgo resolver IS `getaddrinfo`, which is the
// thing that wedges. The hypothesis is that the pure-Go path talks to the
// nameservers directly and survives -- and this package exists precisely
// because that is a hypothesis: the cache below it does not care which resolver
// failed or why.
func SystemResolver() Resolver { return &net.Resolver{PreferGo: true} }

// confidence: high

package ping

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// HTTPSDeadman is §13.4's EXTERNAL dead man: a third-party endpoint that expects
// a check-in on every scheduled heartbeat and alarms when one does not arrive.
//
// External and not internal, and that is the whole idea. Every failure detector
// inside this process shares its fate: a wedged goroutine, a SIGKILL, a host
// that went to sleep and a laptop lid all silence the harness and its own
// alarms together. F18 is "the harness stopped and nobody noticed", and only
// something that is not the harness can notice it.
//
// The endpoint URL is a BEARER credential -- anyone holding it can suppress the
// alarm indefinitely -- so it is held behind a closure, has no `String` method,
// and never appears in a body, a title, a log line or a database column.
type HTTPSDeadman struct {
	reveal func() string
	http   *http.Client
}

// NewHTTPSDeadman validates the check-in endpoint.
//
// HTTPS is required rather than preferred: a plaintext check-in URL is a bearer
// credential broadcast on every heartbeat, and an on-path attacker who wanted
// the harness to look alive would only have to answer it.
func NewHTTPSDeadman(endpoint string) (*HTTPSDeadman, error) {
	if endpoint == "" {
		return nil, errors.New("no dead-man endpoint: §13.4's check-in is the " +
			"only detector of F18 that survives this process dying, and there " +
			"is deliberately no production no-op to fall back to")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("the dead-man endpoint does not parse as a "+
			"URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("the dead-man endpoint uses scheme %q; it must "+
			"be https, because the URL is a bearer credential sent on every "+
			"heartbeat", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("the dead-man endpoint names no host")
	}
	return &HTTPSDeadman{
		reveal: func() string { return endpoint },
		http:   newBearerClient(),
	}, nil
}

// valid reports whether this dead man was built by a constructor. The zero
// value is constructible from any package and must be refused, not dereferenced.
func (d *HTTPSDeadman) valid() bool {
	return d != nil && d.reveal != nil && d.http != nil
}

// errNoDeadman is fixed text. It names nothing.
var errNoDeadman = errors.New("this dead man was not built by " +
	"NewHTTPSDeadman: it checks in with nothing, and §13.4's only detector of " +
	"F18 that survives this process dying would be silently absent")

// CheckIn tells the external watchdog the harness is alive.
//
// A failure is REPORTED and never fatal. The check-in exists so somebody else
// notices we are gone; treating a failed check-in as a reason to stop would
// hand the watchdog authority over a process that is holding inventory.
func (d *HTTPSDeadman) CheckIn(ctx context.Context) error {
	if !d.valid() {
		return errNoDeadman
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.reveal(), nil)
	if err != nil {
		return transportError("building the dead-man check-in", err)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		// NEVER wrapped: the error carries the check-in URL, and anyone
		// holding that URL can report a dead harness alive indefinitely.
		return transportError("the dead-man check-in", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the dead-man endpoint answered %s; the check-in "+
			"did not register", resp.Status)
	}
	return nil
}

// confidence: high

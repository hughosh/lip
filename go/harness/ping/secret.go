package ping

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Bearer secrets
// ---------------------------------------------------------------------------

// Topic is the ntfy topic, and it is a BEARER credential: knowing it is
// sufficient both to read every alert this harness sends and to forge one.
//
// It holds its value behind a closure rather than in a string field, and that is
// not decoration. A struct with an unexported string field is printed in full by
// `fmt.Sprintf("%v", topic)`, which is the single most likely way a secret
// reaches a log -- somebody adds the sender to a debug print. A func field
// prints as an address. There is deliberately no `String` method for the same
// reason: providing one invites `%s`, and the only correct rendering is not to
// render it.
//
// The zero value is INVALID and reveals nothing.
type Topic struct {
	reveal func() string
}

// Valid reports whether this topic was loaded from an env file.
func (t Topic) Valid() bool { return t.reveal != nil }

// LoadNTFYTopic reads `NTFY_TOPIC` from an explicit ABSOLUTE env-file path.
//
// Explicit and absolute, never `os.Getenv` and never a path relative to the
// working directory. A process environment is inherited by every child and is
// visible in a process listing on some systems; a relative path makes which
// credential is loaded depend on where the harness was started from, and the
// failure mode of loading the wrong one is alerts that go to a channel nobody
// watches while the harness reports delivery succeeded.
//
// It rejects an absent entry, an empty value, and a DUPLICATE entry. The
// duplicate case is the one worth naming: two `NTFY_TOPIC` lines mean the file
// has been edited to point somewhere else and the old line was left behind, and
// silently taking either one picks a channel by shell semantics rather than by
// intent.
func LoadNTFYTopic(path string) (Topic, error) {
	value, err := readEnvValue(path, "NTFY_TOPIC")
	if err != nil {
		return Topic{}, err
	}
	if err := validTopic(value); err != nil {
		return Topic{}, err
	}
	return Topic{reveal: func() string { return value }}, nil
}

// LoadHTTPSDeadman reads `DEADMAN_URL` from an explicit ABSOLUTE env-file path
// and constructs the sealed production check-in transport.
//
// The endpoint is a bearer credential: anyone holding it can report a dead
// harness alive. It therefore follows the same source rule as NTFY_TOPIC -- no
// process environment, no relative file and exactly one non-empty entry -- and
// is handed directly to NewHTTPSDeadman without ever being included in an
// error. NewHTTPSDeadman's validation errors are secret-free even when URL
// parsing fails, which is why this loader returns them unchanged rather than
// wrapping them with the value it was trying to load.
func LoadHTTPSDeadman(path string) (*HTTPSDeadman, error) {
	endpoint, err := readEnvValue(path, "DEADMAN_URL")
	if err != nil {
		return nil, err
	}
	return NewHTTPSDeadman(endpoint)
}

// validTopic rejects anything that would change the destination path.
//
// The topic becomes a URL path segment. A value containing a slash, a query
// character or whitespace does not merely look wrong: it silently redirects the
// alert to a different topic, or to a different endpoint entirely, and the
// harness would report every delivery successful.
func validTopic(v string) error {
	if v == "" {
		return errors.New("NTFY_TOPIC is empty; an empty topic addresses no " +
			"channel and every alert would be delivered to nowhere while " +
			"reporting success")
	}
	for _, r := range v {
		ok := (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("NTFY_TOPIC contains a character outside "+
				"[A-Za-z0-9_-] (at byte offset %d); the topic is a URL path "+
				"segment, and anything else redirects the alert somewhere the "+
				"operator is not watching", strings.IndexRune(v, r))
		}
	}
	return nil
}

// readEnvValue parses a shell-style env file for exactly one key.
//
// It handles `export KEY=value`, `#` comments, and single or double quoting,
// because those are the shapes `~/.kalshi/env` already takes for this repository.
// It does NOT expand variables or run anything: an env file that can execute is
// a credential file that can execute.
func readEnvValue(path, key string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("env file path %q is not absolute; which "+
			"credential is loaded must not depend on the working directory of "+
			"whatever started the process", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("the env file holding %s could not be read: %w",
			key, err)
	}
	defer f.Close()

	var (
		value string
		found int
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		found++
		value = unquote(strings.TrimSpace(raw))
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	switch {
	case found == 0:
		return "", fmt.Errorf("%s is not set in %s; §13 has no fallback "+
			"channel, so a harness that cannot address the operator must fail "+
			"to start rather than run unattended and silent", key, path)
	case found > 1:
		return "", fmt.Errorf("%s appears %d times in %s; two values mean the "+
			"file was edited and the old line left behind, and choosing "+
			"between them by shell precedence picks a channel by accident",
			key, found, path)
	}
	return value, nil
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') ||
			(s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// The bearer transport
// ---------------------------------------------------------------------------

// bearerTimeout bounds one delivery attempt. A push that hangs holds the
// caller's Step, and §13's alerts are only useful promptly.
const bearerTimeout = 10 * time.Second

// refuseRedirect is why both live transports use the same client factory.
//
// Both requests carry a bearer credential IN THE URL -- the ntfy topic is the
// path, the dead-man check-in is the whole endpoint. Go's default client
// re-sends a redirected request to the new host, so a compromised or merely
// misconfigured DNS answer turns one 302 into a credential disclosure, and the
// harness would report the delivery successful. `M-P-REDIRECT` restores the
// default.
//
// The message names NEITHER url. Reporting where the redirect pointed would
// publish the attacker-chosen host, and the request that was redirected is the
// one carrying the credential -- so an error written to help debug the redirect
// is a second disclosure through the log instead of the wire.
func refuseRedirect(req *http.Request, via []*http.Request) error {
	return errRedirect
}

var errRedirect = errors.New("refusing to follow a redirect: this request " +
	"carries a bearer credential in its URL, and following the redirect would " +
	"disclose it to whatever answered")

func newBearerClient() *http.Client {
	return &http.Client{
		Timeout:       bearerTimeout,
		CheckRedirect: refuseRedirect,
	}
}

// ---------------------------------------------------------------------------
// Secret-free transport errors
// ---------------------------------------------------------------------------

// transportError classifies a failed request WITHOUT quoting anything from it.
//
// `http.Client.Do` returns a `*url.Error`, whose `Error()` is `Post "<the whole
// URL>": <cause>` -- so returning or wrapping it publishes the ntfy topic or the
// dead-man endpoint to every caller that logs an error. The cause underneath is
// usually a `*net.OpError` carrying the host and port, so unwrapping once is not
// enough either.
//
// The classification is therefore taken from the error's TYPE and from the
// sentinels, never from its text. What survives is the distinction an operator
// can act on -- cancelled, timed out, could not connect, refused a redirect --
// and nothing that identifies where.
func transportError(what string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s was cancelled before it completed", what)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s exceeded its deadline", what)
	case errors.Is(err, errRedirect):
		return fmt.Errorf("%s was answered with a redirect and was refused: "+
			"the request carries a bearer credential in its URL", what)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return fmt.Errorf("%s timed out", what)
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return fmt.Errorf("%s could not %s", what, oe.Op)
	}
	return fmt.Errorf("%s failed in transport", what)
}

// confidence: high

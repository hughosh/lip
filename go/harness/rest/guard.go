package rest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// H-VER-1 — structural read-only/live write arming.
//
// The harness is a complete, runnable pilot binary that has never been pointed
// at the live account, and the thing standing between a rehearsal and a real
// order has been operator discipline: don't run it. That is not a structure, it
// is an intention, and it fails silently the first time someone runs the wrong
// invocation from the wrong shell.
//
// `WriteGuard` makes read-only a property of the PROCESS rather than of the
// operator's care. It sits at the one place every production REST call crosses
// -- the `Doer` inside `newRig`, below startup, adoption sweeps and the
// dispatcher alike -- so there is no path to the exchange that bypasses it. A
// guard installed at `Create`, at `main`, or in the dispatcher would each leave
// the other three reachable.
//
// TWO KEYS, AND THEY ARE DELIBERATELY DIFFERENT IN KIND. Arming requires an
// explicit `-live` on the command line AND a sentinel file on disk. The flag is
// an intention expressed at launch; the sentinel is a fact about the machine
// that an operator can revoke without finding, stopping and relaunching the
// process. One key alone is a single typo away from a live order: `-live` on
// its own turns a copy-pasted command into real money, and a sentinel on its
// own arms every future invocation on that host forever.

// WriteArm is the two-key arming state. Its ZERO VALUE IS READ-ONLY, which is
// the only safe default: a config that forgot to mention writes, a test that
// constructs a client directly, and a caller that has not been updated all get
// a process that cannot place an order.
type WriteArm struct {
	// Live is the explicit `-live` invocation flag. Deliberately NOT a JSON
	// config key: a file that arms itself is a file that arms every process
	// that reads it, including the one an operator started to look at a book.
	Live bool
	// LiveOKPath is the sentinel whose PRESENCE is the second key. It is
	// checked freshly on every write, never at startup, so `rm` disarms the
	// next one.
	LiveOKPath string
}

// WriteRefused is a write the guard stopped before it reached the network.
//
// It is a distinct exported type rather than a plain error because the whole
// value of refusing is being able to prove the refusal. H-ORD-2 turns on whether
// a request was SENT: an order we might have placed is `UNKNOWN` and keeps its
// full size in every aggregate cap (clause 6), so a refusal misread as ambiguous
// would consume the pilot's capital budget with orders that were never even
// transmitted. `WasSent` therefore recognises this alongside `NotSent`.
type WriteRefused struct {
	Method string
	Path   string
	// Sentinel is the path that was consulted, so the operator is told which
	// file to create rather than being told "refused".
	SentinelPath string
	// Reason is why, in the operator's terms.
	Reason string
}

func (e *WriteRefused) Error() string {
	return fmt.Sprintf("write refused before transmission: %s %s (%s); this "+
		"process is READ-ONLY and nothing was sent to the exchange",
		e.Method, e.Path, e.Reason)
}

// WriteGuard wraps a Doer and refuses every non-GET request unless both keys
// are present.
type WriteGuard struct {
	next Doer
	arm  WriteArm
}

// NewWriteGuard wraps next. A nil next is an error and never a default: a guard
// that silently supplied its own transport would be a guard that decides where
// the writes go.
func NewWriteGuard(next Doer, arm WriteArm) (*WriteGuard, error) {
	if next == nil {
		return nil, errors.New("a write guard needs a Doer to wrap; there is " +
			"no default transport, because a guard that supplied one would be " +
			"choosing which exchange the harness talks to")
	}
	if arm.Live && arm.LiveOKPath == "" {
		return nil, errors.New("-live was given with no live_ok path " +
			"configured, so the second key cannot be checked; arming on one " +
			"key is exactly what H-VER-1 forbids")
	}
	return &WriteGuard{next: next, arm: arm}, nil
}

// Do delegates a GET and gates everything else.
//
// The method test is EXACT equality with "GET" rather than a "not a write"
// list. A list has to be kept complete, and the failure mode of an incomplete
// one is that an unlisted method is treated as safe -- so the default answer for
// anything unrecognised must be "refuse". `M-ES6-FLAG` and `M-ES6-SENTINEL`
// remove one key each.
func (g *WriteGuard) Do(ctx context.Context, req Request) (Response, error) {
	if req.Method == "GET" {
		// Reads never consult the sentinel. A read-only rehearsal must reach
		// every truth the live process reaches -- positions, orders, fills, the
		// book -- or it is not a rehearsal of anything.
		return g.next.Do(ctx, req)
	}
	if !g.arm.Live {
		return Response{}, &WriteRefused{
			Method: req.Method, Path: req.Path,
			SentinelPath: g.arm.LiveOKPath,
			Reason:       "this process was not started with -live",
		}
	}
	if err := g.sentinelErr(); err != nil {
		return Response{}, &WriteRefused{
			Method: req.Method, Path: req.Path,
			SentinelPath: g.arm.LiveOKPath,
			Reason:       err.Error(),
		}
	}
	// Delegated UNCHANGED. The guard decides whether a request goes, never what
	// goes: rewriting a method, a path, a query or a body here would make the
	// armed path differ from the tested one in exactly the place nothing else
	// checks. `M-ES6-...` aside, this is why the test asserts byte equality.
	return g.next.Do(ctx, req)
}

// sentinelErr is the freshness seam: it is called PER WRITE and holds no state.
//
// It exists as its own method so that caching is a visible, one-line change
// rather than something that could be introduced by accident -- see
// `M-ES6-RECHECK`, which is exactly that one line.
func (g *WriteGuard) sentinelErr() error { return checkSentinel(g.arm.LiveOKPath) }

// checkSentinel stats the file FRESHLY, every time.
//
// `M-ES6-RECHECK` caches the answer in the constructor, which reads as an
// obvious optimisation and destroys the property the sentinel exists for: an
// operator who deletes the file expects the NEXT write to stop, without having
// to find and kill a process that may be mid-cycle. A cached bool means the
// only way to disarm is a restart, which is the thing you cannot do calmly
// while an unexpected order is resting.
func checkSentinel(path string) error {
	if path == "" {
		return errors.New("no live_ok sentinel is configured")
	}
	if !filepath.IsAbs(path) {
		// A relative sentinel resolves against the working directory, so the
		// same config arms or disarms depending on where the process was
		// started from. That is not a key, it is a coin flip.
		return fmt.Errorf("the live_ok sentinel %q is not an absolute path", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the live_ok sentinel %s is absent", path)
	}
	if !st.Mode().IsRegular() {
		// A directory is the likely accident -- `mkdir -p` on the wrong path --
		// and it would otherwise arm the process.
		return fmt.Errorf("the live_ok sentinel %s is not a regular file", path)
	}
	return nil
}

// confidence: high

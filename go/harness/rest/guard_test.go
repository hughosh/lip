package rest

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// recordingDoer records every request that reached it. The assertions below are
// mostly about ABSENCE -- that nothing arrived -- which is the only way to state
// "this process cannot trade" as a fact rather than as a hope.
type recordingDoer struct {
	seen []Request
	resp Response
	err  error
}

func (d *recordingDoer) Do(_ context.Context, req Request) (Response, error) {
	d.seen = append(d.seen, req)
	return d.resp, d.err
}

func sentinel(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "live_ok")
	if err := os.WriteFile(p, []byte("armed\n"), 0o600); err != nil {
		t.Fatalf("writing the sentinel: %v", err)
	}
	return p
}

func guard(t *testing.T, d Doer, arm WriteArm) *WriteGuard {
	t.Helper()
	g, err := NewWriteGuard(d, arm)
	if err != nil {
		t.Fatalf("NewWriteGuard: %v", err)
	}
	return g
}

// TestWriteGuardTruthTable is H-VER-1 stated as the four states it has.
//
// Both keys are required, and the test is written as the full cross product
// rather than as "the happy path plus one refusal" because the defect this
// prevents is a guard that checks one key and reads as if it checked two.
// `M-ES6-FLAG` and `M-ES6-SENTINEL` each delete one, and only a truth table
// notices which.
func TestWriteGuardTruthTable(t *testing.T) {
	for _, method := range []string{"POST", "DELETE"} {
		for _, tc := range []struct {
			name    string
			live    bool
			present bool
			want    bool // want delegated
		}{
			{"neither key", false, false, false},
			{"flag only", true, false, false},
			{"sentinel only", false, true, false},
			{"both keys", true, true, true},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "live_ok")
				if tc.present {
					if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
						t.Fatalf("sentinel: %v", err)
					}
				}
				d := &recordingDoer{resp: Response{Status: 201}}
				g := guard(t, d, WriteArm{Live: tc.live, LiveOKPath: path})

				_, err := g.Do(context.Background(),
					Request{Method: method, Path: "/portfolio/orders"})

				if tc.want {
					if err != nil {
						t.Fatalf("an armed %s was refused: %v", method, err)
					}
					if len(d.seen) != 1 {
						t.Fatalf("an armed %s did not reach the transport",
							method)
					}
					return
				}
				var wr *WriteRefused
				if !errors.As(err, &wr) {
					t.Fatalf("%s with %s produced %v, want WriteRefused",
						method, tc.name, err)
				}
				if len(d.seen) != 0 {
					t.Fatalf("a refused %s REACHED the transport with %s. The "+
						"refusal is worthless if the request went anyway",
						method, tc.name)
				}
				if WasSent(err) {
					t.Fatal("a refused write reads as SENT, so a create would " +
						"become UNKNOWN and hold its full size in every " +
						"aggregate cap for an order that was never transmitted")
				}
			})
		}
	}
}

// TestGETAlwaysPassesWithoutWriteKeys is the other half of being useful.
//
// A read-only rehearsal has to reach every truth the live process reaches --
// positions, orders, fills, the book -- or it rehearses nothing. So reads never
// consult the sentinel, and the disarmed process is a COMPLETE observer.
func TestGETAlwaysPassesWithoutWriteKeys(t *testing.T) {
	d := &recordingDoer{resp: Response{Status: 200, Body: []byte(`{}`)}}
	g := guard(t, d, WriteArm{}) // zero value: read-only, no sentinel at all

	for _, p := range []string{"/portfolio/positions", "/portfolio/orders",
		"/portfolio/fills", "/portfolio/balance"} {

		if _, err := g.Do(context.Background(),
			Request{Method: "GET", Path: p}); err != nil {
			t.Fatalf("GET %s was refused by a read-only process: %v", p, err)
		}
	}
	if len(d.seen) != 4 {
		t.Fatalf("%d of 4 reads reached the transport", len(d.seen))
	}
}

// TestArmedWriteIsPassedByteForByte pins that the guard decides WHETHER, never
// WHAT.
//
// Rewriting a method, path, query or body here would make the armed path differ
// from the rehearsed one at the exact point nothing downstream re-checks -- the
// signature covers the path without the query (see `Request`), so a guard that
// touched either would produce a 401 that reads as a credential fault.
func TestArmedWriteIsPassedByteForByte(t *testing.T) {
	d := &recordingDoer{resp: Response{Status: 201}}
	g := guard(t, d, WriteArm{Live: true, LiveOKPath: sentinel(t)})

	want := Request{
		Method: "POST",
		Path:   "/portfolio/orders",
		Query:  url.Values{"ticker": []string{"KXTEST-A"}},
		Body:   []byte(`{"client_order_id":"lipH-1","count":"0.01"}`),
	}
	if _, err := g.Do(context.Background(), want); err != nil {
		t.Fatalf("armed write: %v", err)
	}
	got := d.seen[0]
	if got.Method != want.Method || got.Path != want.Path {
		t.Fatalf("the guard rewrote the request line: %s %s", got.Method, got.Path)
	}
	if got.Query.Encode() != want.Query.Encode() {
		t.Fatalf("the guard rewrote the query: %q", got.Query.Encode())
	}
	if string(got.Body) != string(want.Body) {
		t.Fatalf("the guard rewrote the body:\n got %s\nwant %s",
			got.Body, want.Body)
	}
}

// TestRemovingLiveOKDisarmsTheNextWrite is the property that makes the sentinel
// worth having over a second flag.
//
// An operator watching something go wrong must be able to stop the next order
// with `rm`, without finding the process, without a signal, and without waiting
// for it to notice. `M-ES6-RECHECK` caches presence in the constructor -- an
// obvious-looking optimisation -- and the only remaining way to disarm becomes a
// restart, which is precisely what nobody wants to do calmly while an unexpected
// order is resting.
func TestRemovingLiveOKDisarmsTheNextWrite(t *testing.T) {
	path := sentinel(t)
	d := &recordingDoer{resp: Response{Status: 201}}
	g := guard(t, d, WriteArm{Live: true, LiveOKPath: path})

	if _, err := g.Do(context.Background(),
		Request{Method: "POST", Path: "/portfolio/orders"}); err != nil {
		t.Fatalf("the first armed write was refused: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the sentinel: %v", err)
	}

	_, err := g.Do(context.Background(),
		Request{Method: "POST", Path: "/portfolio/orders"})
	var wr *WriteRefused
	if !errors.As(err, &wr) {
		t.Fatalf("the write after `rm live_ok` was still armed (%v). The "+
			"sentinel is checked once and cached, so the only way to stop the "+
			"harness writing is to kill it", err)
	}
	if len(d.seen) != 1 {
		t.Fatalf("%d writes reached the transport, want 1 before the disarm",
			len(d.seen))
	}
}

// TestLiveOKPathMustBeAbsoluteAndRegular refuses the two shapes that look armed
// and are not a key.
func TestLiveOKPathMustBeAbsoluteAndRegular(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		d := &recordingDoer{}
		g := guard(t, d, WriteArm{Live: true, LiveOKPath: "live_ok"})
		_, err := g.Do(context.Background(), Request{Method: "POST"})
		if err == nil || len(d.seen) != 0 {
			t.Fatal("a relative sentinel armed the guard; it resolves against " +
				"the working directory, so the same config arms or disarms " +
				"depending on where the process was started")
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "live_ok")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		d := &recordingDoer{}
		g := guard(t, d, WriteArm{Live: true, LiveOKPath: dir})
		_, err := g.Do(context.Background(), Request{Method: "POST"})
		if err == nil || len(d.seen) != 0 {
			t.Fatal("a directory armed the guard; `mkdir -p` on the sentinel " +
				"path is the likely accident and it must not count")
		}
	})
}

// TestGuardRefusesToWrapNothing keeps the guard from choosing a transport.
func TestGuardRefusesToWrapNothing(t *testing.T) {
	if _, err := NewWriteGuard(nil, WriteArm{}); err == nil {
		t.Fatal("a nil Doer was accepted; a guard that supplies its own " +
			"transport is choosing which exchange the harness talks to")
	}
	if _, err := NewWriteGuard(&recordingDoer{},
		WriteArm{Live: true}); err == nil {
		t.Fatal("-live with no sentinel path was accepted, which is arming on " +
			"one key")
	}
}

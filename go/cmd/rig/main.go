// Command rig is the live read-only measurement rig for Kalshi LIP markets.
//
// Port of rig.py's `run` and `resolve_mids` (rig.py:379-569). Everything that
// decides what a row contains lives in core; this file owns the socket, the
// clock and the tape.
//
// Nothing here places, amends, or cancels an order. The only credential use is
// the websocket handshake, which Kalshi requires even for public channels.
//
// # CONCURRENCY
//
// The Python is a single asyncio event loop: the frame handler and the
// forward-mid resolver never run at the same time, and both touch the same
// books and the same sqlite connection. That is reproduced here by ONE owner
// goroutine selecting over the frame channel and the resolver's timer. The only
// other goroutine reads the socket; it never touches a book, a row, the DB, or
// the tape.
//
// The tape is written by the OWNER, immediately before Handle. Writing it in the
// reader would let capture run thousands of frames ahead of processing, so a
// shutdown would leave the tape holding frames that were never handled — and
// replaying that tape would produce rows the live run never wrote.
//
// One divergence is accepted rather than fixed: asyncio can run the resolver
// while the main task is suspended at `await connect()`, `await ws.send()` or
// the close handshake, and this owner cannot. It affects only which live book a
// horizon reads, i.e. mid_1m/5m/30m — columns the differential gate excludes by
// construction because they are wall-clock derived and not a function of the
// tape. Two live runs can never agree on them anyway. See port-spec.md §6.
//
// Usage:
//
//	rig                      # stream until interrupted
//	rig --duration 3600      # stop after an hour
//	rig --db /tmp/shadow.db  # write somewhere other than ./rig.db
//
// `rig --report` is deliberately absent: rig.py --report --db <godb> reads a
// Go-written database unchanged, because the schema is byte-identical.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"lip/core"
	"lip/feed"
	"lip/store"
	"lip/tape"
)

// errDeadline unwinds the session loop when --duration expires. It is not a
// failure and must never reach the reconnect path.
var errDeadline = errors.New("duration reached")

// errTerminated unwinds when SIGTERM is latched mid-frame. Like errDeadline it
// is not a failure, and it must never reach the reconnect path — that path
// commits.
var errTerminated = errors.New("terminated")

type app struct {
	rig     *core.Rig
	store   *store.Store
	signer  *feed.Signer
	raw     *tape.Writer
	tickers []string

	// Wall-clock seconds, as CPython floats, NOT time.Time — see pyTime. Go's
	// time.Time carries a monotonic reading that Python's time.time() does not,
	// and a clock step would move one schedule and not the other.
	deadline    float64
	hasDeadline bool

	lastCommit float64
	lastReport float64

	// Latched by the signal goroutine BEFORE the context is cancelled, so
	// shutdown can tell the two signals apart without racing the cancellation.
	//
	// `mu` serialises the latch against every commit decision. Ordering the
	// store alone is not enough: a frame branch that reads the latch as false
	// and is then pre-empted would still start a commit that Python's SIGTERM
	// would have prevented. Every commit takes the lock and re-reads the latch
	// inside it, so "terminated" and "committing" cannot both be true.
	//
	// EXACT reproduction is impossible in principle, and that is worth stating
	// rather than pretending otherwise. Python's SIGTERM kills the interpreter
	// at whatever bytecode it happens to be executing — possibly mid-commit —
	// so its transaction boundary is not deterministic either. The goal is the
	// same CLASS of boundary, not the same instruction.
	mu         sync.Mutex
	sawSigterm bool

	// A one-shot timer, reset only after each pass RETURNS. Python's resolver is
	// `while True: await asyncio.sleep(5); ...`, so the period is 5s PLUS the
	// pass duration and the phase re-arms from completion. A time.Ticker is
	// phase-locked to its creation and coalesces ticks it missed while blocked,
	// so after a slow pass the two schedules drift apart and a horizon resolves
	// against a different live book.
	resolve *time.Timer

	// Set once the resolver has failed. Python's resolve_mids has no exception
	// handler, so the first DB error kills that task permanently while the
	// websocket loop carries on. Retrying would resolve horizons Python leaves
	// pending forever.
	resolverDead bool

	backoff time.Duration
}

const resolveInterval = 5 * time.Second

// commit is the ONLY path that may commit the shared transaction.
//
// The latch is re-read while holding the lock, so a SIGTERM cannot land between
// "we are not terminating" and the commit itself. Every caller goes through
// here — the periodic commit, the resolver, and the reconnect path — because
// rig.py:407's conn.commit() commits the WHOLE transaction, not just the
// resolver's own updates (§6a.1), so any unserialised commit is a hole.
//
// Returns false when the commit was suppressed by termination.
func (a *app) commit() (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sawSigterm {
		return false, nil
	}
	return true, a.store.Commit()
}

// terminating reports whether a SIGTERM has been latched.
func (a *app) terminating() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sawSigterm
}

// pyTimeSeconds reproduces CPython's _PyTime_AsSecondsDouble, which is how
// time.time() turns the clock into a float.
//
// The construction matters and is not the obvious one. CPython forms a SINGLE
// int64 nanosecond value and converts THAT to double; it does not convert
// seconds and nanoseconds separately and add them. The two disagree in the last
// ulp, in both directions:
//
//	(1784934732, 999808 ns)   CPython 1784934732001   sec+nsec/1e9  1784934732000
//	(1784934732, 1999975 ns)  CPython 1784934732001   sec+nsec/1e9  1784934732002
//
// The whole-second fast path is CPython's too, and is not merely an
// optimisation: (double)(t/1e9) and (double)t/1e9 need not agree.
//
//	if (t % SEC_TO_NS == 0) { secs = t / SEC_TO_NS; return (double)secs; }
//	else                    { return (double)t / (double)SEC_TO_NS; }
func pyTimeSeconds(ns int64) float64 {
	const secToNs = 1_000_000_000
	if ns%secToNs == 0 {
		return float64(ns / secToNs)
	}
	return float64(ns) / float64(secToNs)
}

// pyTimeMs reproduces `int(time.time() * 1000)`.
//
// int() truncates the FLOAT, not the exact nanosecond count, so the product can
// round up across a millisecond boundary. It feeds recv_ms on the tape, the auth
// timestamp, and now_ms in the resolver's due-row query — the last of which
// selects which horizons come due, and so which live book supplies a mid.
func pyTimeMs(sec int64, nsec int) int64 {
	return int64(pyTimeSeconds(sec*1_000_000_000+int64(nsec)) * 1000)
}

// pyTime is `time.time()`: wall-clock seconds as CPython's float.
//
// The rig compares deadlines and commit/report intervals against this rather
// than against time.Time, because Go's time.Time carries a monotonic reading and
// Python's float does not. A wall-clock step moves Python's schedule and would
// not move a monotonic one, which changes how many frames are processed before a
// deadline and which rows are inside the transaction when it ends.
func pyTime() float64 {
	t := time.Now()
	return pyTimeSeconds(t.Unix()*1_000_000_000 + int64(t.Nanosecond()))
}

func nowMs() int64 {
	t := time.Now()
	return pyTimeMs(t.Unix(), t.Nanosecond())
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

func main() {
	duration := flag.Float64("duration", 0, "seconds to run (default: until interrupted)")
	dbPath := flag.String("db", "rig.db", "SQLite database to write")
	noRaw := flag.Bool("no-raw", false, "skip raw frame capture (loses replay/reanalysis)")
	universePath := flag.String("universe", "", "JSON {ticker: target_size} instead of the REST fetch")
	flag.Parse()

	if err := run(*duration, *dbPath, *noRaw, *universePath); err != nil {
		logf("%v", err)
		os.Exit(1)
	}
}

func run(duration float64, dbPath string, noRaw bool, universePath string) error {
	// Both signals unwind, but they do NOT end the same way, because Python's
	// two paths do not either:
	//
	//   SIGINT   caught as KeyboardInterrupt, `finally` runs, transaction
	//            COMMITS (rig.py:565-569, 661-667).
	//   SIGTERM  no handler installed; the process dies without reaching either
	//            `finally` and SQLite ROLLS BACK the open transaction.
	//
	// Committing on SIGTERM would keep up to one commit interval of rows that
	// Python discards, on every restart. The gzip trailer is flushed either way:
	// the tape is a replay INPUT rather than a compared output, so completing it
	// cannot change a row, and it removes the repair step every Python tape
	// needs before it can be gated.
	//
	// ONE registration, not two. signal.NotifyContext plus a second
	// signal.Notify channel is a race: the runtime delivers to registered
	// channels in map order, so the context can be cancelled and cleanup can
	// reach a non-blocking receive before SIGTERM lands in the second channel —
	// silently selecting the COMMIT path. Latching the signal identity before
	// cancelling removes the window entirely.
	a := &app{}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Keep consuming. Reading exactly one signal would let a SIGTERM that
		// follows a SIGINT be ignored entirely, and Go would then COMMIT where
		// the second signal should have discarded.
		for s := range sigCh {
			if s == syscall.SIGTERM {
				a.mu.Lock()
				a.sawSigterm = true
				a.mu.Unlock()
			}
			cancel()
		}
	}()

	universe, tickers, err := loadUniverse(ctx, universePath)
	if err != nil {
		return err
	}

	signer, err := feed.NewSigner()
	if err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	fmt.Printf("universe: %d active LIP markets -> %s\n", len(universe), dbPath)

	var raw *tape.Writer
	if !noRaw {
		stamp := time.Now().UTC().Format("20060102T150405Z")
		rawPath := filepath.Join(filepath.Dir(dbPath), "raw-"+stamp+".jsonl.gz")
		if raw, err = tape.NewWriter(rawPath, universe, tickers); err != nil {
			st.Close()
			return err
		}
		fmt.Printf("raw capture -> %s\n", rawPath)
	}

	rig := core.NewRig(st, universe)
	rig.Log = func(s string) { logf("%s", s) }

	now := pyTime()
	a.rig, a.store, a.signer, a.raw, a.tickers = rig, st, signer, raw, tickers
	a.lastCommit, a.lastReport = now, now
	a.resolve = time.NewTimer(resolveInterval)
	a.backoff = time.Second
	// `if duration else None` in Python: any non-zero value is truthy, so a
	// NEGATIVE duration gives an already-past deadline and an immediate exit,
	// where `> 0` would silently mean "run forever". NaN is truthy too and its
	// comparisons are all false, so it never expires; +Inf likewise. Both are
	// reachable from the CLI and neither survives a time.Duration conversion.
	if duration != 0 && !math.IsNaN(duration) {
		if math.IsInf(duration, 1) {
			a.hasDeadline = false
		} else {
			a.deadline = now + duration
			a.hasDeadline = true
		}
	} else if math.IsNaN(duration) {
		a.hasDeadline = false
	}
	defer a.resolve.Stop()

	loopErr := a.loop(ctx)

	// rig.py's `finally`: commit, then close the tape — except under SIGTERM,
	// where Python never reaches the `finally` at all and the transaction is
	// rolled back with it. The latch is set before the context is cancelled, so
	// reading it here cannot race the shutdown that cancellation triggered.
	closeStore := st.Close
	if a.terminating() {
		closeStore = st.Discard
		logf("[sigterm] discarding the open transaction, as Python's unhandled " +
			"SIGTERM would")
	}
	// Cleanup errors are returned, not just logged. Exiting 0 after a failed
	// commit reports success for a run whose rows were never written.
	closeErr := closeStore()
	if closeErr != nil {
		logf("store: %v", closeErr)
	}
	if raw != nil {
		if err := raw.Close(); err != nil {
			logf("tape: %v", err)
			if closeErr == nil {
				closeErr = err
			}
		}
	}
	s := a.rig.Stats
	fmt.Printf("stopped. trades=%d through=%d refs=%d gaps=%d quarantined=%d stale=%d\n",
		s.Trades, s.Through, s.Refs, s.Gaps, s.Quarantined, a.rig.StaleCount())
	if loopErr != nil {
		return loopErr
	}
	return closeErr
}

func loadUniverse(ctx context.Context, path string) (map[string]float64, []string, error) {
	if path == "" {
		return feed.Universe(ctx)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var m map[string]float64
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, nil, err
	}
	// A JSON object has no order to preserve, so this path sorts to stay
	// deterministic. The REST path carries the real response order.
	order := make([]string, 0, len(m))
	for t := range m {
		order = append(order, t)
	}
	sort.Strings(order)
	return m, order, nil
}

// loop is rig.py:490-564: connect, stream, and on any transport failure commit,
// back off, and reset every book before trying again.
func (a *app) loop(ctx context.Context) error {
	for {
		if a.hasDeadline && pyTime() >= a.deadline {
			return nil
		}
		err := a.session(ctx)
		switch {
		case errors.Is(err, errDeadline), errors.Is(err, errTerminated):
			return nil
		case ctx.Err() != nil:
			return nil
		}

		var hs *feed.HandshakeError
		if errors.As(err, &hs) {
			return hs
		}

		// A CLEAN close is not the reconnect path. websockets' __aiter__ catches
		// ConnectionClosedOK and returns, so rig.py:505's `async for` ends
		// normally, the `try` completes, and the except at rig.py:548 never
		// runs: no commit, no backoff, and NO reset. The loop just reconnects,
		// carrying the old books, history, refs, seq and stale set with it.
		//
		// Carrying stale books across a close looks like a defect. Phase A
		// reproduces it. See port-spec.md P25a.
		if err == nil || feed.IsCleanClose(err) {
			logf("[clean close] reconnecting immediately, state retained")
			continue
		}

		if _, cerr := a.commit(); cerr != nil {
			return cerr
		}
		logf("[reconnect in %.0fs] %v", a.backoff.Seconds(), err)
		if serr := a.sleep(ctx, a.backoff); serr != nil {
			return nil
		}
		a.backoff *= 2
		if a.backoff > 60*time.Second {
			a.backoff = 60 * time.Second
		}

		// AFTER the wait, not before — Python resets the books in the same
		// position, so the resolver that keeps running during the backoff still
		// sees the pre-disconnect books. See port-spec.md P25 for why all five
		// pieces of state go together.
		a.rig.ResetOnReconnect()
	}
}

// sleep is `await asyncio.sleep(backoff)`: the forward-mid resolver is a
// separate task in Python and keeps firing throughout the reconnect wait, so it
// keeps firing here too.
func (a *app) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		case <-a.resolve.C:
			a.resolveMids()
		}
	}
}

// resolveMids runs one pass and re-arms the timer from its COMPLETION, which is
// what `while True: await asyncio.sleep(5); ...` does. rig.py:385-407.
func (a *app) resolveMids() {
	if !a.resolverDead {
		_, err := a.store.ResolveMids(nowMs(), 2000, func(ticker string) *float64 {
			book := a.rig.Book(ticker)
			if book == nil {
				return nil
			}
			return book.Mid()
		}, func() error { _, e := a.commit(); return e })
		if err != nil {
			// Python's resolve_mids has no exception handler, so the first
			// error kills the task for the lifetime of the process while the
			// websocket loop carries on. Retrying would resolve horizons Python
			// leaves pending forever.
			logf("[resolve] %v — resolver stopped, as Python's unhandled task "+
				"exception would stop it", err)
			a.resolverDead = true
		}
	}
	a.resolve.Reset(resolveInterval)
}

// session runs one websocket connection to completion.
func (a *app) session(ctx context.Context) (err error) {
	pumpCtx, cancelPump := context.WithCancel(ctx)
	defer cancelPump()

	conn, derr := feed.Dial(pumpCtx, a.signer, nowMs(), a.tickers)
	if derr != nil {
		return derr
	}
	fmt.Printf("connected: %d markets, whole-exchange trade tape\n", len(a.tickers))
	a.backoff = time.Second

	frames := make(chan []byte, 4096)
	pumpErr := make(chan error, 1)
	go func() { pumpErr <- conn.Pump(pumpCtx, frames) }()

	// However we leave, the socket is closed and the pump has stopped before we
	// return. The tape writer has exactly one owner and a second pump must never
	// overlap the first.
	defer func() {
		cancelPump()
		conn.Close()
		perr := <-pumpErr
		if err == nil {
			err = perr
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-a.resolve.C:
			a.resolveMids()

		case data, ok := <-frames:
			if !ok {
				// The pump ended; the deferred receive supplies its error.
				return nil
			}
			// Taped HERE, on the owner, immediately before handling — not in
			// the pump. Python writes the frame it is about to handle
			// (rig.py:511-519), so taping in the reader would let it run
			// thousands of frames ahead and leave the tape holding frames that
			// were never handled. recv_ms means application consumption, as it
			// does in Python. See port-spec.md P26.
			if terr := a.tapeFrame(data); terr != nil {
				// The raw write sits OUTSIDE Python's handler try, so an OSError
				// there skips _handle entirely and drops into the reconnect
				// path. The frame must not be handled.
				return terr
			}
			if herr := a.rig.Handle(data); herr != nil {
				logf("[handle] %v", herr)
			}
			if a.rig.NeedsResnapshot {
				a.rig.NeedsResnapshot = false
				if serr := conn.Resnapshot(ctx, a.rig.Sids(), a.tickers); serr != nil {
					return serr
				}
			}
			// The commit, report and deadline checks live inside the frame
			// branch, exactly as in Python: a silent feed commits nothing.
			// Python reads the clock ONCE here and reuses it for all three.
			now := pyTime()
			// A SIGTERM that landed mid-branch must not get one more commit in
			// before the loop notices: Python dies where it stands and the
			// transaction goes with it.
			if a.terminating() {
				return errTerminated
			}
			if now-a.lastCommit > 2.0 {
				ok, cerr := a.commit()
				if cerr != nil {
					return cerr
				}
				if !ok {
					return errTerminated
				}
				a.lastCommit = now
			}
			if now-a.lastReport > 60.0 {
				s := a.rig.Stats
				// int(now) in Python: truncation, not rounding.
				fmt.Printf("[%d] trades=%d through=%d refs=%d gaps=%d quarantined=%d stale=%d\n",
					int64(now), s.Trades, s.Through, s.Refs, s.Gaps,
					s.Quarantined, a.rig.StaleCount())
				a.lastReport = now
			}
			if a.hasDeadline && now >= a.deadline {
				return errDeadline
			}
		}
	}
}

// tapeFrame captures one frame before it is handled, so a frame that goes on to
// crash the handler is still on the tape. P26.
//
// A write failure is returned rather than logged: Python's raw write is outside
// the handler's try/except, so an OSError there prevents _handle from running
// and enters the reconnect path.
func (a *app) tapeFrame(data []byte) error {
	if a.raw == nil {
		return nil
	}
	return a.raw.Frame(nowMs(), data)
}

// confidence: high

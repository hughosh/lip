package hstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"lip/harness/risk"
)

// TestOrderlyShutdownDrainsBeforeStoppingTheWriter is the ORDER, and the order
// is the whole of the requirement.
//
// `Close` refuses while the FIFO is non-empty, and a writer whose context is
// cancelled fails every record it was still holding -- one terminal `Result`
// each, plus `own.failBinding` for a binding. Both behaviours are correct and
// both are about the CRASH path: a writer that has gone leaves records that
// nothing will ever write, so saying so loudly beats leaving them in limbo.
//
// An ORDERLY shutdown is the opposite situation. Nothing has failed; the writer
// is simply being asked to stop, and everything queued behind it is still
// perfectly writable. Cancelling first converts a clean stop into exactly the
// loss the crash path is a damage-limitation response to -- and it does so at
// the one moment a process is least likely to be watched. So the order is
// `Pending()==0` -> cancel -> `Close`, and the drain is not an optimisation.
//
// The drain's only observable is that it WAITS, so the wait is made into a
// rendezvous rather than a race: `s.sleep` is the writer's injected backoff and
// nothing on this path calls it -- no write fails here -- so one call to it is
// proof that the shutdown found the queue non-empty and chose to wait instead
// of cancelling. Without that handshake the test would unblock the writer on a
// timer and pass against a shutdown that never drained at all.
func TestOrderlyShutdownDrainsBeforeStoppingTheWriter(t *testing.T) {
	dbPath, logPath := paths(t)
	s, c, _ := gatedStoreAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)

	waited := make(chan struct{}, 1)
	slept := s.sleep
	s.sleep = func(ctx context.Context, d time.Duration) bool {
		select {
		case waited <- struct{}{}:
		default:
		}
		return slept(ctx, d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Block the writer INSIDE the first record's write, so the two behind it
	// are provably still queued when the shutdown begins.
	c.block()
	go s.Run(ctx)

	rec := func(id string, ms int64) {
		t.Helper()
		if _, err := s.RecordAnomaly(h, id,
			anomaly("OWNER_STALLED", risk.SEV1, ""), ms); err != nil {
			t.Fatalf("RecordAnomaly %s: %v", id, err)
		}
	}
	rec("shut-1", 1_700_000_010_000)
	<-c.entered
	rec("shut-2", 1_700_000_011_000)
	rec("shut-3", 1_700_000_012_000)
	if p := s.Health().Pending(); p != 3 {
		t.Fatalf("%d record(s) queued, want 3; the property under test needs a "+
			"non-empty FIFO at the moment the shutdown starts", p)
	}

	done := make(chan error, 1)
	go func() { done <- s.Shutdown(ctx, cancel) }()

	select {
	case <-waited:
		// The shutdown is waiting on the queue. This is the property.
	case err := <-done:
		t.Fatalf("Shutdown returned (%v) with three accepted records still "+
			"queued: the orderly path must wait for Pending()==0 BEFORE it "+
			"cancels the writer, because a cancelled writer terminally fails "+
			"every record it was holding", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown never waited for the queue: three records were " +
			"accepted as durable-in-progress and the orderly path did not " +
			"drain them before stopping the writer")
	}

	// Only now does the disk come back.
	c.unblock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown of a store that could drain: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown never returned after the writer was released")
	}

	// Nothing was terminally failed. A terminal Result on this path would be a
	// record that the orderly shutdown itself destroyed.
	for _, r := range s.TakeResults() {
		if !r.OK() {
			t.Fatalf("the orderly shutdown terminally failed a %s record it "+
				"had already accepted: %v", r.Kind, r.Err)
		}
	}

	// Step 2 happened: the writer was cancelled, not merely left running.
	if ctx.Err() == nil {
		t.Fatal("the shutdown closed the store without stopping the writer; " +
			"a writer left running against a closed store is a goroutine " +
			"holding a connection that no longer exists")
	}
	// Step 3 happened: the store is closed and refuses new work.
	if hl := s.Health(); hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("the store reports %+v after a completed shutdown", hl)
	}
	if _, err := s.RecordAnomaly(h, "shut-4",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_013_000); err == nil {
		t.Fatal("a closed store accepted an audit record")
	}

	// And the three records are durable in BOTH journals, read back through a
	// fresh Open of the same two files -- which is the only claim that matters.
	reopened := openAt(t, dbPath, logPath)
	t.Cleanup(func() { reopened.Close() })
	for _, id := range []string{"shut-1", "shut-2", "shut-3"} {
		row, ok, err := reopened.Reader().Anomaly(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if !ok || !row.Journaled {
			t.Fatalf("audit record %s was accepted and did not survive the "+
				"orderly shutdown; the shutdown that stopped the writer is "+
				"the one that lost it", id)
		}
	}
}

// TestShutdownWillNotCancelAWriterItCouldNotDrain is the refusal half.
//
// A shutdown that cannot drain must change NOTHING. Cancelling the writer of a
// store whose disk is unavailable converts records that were still going to be
// retried forever into records that are terminally lost, and closing over them
// is refused anyway -- so a shutdown that cancels first has destroyed evidence
// and still not shut down. It reports why instead, and the store stays exactly
// as usable as it was.
func TestShutdownWillNotCancelAWriterItCouldNotDrain(t *testing.T) {
	s, c, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	// A TRANSIENT failure: retried forever, so the record stays queued and
	// stays writable. It is evidence, not garbage.
	c.setBackErr(errors.New("input/output error"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	if _, err := s.RecordAnomaly(h, "wedge-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	if s.Health().Pending() == 0 {
		t.Fatal("the record did not queue")
	}

	deadline, stop := context.WithTimeout(context.Background(),
		100*time.Millisecond)
	defer stop()
	if err := s.Shutdown(deadline, cancel); err == nil {
		t.Fatal("Shutdown closed a store it could not drain, discarding an " +
			"accepted audit record")
	}

	if ctx.Err() != nil {
		t.Fatal("the shutdown cancelled a writer it could not drain: every " +
			"record that writer was holding is now terminally lost, which is " +
			"the outcome the drain-first ordering exists to prevent")
	}
	if _, err := s.RecordAnomaly(h, "wedge-2",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_011_000); err != nil {
		t.Fatalf("a refused Shutdown left the store unusable: %v", err)
	}

	// And it completes, unchanged, once the disk comes back.
	c.setBackErr(nil)
	if err := s.Shutdown(context.Background(), cancel); err != nil {
		t.Fatalf("Shutdown of a recovered store: %v", err)
	}
	for _, r := range s.TakeResults() {
		if !r.OK() {
			t.Fatalf("a %s record was lost by the recovered shutdown: %v",
				r.Kind, r.Err)
		}
	}
}

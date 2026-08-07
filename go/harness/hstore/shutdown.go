package hstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// drainPollInterval is how often an orderly shutdown re-asks whether the FIFO
// has emptied.
//
// It is a POLL and not a wait on `Wake()`, because `Wake()` is a capacity-one
// signal that the store's real result consumer owns: a shutdown that received
// from it would swallow the wakeup that tells that consumer its own receipts
// have resolved. Asking is cheap -- `Health()` takes the mutex and returns a
// snapshot -- and asking is also what OBSERVES a wedged writer, since a write
// that never returns cannot be reported by the goroutine blocked inside it.
// Two milliseconds and not `HealthPollInterval`: this is a bounded wait a
// process is sitting inside, not a background liveness check.
const drainPollInterval = 2 * time.Millisecond

// Drain blocks until every record this store has accepted has reached a
// terminal outcome -- durable, or terminally failed and reported through
// `TakeResults`.
//
// "Accepted" is the whole point. `submit` returns a receipt and the caller goes
// on believing the record exists; until `Pending()` reaches zero that belief is
// about a row that is still only in memory. Draining is therefore the act of
// making the store's promises true before anything is allowed to stop.
//
// It returns an error rather than waiting forever when the context ends, and it
// does NOT touch the store on the way out: a store that could not drain is
// still a store that will keep retrying, and the caller's correct response is
// to stay up and say so, not to tear down.
//
// The wait goes through `s.sleep`, the same injected, context-aware sleeper the
// writer's retry ladder uses, so a cancelled context ends the wait immediately
// rather than after one more tick.
func (s *Store) Drain(ctx context.Context) error {
	for {
		h := s.Health()
		if h.Pending() == 0 {
			return nil
		}
		if !s.sleep(ctx, drainPollInterval) {
			why := h.LastError()
			if why == "" {
				why = "no write failure has been reported, so the writer is " +
					"either still behind or was never started"
			}
			return fmt.Errorf("the store did not drain: %d record(s) were "+
				"accepted as durable-in-progress and are still unwritten (%s)",
				h.Pending(), why)
		}
	}
}

// Shutdown is the ORDERLY stop, and the ORDER is the whole of it: drain, then
// stop the writer, then close.
//
// It is one call because the three steps are only safe in this sequence, and
// two of the three refuse or destroy evidence when they are taken early.
//
//   - `Close` REFUSES while the FIFO is non-empty. Every queued record has
//     already been accepted, and closing over the top of one turns "this store
//     took my anomaly" into nothing at all.
//   - A writer whose context is cancelled fails everything it was still
//     holding: one terminal `Result` per record, plus `own.failBinding` for a
//     binding. That behaviour is correct and it is about the CRASH path -- a
//     writer that has gone leaves records nothing will ever write, so saying so
//     loudly beats leaving them in limbo, and it is what lets `Close` succeed
//     after a writer exit. It is a damage-limitation response, not a shutdown
//     procedure. Reaching it from an orderly stop means the shutdown itself
//     destroyed records that were still perfectly writable.
//
// So the drain is not an optimisation and not politeness: it is the step that
// makes the difference between stopping and losing. `lip-3af` wires the process
// and calls this; the ordering lives here so that it cannot be re-derived
// slightly differently at the call site.
//
// `stopWriter` is the `context.CancelFunc` of the context passed to `Run`. The
// store does not own it: `Run` takes the caller's context by design, and a
// second private stop channel would be a second way to stop the writer, which
// is one more than there should be.
//
// A shutdown that cannot drain returns the reason and changes NOTHING -- the
// writer is not cancelled and the store is not closed. It stays exactly as
// usable as it was, still retrying, and the caller learns that it cannot stop
// cleanly rather than discovering afterwards that it stopped destructively.
func (s *Store) Shutdown(ctx context.Context, stopWriter context.CancelFunc) error {
	if stopWriter == nil {
		return errors.New("an orderly shutdown needs the writer's cancel " +
			"function: without it the writer outlives the connection this " +
			"closes, and a store cannot be stopped by closing it -- Close " +
			"refuses while the writer still holds a record")
	}
	// 1. Drain. Nothing below this line may run while a record the store has
	//    accepted is still only in memory.
	if err := s.Drain(ctx); err != nil {
		return fmt.Errorf("orderly shutdown refused: %w; the writer has NOT "+
			"been stopped and the store has NOT been closed, because both "+
			"would turn records that are still being retried into records "+
			"that are permanently lost", err)
	}
	// 2. Stop the writer. The queue is empty, so there is nothing for its exit
	//    to fail.
	stopWriter()
	// 3. Close. It can only succeed now, which is the proof that step 1 ran.
	return s.Close()
}

// confidence: high

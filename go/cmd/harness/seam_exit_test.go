package main

import (
	"syscall"
	"testing"
	"time"
)

// A planned flat drain goes through the same helper used by the other composed
// tests. Its exit must be observable without ending the test process, and the
// helper's cleanup must still be able to wait for serve to return.
func TestSeamHarnessPlannedFlatDrainCapturesExit(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	h.start()
	h.awaitActionable()

	select {
	case code := <-h.serveExits:
		t.Fatalf("serve exited %d before the operator signal", code)
	default:
	}
	h.serveSignals <- syscall.SIGTERM

	select {
	case code := <-h.serveExits:
		if code != 0 {
			t.Fatalf("planned flat drain exited with code %d, want 0", code)
		}
	case <-time.After(seamBudget):
		t.Fatal("planned flat drain did not reach the injected exit callback")
	}
	if got := h.latchTrigger(); got != "sigterm" {
		t.Fatalf("durable stop trigger = %q, want sigterm", got)
	}
	// The callback cancels serve as well as capturing its exit. An explicit stop
	// here checks the helper's cleanup path; the registered cleanup is idempotent.
	h.stopServe()
}

package lifecycle

import (
	"sync"
	"testing"
	"time"
)

// The signal handler can begin or upgrade a drain while the main loop observes
// it. Concurrent observations must also consume each escalation deadline once.
func TestDrainTrackerConcurrentSignalAndObservation(t *testing.T) {
	p := testParams()
	d, err := NewDrainTracker(p)
	if err != nil {
		t.Fatalf("NewDrainTracker: %v", err)
	}
	permit, _ := grantedPermit(t, &recordingLatch{})
	open := DrainObservation{TruthKnown: true, AnyInventory: true}

	runTogether := func(work []func()) {
		t.Helper()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, fn := range work {
			wg.Add(1)
			go func(fn func()) {
				defer wg.Done()
				<-start
				fn()
			}(fn)
		}
		close(start)
		wg.Wait()
	}

	// Exercise the first write against both forms of read, before there is
	// any exit authority.
	var startup []func()
	for range 32 {
		startup = append(startup,
			func() { d.BeginUnplanned(0) },
			func() { _ = d.Started() },
			func() { _ = d.Observe(open, time.Minute) },
		)
	}
	runTogether(startup)
	if !d.Started() || d.Observe(DrainObservation{TruthKnown: true}, time.Minute).ExitAuthorised {
		t.Fatal("unplanned drain was absent or authorised an exit")
	}

	// A later signal grants exit authority but must retain the first drain's
	// monotonic escalation deadline, even alongside ticks and repeat begins.
	var upgrade []func()
	errs := make(chan error, 32)
	for range 32 {
		upgrade = append(upgrade,
			func() { errs <- d.BeginPlanned(permit, p.DrainTimeout/2) },
			func() { d.BeginUnplanned(p.DrainTimeout / 2) },
			func() { _ = d.Observe(open, p.DrainTimeout/2) },
		)
	}
	runTogether(upgrade)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("BeginPlanned: %v", err)
		}
	}
	if d.Observe(open, p.DrainTimeout-time.Nanosecond).ExitAuthorised {
		t.Fatal("inventory open authorised an exit")
	}

	results := make(chan DrainEffects, 32)
	var ticks []func()
	for range 32 {
		ticks = append(ticks, func() { results <- d.Observe(open, p.DrainTimeout) })
	}
	runTogether(ticks)
	close(results)
	pings := 0
	for eff := range results {
		if eff.ExitAuthorised {
			t.Fatal("inventory open authorised an exit at the deadline")
		}
		pings += len(eff.Anomalies)
	}
	if pings != 1 {
		t.Fatalf("concurrent ticks emitted %d first deadline pings, want 1", pings)
	}
	if !d.Observe(DrainObservation{TruthKnown: true}, p.DrainTimeout).ExitAuthorised {
		t.Fatal("valid signal permit did not authorise exit once flat and unrested")
	}
}

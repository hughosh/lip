package ping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lip/harness/quote"
	"lip/harness/risk"
)

const hour = int64(60 * 60 * 1000)

// TestSEV1IsUrgentImmediateAndFiveMinuteDeduplicated is §13.2's top tier.
//
// SEV1 is "risk state is wrong or unmanaged; act now", so it does NOT wait for
// the fifteen-minute bucket -- being told at the top of the hour that the
// position model is unreliable is being told too late. It is `urgent`, which is
// what makes a phone ring through a silent profile. And it deduplicates per
// (class,ticker) for five minutes, because a condition that re-fires every
// second must not flatten the battery of the person it is trying to wake.
//
// `M-P-SEV1` routes SEV1 through the fifteen-minute bucket.
func TestSEV1IsUrgentImmediateAndFiveMinuteDeduplicated(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()

	svc := f.service(senderTo(t, srv, "topic-sev1"), deadmanTo(t, dsrv),
		time.Duration(hour)*time.Millisecond)
	ctx := context.Background()
	t0 := int64(1_700_000_000_000)

	f.anomaly("a1", "FOREIGN_FILL", risk.SEV1, "KXTEST-A", t0)
	eff := svc.Step(ctx, t0, heartbeatFor(quote.Running))
	sent := pushesOf(eff, PushAnomaly)
	if len(sent) != 1 {
		t.Fatalf("a SEV1 produced %d pushes at the moment it was recorded, "+
			"want exactly one; it does not wait for a bucket", len(sent))
	}
	if sent[0].Err != nil {
		t.Fatalf("push failed: %v", sent[0].Err)
	}
	if sent[0].Priority != PriorityUrgent {
		t.Fatalf("SEV1 priority %q, want %q -- default priority does not "+
			"break through a silent profile at 3am", sent[0].Priority,
			PriorityUrgent)
	}

	// A DIFFERENT class is not deduplicated against the first.
	f.anomaly("a2", "TAKER_FILL", risk.SEV1, "KXTEST-A", t0+1_000)
	eff = svc.Step(ctx, t0+1_000, heartbeatFor(quote.Running))
	if got := len(pushesOf(eff, PushAnomaly)); got != 1 {
		t.Fatalf("a SEV1 of a different class produced %d pushes, want one; "+
			"deduplication is per (class,ticker)", got)
	}

	// The SAME (class,ticker) inside five minutes is suppressed. The row stays.
	f.anomaly("a3", "FOREIGN_FILL", risk.SEV1, "KXTEST-A", t0+60_000)
	eff = svc.Step(ctx, t0+60_000, heartbeatFor(quote.Running))
	if got := len(pushesOf(eff, PushAnomaly)); got != 0 {
		t.Fatalf("a repeated SEV1 inside the five-minute window produced %d "+
			"pushes", got)
	}
	f.settle()
	row, ok, err := f.store.Reader().Anomaly("a3")
	if err != nil || !ok {
		t.Fatalf("suppressed row: ok=%v err=%v", ok, err)
	}
	if row.Delivered {
		t.Fatal("a suppressed occurrence was marked delivered without being " +
			"sent; §13.2 suppresses the push, never the record")
	}

	// Past five minutes and still inside fifteen: it must go out.
	eff = svc.Step(ctx, t0+6*60_000, heartbeatFor(quote.Running))
	sent = pushesOf(eff, PushAnomaly)
	if len(sent) != 1 {
		t.Fatalf("a SEV1 six minutes after the last one produced %d pushes, "+
			"want one. Five minutes is SEV1's window; routing it through the "+
			"fifteen-minute SEV2 bucket means the operator hears about an "+
			"unmanaged risk state up to a quarter of an hour late", len(sent))
	}
	if sent[0].Priority != PriorityUrgent {
		t.Fatalf("recovered SEV1 priority %q", sent[0].Priority)
	}
	f.settle()
	if row, _, _ := f.store.Reader().Anomaly("a3"); !row.Delivered {
		t.Fatal("the suppressed occurrence was not delivered by the push that " +
			"represented it")
	}
}

// TestSEV2IsLimitedPerClassMarketAndReportsSuppression is §13.2's middle tier.
//
// One push per (class,ticker) per fifteen minutes. The occurrences in between
// remain ROWS -- the rate limit suppresses the notification, not the evidence --
// and when the bucket next opens, ONE aggregate push carries their count and
// marks every row it represented delivered. An operator who reads "3 further
// occurrences" knows something different from one who reads three identical
// alerts or one alert.
//
// `M-P-SEV2` bypasses the suppression.
func TestSEV2IsLimitedPerClassMarketAndReportsSuppression(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()

	svc := f.service(senderTo(t, srv, "topic-sev2"), deadmanTo(t, dsrv),
		time.Duration(hour)*time.Millisecond)
	ctx := context.Background()
	t0 := int64(1_700_000_000_000)

	f.anomaly("b1", "POSITION_DRIFT", risk.SEV2, "KXTEST-A", t0)
	eff := svc.Step(ctx, t0, heartbeatFor(quote.Running))
	if got := len(pushesOf(eff, PushAnomaly)); got != 1 {
		t.Fatalf("the first SEV2 produced %d pushes, want one", got)
	}

	// Two more of the same class and market, inside the bucket.
	f.anomaly("b2", "POSITION_DRIFT", risk.SEV2, "KXTEST-A", t0+60_000)
	f.anomaly("b3", "POSITION_DRIFT", risk.SEV2, "KXTEST-A", t0+120_000)
	eff = svc.Step(ctx, t0+120_000, heartbeatFor(quote.Running))
	if got := len(pushesOf(eff, PushAnomaly)); got != 0 {
		t.Fatalf("SEV2 occurrences inside the fifteen-minute bucket produced "+
			"%d pushes; §13.2 allows one per (class,ticker) per bucket", got)
	}

	// A different MARKET is a different bucket.
	f.anomaly("b4", "POSITION_DRIFT", risk.SEV2, "KXTEST-B", t0+130_000)
	eff = svc.Step(ctx, t0+130_000, heartbeatFor(quote.Running))
	if got := len(pushesOf(eff, PushAnomaly)); got != 1 {
		t.Fatalf("the same class on a different market produced %d pushes, "+
			"want one; the limit is per (class,ticker)", got)
	}

	// The bucket opens. ONE aggregate carries both suppressed occurrences.
	eff = svc.Step(ctx, t0+16*60_000, heartbeatFor(quote.Running))
	sent := pushesOf(eff, PushAnomaly)
	if len(sent) != 1 {
		t.Fatalf("reopening the bucket produced %d pushes, want one aggregate",
			len(sent))
	}
	if sent[0].Suppressed != 1 {
		t.Fatalf("the aggregate reports %d suppressed occurrences, want 1",
			sent[0].Suppressed)
	}
	if !strings.Contains(sent[0].Body, "1 further occurrence") {
		t.Fatalf("the aggregate body does not report the suppressed count:\n%s",
			sent[0].Body)
	}
	if sent[0].Priority != PriorityDefault {
		t.Fatalf("SEV2 priority %q, want %q", sent[0].Priority, PriorityDefault)
	}

	// Every represented row is delivered, and the representative carries the
	// count so the table alone reconstructs what the operator was told.
	f.settle()
	for _, id := range []string{"b1", "b2", "b3", "b4"} {
		row, ok, err := f.store.Reader().Anomaly(id)
		if err != nil || !ok {
			t.Fatalf("row %s: ok=%v err=%v", id, ok, err)
		}
		if !row.Delivered {
			t.Fatalf("row %s was represented by a push and not marked "+
				"delivered", id)
		}
	}
	if row, _, _ := f.store.Reader().Anomaly("b2"); row.SuppressedCount != 1 {
		t.Fatalf("the representative row records %d suppressed occurrences, "+
			"want 1", row.SuppressedCount)
	}
}

// TestUndeliveredAlertsSurviveRestartAndDrainOldestFirst is why the pending
// queue is a table and not a slice.
//
// An alert that was undelivered when the process died is an alert the operator
// still has not seen. The drain therefore covers EVERY run's rows, in `first_ms`
// order, carrying the ORIGINAL timestamps -- an alert about something four hours
// old must not read as having just happened -- and due SEV1 goes first.
//
// `M-P-RESTART` scopes the load to the current run, which makes every restart
// silently discard the backlog that the restart itself is evidence for.
func TestUndeliveredAlertsSurviveRestartAndDrainOldestFirst(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()

	ctx := context.Background()
	t0 := int64(1_700_000_000_000)

	// The channel is down. Three alerts are journalled and none is delivered.
	rec.setStatus(http.StatusInternalServerError)
	svc := f.service(senderTo(t, srv, "topic-restart"), deadmanTo(t, dsrv),
		time.Duration(hour)*time.Millisecond)

	f.anomaly("c1", "POSITION_DRIFT", risk.SEV2, "KXTEST-A", t0)
	f.anomaly("c2", "FOREIGN_FILL", risk.SEV1, "KXTEST-B", t0+1_000)
	f.anomaly("c3", "ORDER_WALK_INCOMPLETE", risk.SEV2, "KXTEST-C", t0+2_000)

	eff := svc.Step(ctx, t0+3_000, heartbeatFor(quote.Running))
	for _, p := range pushesOf(eff, PushAnomaly) {
		if p.Err == nil {
			t.Fatal("a 500 from the notification service was treated as a " +
				"delivery")
		}
	}
	f.settle()
	for _, id := range []string{"c1", "c2", "c3"} {
		row, _, _ := f.store.Reader().Anomaly(id)
		if row.Delivered {
			t.Fatalf("row %s was marked delivered by a failed push", id)
		}
		if row.Attempts == 0 {
			t.Fatalf("row %s did not record the failed attempt", id)
		}
	}

	// The process dies and a NEW run begins against the same files.
	f.restart("runb", t0+10*hour)
	rec.setStatus(http.StatusOK)
	rec.reset()
	svc2 := f.service(senderTo(t, srv, "topic-restart"), deadmanTo(t, dsrv),
		time.Duration(hour)*time.Millisecond)

	eff = svc2.Step(ctx, t0+10*hour, heartbeatFor(quote.Running))
	sent := pushesOf(eff, PushAnomaly)
	if len(sent) != 3 {
		t.Fatalf("the drain after a restart sent %d of 3 undelivered alerts. "+
			"An alert nobody has seen is not made irrelevant by the process "+
			"that failed to send it dying, and scoping the queue to the "+
			"current run discards exactly the backlog the restart is evidence "+
			"for", len(sent))
	}
	// Due SEV1 first, then the rest oldest-first.
	order := []string{sent[0].AnomalyIDs[0], sent[1].AnomalyIDs[0],
		sent[2].AnomalyIDs[0]}
	want := []string{"c2", "c1", "c3"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("drain order %v, want %v (due SEV1 first, then oldest "+
				"first)", order, want)
		}
	}
	if sent[0].Priority != PriorityUrgent {
		t.Fatalf("the recovered SEV1 was not urgent: %q", sent[0].Priority)
	}

	// The original timestamps travel with the alert.
	if !strings.Contains(sent[1].Body, stamp(t0)) {
		t.Fatalf("a recovered alert does not carry its original timestamp "+
			"%s:\n%s", stamp(t0), sent[1].Body)
	}

	f.settle()
	for _, id := range []string{"c1", "c2", "c3"} {
		row, _, _ := f.store.Reader().Anomaly(id)
		if !row.Delivered {
			t.Fatalf("row %s was pushed after the restart and not marked "+
				"delivered", id)
		}
	}
}

// TestHeartbeatContinuesInEveryStateAndNamesStoreFailure is §13.3 and H-PING-1.
//
// The heartbeat is the dead man's switch, and F18 -- "the harness stopped and
// nobody noticed" -- is detected by its ABSENCE. A heartbeat that stops while
// things are wrong therefore goes quiet at exactly the moment its silence would
// be read as the process having died, and it takes the external check-in with
// it. So it fires in every global state, it fires while persistence is broken,
// and when persistence is broken it SAYS so.
//
// `M-P-HEALTHBEAT` suppresses it unless the store is healthy and the state is
// RUNNING.
func TestHeartbeatContinuesInEveryStateAndNamesStoreFailure(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	deadRec := newRecorder()
	dsrv := httptest.NewTLSServer(deadRec)
	defer dsrv.Close()

	const interval = int64(60_000)
	svc := f.service(senderTo(t, srv, "topic-hb"), deadmanTo(t, dsrv),
		time.Duration(interval)*time.Millisecond)
	ctx := context.Background()
	now := int64(1_700_000_000_000)

	states := []quote.GlobalState{quote.Starting, quote.UnknownRisk,
		quote.Running, quote.WindingDown, quote.Drained}
	for i, gs := range states {
		at := now + int64(i)*interval
		eff := svc.Step(ctx, at, heartbeatFor(gs))
		beats := pushesOf(eff, PushHeartbeat)
		if len(beats) != 1 {
			t.Fatalf("global state %v produced %d heartbeats, want one. The "+
				"heartbeat IS the dead man's switch: suppressing it in a "+
				"non-RUNNING state makes a stopped harness and a winding-down "+
				"one indistinguishable from outside", gs, len(beats))
		}
		if beats[0].Err != nil {
			t.Fatalf("heartbeat in %v failed: %v", gs, beats[0].Err)
		}
		if !strings.Contains(beats[0].Body, "global: "+gs.String()) {
			t.Fatalf("the %v heartbeat does not name its state:\n%s", gs,
				beats[0].Body)
		}
		if !eff.Heartbeat || !eff.Deadman {
			t.Fatalf("state %v: heartbeat=%v deadman=%v; every scheduled "+
				"heartbeat checks in with the external watchdog", gs,
				eff.Heartbeat, eff.Deadman)
		}
	}
	if deadRec.count() != len(states) {
		t.Fatalf("the dead man was checked in %d times for %d heartbeats",
			deadRec.count(), len(states))
	}

	// Persistence breaks. SQLITE_BUSY is transient, so the store goes unhealthy
	// and keeps retrying -- H-STORE-3's exact condition.
	release := f.lockDatabase()
	defer release()
	f.anomalyAsync("d1", "OWNER_STALLED", risk.SEV1, "", now)
	f.awaitUnhealthy()

	at := now + int64(len(states))*interval
	eff := svc.Step(ctx, at, heartbeatFor(quote.WindingDown))
	beats := pushesOf(eff, PushHeartbeat)
	if len(beats) != 1 {
		t.Fatalf("a broken store produced %d heartbeats, want one; a store "+
			"failure revokes ADDING and nothing else (H-STORE-3), and the "+
			"heartbeat is how the operator learns it happened", len(beats))
	}
	body := beats[0].Body
	if !strings.Contains(body, "store: UNHEALTHY") ||
		!strings.Contains(body, "persistence is unavailable") {
		t.Fatalf("the heartbeat does not name the storage failure:\n%s", body)
	}
	if !strings.Contains(body, "ADDING authority is revoked") {
		t.Fatalf("the heartbeat does not say what the failure cost:\n%s", body)
	}
	if !eff.Deadman {
		t.Fatal("the external check-in was skipped because the store was " +
			"broken; the watchdog would alarm on a harness that is alive and " +
			"correctly refusing to add risk")
	}

	// And the transition itself is an immediate urgent notice.
	health := pushesOf(eff, PushHealth)
	if len(health) != 1 {
		t.Fatalf("the healthy->unhealthy transition produced %d urgent "+
			"notices, want one", len(health))
	}
	if health[0].Priority != PriorityUrgent {
		t.Fatalf("the health notice priority is %q", health[0].Priority)
	}
	if len(health[0].AnomalyIDs) != 0 {
		t.Fatal("the health notice delivered anomaly rows; it is status " +
			"telemetry, and the anomaly whose journalling failed is precisely " +
			"the record that is not yet safe to send")
	}
}

package qual

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/rest"
)

var testStart = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func testMetadata() Metadata {
	return Metadata{
		SchemaVersion:  SchemaVersion,
		ConfigHash:     "sha256:test-config",
		BinaryIdentity: "git:a72a7ce+sha256:test-binary",
		Ticker:         "TEST-26AUG11",
		Rung:           "read-only-qualification",
		Live:           false,
	}
}

func openTestRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qualification.json")
	r, err := Open(path, testMetadata(), SegmentStart{
		ID: "process-1", PID: 101, StartedAt: testStart,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r, path
}

type countingDoer struct{ calls atomic.Uint64 }

func (d *countingDoer) Do(context.Context, rest.Request) (rest.Response, error) {
	d.calls.Add(1)
	return rest.Response{Status: 200, Body: []byte(`{}`)}, nil
}

type inspectingDoer struct {
	calls atomic.Uint64
	do    func(rest.Request) error
}

func (d *inspectingDoer) Do(_ context.Context, req rest.Request) (rest.Response, error) {
	d.calls.Add(1)
	if d.do != nil {
		if err := d.do(req); err != nil {
			return rest.Response{}, err
		}
	}
	return rest.Response{Status: 200, Body: []byte(`{}`)}, nil
}

func TestHTTPCountingAndBelowGuardComposition(t *testing.T) {
	r, _ := openTestRecorder(t)
	next := &countingDoer{}
	counted, err := r.WrapDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := rest.NewWriteGuard(counted, rest.WriteArm{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := guard.Do(context.Background(), rest.Request{
		Method: "GET", Path: "/markets/TEST-26AUG11?ignored=yes",
	}); err != nil {
		t.Fatalf("guarded GET: %v", err)
	}
	if _, err := guard.Do(context.Background(), rest.Request{
		Method: "POST", Path: "/portfolio/events/orders",
	}); err == nil {
		t.Fatal("read-only guard allowed POST")
	}
	if _, err := counted.Do(context.Background(), rest.Request{
		Method: "delete", Path: "/portfolio/events/orders/exchange-order-1",
	}); err != nil {
		t.Fatalf("direct counted DELETE: %v", err)
	}

	got := r.Snapshot().HTTP
	want := []HTTPCount{
		{HTTPKey: HTTPKey{Method: "GET", Endpoint: "/markets/{ticker}"}, Count: 1},
		{HTTPKey: HTTPKey{Method: "delete", Endpoint: "/portfolio/events/orders/{order_id}"}, Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP counts = %#v, want %#v", got, want)
	}
	if got := next.calls.Load(); got != 2 {
		t.Fatalf("wrapped transport calls = %d, want 2 (refused POST must not reach it)", got)
	}
}

func TestAttemptHTTPCountingIsDistinctFromBelowGuard(t *testing.T) {
	r, path := openTestRecorder(t)
	next := &countingDoer{}
	belowGuard, err := r.WrapDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := rest.NewWriteGuard(belowGuard, rest.WriteArm{})
	if err != nil {
		t.Fatal(err)
	}
	attempted, err := r.WrapAttemptDoer(guard)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := attempted.Do(context.Background(), rest.Request{
		Method: "GET", Path: "/portfolio/balance",
	}); err != nil {
		t.Fatalf("attempted GET: %v", err)
	}
	for _, method := range []string{"POST", "post"} {
		if _, err := attempted.Do(context.Background(), rest.Request{
			Method: method, Path: "/portfolio/events/orders",
		}); err == nil {
			t.Fatalf("read-only guard allowed attempted %s", method)
		}
	}

	wantAttempted := []HTTPCount{
		{HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/balance"}, Count: 1},
		{HTTPKey: HTTPKey{Method: "POST", Endpoint: "/portfolio/events/orders"}, Count: 1},
		{HTTPKey: HTTPKey{Method: "post", Endpoint: "/portfolio/events/orders"}, Count: 1},
	}
	snapshot := r.Snapshot()
	if !reflect.DeepEqual(snapshot.AttemptedHTTP, wantAttempted) {
		t.Fatalf("attempted HTTP counts = %#v, want %#v", snapshot.AttemptedHTTP, wantAttempted)
	}
	wantBelow := []HTTPCount{{
		HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/balance"}, Count: 1,
	}}
	if !reflect.DeepEqual(snapshot.HTTP, wantBelow) {
		t.Fatalf("below-guard HTTP counts = %#v, want %#v", snapshot.HTTP, wantBelow)
	}
	if got := next.calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want only the GET", got)
	}
	onDisk, err := readEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(onDisk.AttemptedHTTP, wantAttempted) {
		t.Fatalf("durable attempted HTTP counts = %#v, want %#v",
			onDisk.AttemptedHTTP, wantAttempted)
	}
}

func TestAttemptNonGETIsDurableBeforeForwarding(t *testing.T) {
	r, path := openTestRecorder(t)
	next := &inspectingDoer{do: func(req rest.Request) error {
		onDisk, err := readEvidence(path)
		if err != nil {
			return err
		}
		want := HTTPKey{Method: req.Method, Endpoint: NormalizeEndpoint(req.Path)}
		for _, count := range onDisk.AttemptedHTTP {
			if count.HTTPKey == want && count.Count == 1 {
				return nil
			}
		}
		return errors.New("attempted non-GET observation was not durable when next ran")
	}}
	wrapped, err := r.WrapAttemptDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Do(context.Background(), rest.Request{
		Method: "pOsT", Path: "/portfolio/events/orders",
	}); err != nil {
		t.Fatalf("wrapped attempted pOsT: %v", err)
	}
	if got := next.calls.Load(); got != 1 {
		t.Fatalf("next calls = %d, want 1", got)
	}
}

func TestAttemptNonGETCheckpointFailurePreventsForwarding(t *testing.T) {
	r, path := openTestRecorder(t)
	next := &countingDoer{}
	wrapped, err := r.WrapAttemptDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	r.ops.rename = func(string, string) error { return errors.New("injected rename failure") }
	_, err = wrapped.Do(context.Background(), rest.Request{
		Method: "POST", Path: "/portfolio/events/orders",
	})
	if err == nil {
		t.Fatal("attempted POST forwarded despite checkpoint failure")
	}
	if got := next.calls.Load(); got != 0 {
		t.Fatalf("next calls = %d, want 0 after checkpoint failure", got)
	}
	onDisk, readErr := readEvidence(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(onDisk.AttemptedHTTP) != 0 {
		t.Fatalf("failed replacement changed durable attempted evidence: %#v",
			onDisk.AttemptedHTTP)
	}
}

func TestWouldWriteDedupeAndMaterialChange(t *testing.T) {
	r, _ := openTestRecorder(t)
	base := WouldWriteFingerprint{
		Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
		Price: "45", Quantity: "1", State: "ADDING",
	}
	for i := 0; i < 2; i++ {
		if err := r.RecordWouldWrite(base, testStart.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	changed := base
	changed.Price = "46"
	if err := r.RecordWouldWrite(changed, testStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordWouldWrite(base, testStart.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	got := r.Snapshot().WouldWrites
	if len(got) != 2 {
		t.Fatalf("details = %d, want 2 for segment-local A -> A -> B -> A: %#v", len(got), got)
	}
	if got[0].Fingerprint.Price != "45" || got[0].Observations != 3 ||
		got[1].Fingerprint.Price != "46" || got[1].Observations != 1 {
		t.Fatalf("details = %#v, want 45x3 and 46x1", got)
	}
	if err := r.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(r.path, testMetadata(), SegmentStart{
		ID: "process-2", PID: 202, StartedAt: testStart.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.RecordWouldWrite(base, testStart.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got = resumed.Snapshot().WouldWrites
	if len(got) != 3 || got[0].Observations != 3 || got[2].Observations != 1 ||
		got[0].SegmentID != "process-1" || got[2].SegmentID != "process-2" {
		t.Fatalf("resume did not start a new same-fingerprint episode: %#v", got)
	}
}

func TestBelowGuardNonGETIsDurableBeforeForwarding(t *testing.T) {
	r, path := openTestRecorder(t)
	next := &inspectingDoer{do: func(req rest.Request) error {
		onDisk, err := readEvidence(path)
		if err != nil {
			return err
		}
		want := HTTPKey{Method: req.Method, Endpoint: NormalizeEndpoint(req.Path)}
		for _, count := range onDisk.HTTP {
			if count.HTTPKey == want && count.Count == 1 {
				return nil
			}
		}
		return errors.New("non-GET observation was not durable when transport ran")
	}}
	wrapped, err := r.WrapDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Do(context.Background(), rest.Request{
		Method: "DELETE", Path: "/portfolio/events/orders/exchange-order-1",
	}); err != nil {
		t.Fatalf("wrapped DELETE: %v", err)
	}
	if got := next.calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

func TestBelowGuardNonGETCheckpointFailurePreventsForwarding(t *testing.T) {
	r, path := openTestRecorder(t)
	next := &countingDoer{}
	wrapped, err := r.WrapDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	r.ops.rename = func(string, string) error { return errors.New("injected rename failure") }
	_, err = wrapped.Do(context.Background(), rest.Request{
		Method: "POST", Path: "/portfolio/events/orders",
	})
	if err == nil {
		t.Fatal("wrapped POST forwarded despite checkpoint failure")
	}
	if got := next.calls.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0 after checkpoint failure", got)
	}
	onDisk, readErr := readEvidence(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(onDisk.HTTP) != 0 {
		t.Fatalf("failed replacement changed durable evidence: %#v", onDisk.HTTP)
	}
}

func TestLinkCurrentSegmentRunPersistsAndIsIdempotent(t *testing.T) {
	r, path := openTestRecorder(t)
	if err := r.LinkCurrentSegmentRun("  run-committed-1  "); err != nil {
		t.Fatalf("LinkCurrentSegmentRun: %v", err)
	}
	if err := r.LinkCurrentSegmentRun("run-committed-1"); err != nil {
		t.Fatalf("idempotent LinkCurrentSegmentRun: %v", err)
	}
	if got := r.Snapshot().Segments[0].RunID; got != "run-committed-1" {
		t.Fatalf("in-memory run id = %q, want %q", got, "run-committed-1")
	}
	onDisk, err := readEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk.Segments[0].RunID; got != "run-committed-1" {
		t.Fatalf("durable run id = %q, want %q", got, "run-committed-1")
	}
}

func TestLinkCurrentSegmentRunRefusesEmptyAndDifferentSecondLink(t *testing.T) {
	r, _ := openTestRecorder(t)
	if err := r.LinkCurrentSegmentRun("  "); err == nil {
		t.Fatal("LinkCurrentSegmentRun accepted an empty run id")
	}
	if got := r.Snapshot().Segments[0].RunID; got != "" {
		t.Fatalf("empty link changed run id to %q", got)
	}
	if err := r.LinkCurrentSegmentRun("run-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.LinkCurrentSegmentRun("run-2"); err == nil {
		t.Fatal("LinkCurrentSegmentRun replaced an existing run id")
	}
	if got := r.Snapshot().Segments[0].RunID; got != "run-1" {
		t.Fatalf("second link changed run id to %q", got)
	}
}

func TestLinkCurrentSegmentRunCheckpointFailureRollsBack(t *testing.T) {
	r, path := openTestRecorder(t)
	realRename := r.ops.rename
	r.ops.rename = func(string, string) error { return errors.New("injected rename failure") }
	if err := r.LinkCurrentSegmentRun("run-1"); err == nil {
		t.Fatal("LinkCurrentSegmentRun succeeded despite checkpoint failure")
	}
	if got := r.Snapshot().Segments[0].RunID; got != "" {
		t.Fatalf("failed link left in-memory run id %q", got)
	}
	onDisk, err := readEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk.Segments[0].RunID; got != "" {
		t.Fatalf("failed link changed durable run id to %q", got)
	}
	r.ops.rename = realRename
	if err := r.LinkCurrentSegmentRun("run-1"); err != nil {
		t.Fatalf("retry LinkCurrentSegmentRun: %v", err)
	}
}

func TestWouldWriteDetailsAreHardBoundedWithExactOverflowTotals(t *testing.T) {
	r, _ := openTestRecorder(t)
	for i := 0; i < MaxWouldWriteDetails+10; i++ {
		fingerprint := WouldWriteFingerprint{
			Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
			Price: "45", Quantity: "1", State: string(rune('A' + i)),
		}
		if err := r.RecordWouldWrite(fingerprint, testStart.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// Repeat the current overflow key once (same episode), then return to a
	// retained detail. The latter aggregates and cannot grow either collection.
	lastOverflow := WouldWriteFingerprint{
		Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
		Price: "45", Quantity: "1", State: string(rune('A' + MaxWouldWriteDetails + 9)),
	}
	if err := r.RecordWouldWrite(lastOverflow, testStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	retained := WouldWriteFingerprint{
		Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
		Price: "45", Quantity: "1", State: "A",
	}
	if err := r.RecordWouldWrite(retained, testStart.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if len(s.WouldWrites) != MaxWouldWriteDetails ||
		s.WouldWrites[0].Observations != 2 ||
		s.WouldWriteOverflow.Observations != 11 ||
		s.WouldWriteOverflow.Episodes != 10 {
		t.Fatalf("bounded would-write evidence = details:%d first:%d overflow:%+v",
			len(s.WouldWrites), s.WouldWrites[0].Observations, s.WouldWriteOverflow)
	}
}

func TestCheckpointPersistsMonotonicActiveDurationAcrossResume(t *testing.T) {
	r, path := openTestRecorder(t)
	if err := r.LinkCurrentSegmentRun("run-1"); err != nil {
		t.Fatal(err)
	}
	origin := time.Now()
	r.activeOrigin = origin
	r.activeNow = func() time.Time { return origin.Add(90 * time.Minute) }
	if err := r.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	persisted, err := readEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(persisted.Segments[0].ActiveNanos); got != 90*time.Minute {
		t.Fatalf("persisted active duration = %s, want 90m", got)
	}

	resumed, err := Open(path, testMetadata(), SegmentStart{
		ID: "process-2", PID: 202, StartedAt: testStart.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.LinkCurrentSegmentRun("run-2"); err != nil {
		t.Fatal(err)
	}
	secondOrigin := time.Now()
	resumed.activeOrigin = secondOrigin
	resumed.activeNow = func() time.Time { return secondOrigin.Add(30 * time.Minute) }
	if err := resumed.Finalize(testStart.Add(3 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	final, err := readEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(final.Segments[0].ActiveNanos); got != 90*time.Minute {
		t.Fatalf("resumed first active duration = %s, want 90m", got)
	}
	if got := time.Duration(final.Segments[1].ActiveNanos); got != 30*time.Minute {
		t.Fatalf("resumed second active duration = %s, want 30m", got)
	}
}

func TestWallClockRollbackCannotEraseMonotonicProgress(t *testing.T) {
	r, path := openTestRecorder(t)
	if err := r.LinkCurrentSegmentRun("run-1"); err != nil {
		t.Fatal(err)
	}
	origin := time.Now()
	r.activeOrigin = origin
	r.activeNow = func() time.Time { return origin.Add(4 * time.Hour) }
	wallBeforeStart := testStart.Add(-time.Hour)
	if err := r.Finalize(wallBeforeStart); err != nil {
		t.Fatalf("Finalize after wall rollback: %v", err)
	}
	evidence, err := readEvidence(path)
	if err != nil {
		t.Fatalf("read wall-rollback evidence: %v", err)
	}
	if got := time.Duration(evidence.Segments[0].ActiveNanos); got != 4*time.Hour {
		t.Fatalf("active duration after wall rollback = %s, want 4h", got)
	}
	if evidence.Segments[0].EndedAt == nil ||
		!evidence.Segments[0].EndedAt.Equal(wallBeforeStart) {
		t.Fatalf("diagnostic wall end = %v, want %s",
			evidence.Segments[0].EndedAt, wallBeforeStart)
	}
}

func TestLinkCurrentSegmentRunResetsPreAuthorityActiveOrigin(t *testing.T) {
	r, _ := openTestRecorder(t)
	origin := time.Now()
	elapsed := 30 * time.Minute
	r.activeOrigin = origin
	r.activeNow = func() time.Time { return origin.Add(elapsed) }
	if err := r.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(r.Snapshot().Segments[0].ActiveNanos); got != 0 {
		t.Fatalf("unlinked construction active duration = %s, want zero", got)
	}
	if err := r.LinkCurrentSegmentRun("run-1"); err != nil {
		t.Fatal(err)
	}
	elapsed += time.Minute
	if err := r.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(r.Snapshot().Segments[0].ActiveNanos); got != time.Minute {
		t.Fatalf("linked active duration = %s, want 1m after origin reset", got)
	}
}

func TestCadenceSlotsDeduplicateBurstsWithinOneSlot(t *testing.T) {
	r, _ := openTestRecorder(t)
	origin := time.Now()
	elapsed := time.Duration(0)
	r.activeNow = func() time.Time { return origin.Add(elapsed) }
	if err := r.LinkCurrentSegmentRun("run-1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		elapsed = 100 * time.Millisecond
		if err := r.RecordMonitorSample(true, testStart.Add(time.Duration(i)*time.Nanosecond)); err != nil {
			t.Fatal(err)
		}
		if err := r.RecordPortfolioWalk(true, true,
			testStart.Add(time.Duration(i)*time.Nanosecond)); err != nil {
			t.Fatal(err)
		}
	}
	segment := r.Snapshot().Segments[0]
	if segment.MonitorSlots != (MonitorSlotCoverage{Observed: 1, Fresh: 1}) {
		t.Fatalf("monitor burst filled slots: %+v", segment.MonitorSlots)
	}
	if segment.PortfolioSlots.Observed != 1 ||
		segment.PortfolioSlots.FreshComplete != 1 {
		t.Fatalf("portfolio burst filled slots: %+v", segment.PortfolioSlots)
	}

	elapsed = time.Second + time.Nanosecond
	if err := r.RecordMonitorSample(true, testStart.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	elapsed = 5*time.Second + time.Nanosecond
	if err := r.RecordPortfolioWalk(true, true, testStart.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	segment = r.Snapshot().Segments[0]
	if segment.MonitorSlots.Observed != 2 || segment.MonitorSlots.Fresh != 2 ||
		segment.PortfolioSlots.Observed != 2 || segment.PortfolioSlots.FreshComplete != 2 {
		t.Fatalf("new cadence slots were not counted once: %+v %+v",
			segment.MonitorSlots, segment.PortfolioSlots)
	}
}

func TestResumeAllowsWallStartEarlierThanPriorSegment(t *testing.T) {
	r, path := openTestRecorder(t)
	if err := r.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(path, testMetadata(), SegmentStart{
		ID: "process-2", PID: 202, StartedAt: testStart.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("resume after wall rollback: %v", err)
	}
	if got := resumed.Snapshot().Segments; len(got) != 2 || got[1].ID != "process-2" {
		t.Fatalf("wall rollback changed segment sequence: %#v", got)
	}
}

func TestFreshnessAndGenericEventCounters(t *testing.T) {
	r, _ := openTestRecorder(t)
	if err := r.RecordMonitorSample(true, testStart.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordMonitorSample(false, testStart.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordPortfolioWalk(true, true, testStart.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordPortfolioWalk(true, false, testStart.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordEvent(EventForced, "clean_disconnect", testStart.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordEvent(EventForced, "clean_disconnect", testStart.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, category := range []EventCategory{EventState, EventAnomaly, EventHeartbeat} {
		if err := r.RecordEvent(category, "observed", testStart.Add(7*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	s := r.Snapshot()
	if s.Monitor.Checks != 2 || s.Monitor.Fresh != 1 || s.Monitor.Stale != 1 {
		t.Fatalf("monitor counters = %+v", s.Monitor)
	}
	if s.Portfolio.Walks != 2 || s.Portfolio.Fresh != 2 ||
		s.Portfolio.Complete != 1 || s.Portfolio.Incomplete != 1 ||
		s.Portfolio.FreshComplete != 1 {
		t.Fatalf("portfolio counters = %+v", s.Portfolio)
	}
	if len(s.Events) != 4 {
		t.Fatalf("event counters = %d, want 4", len(s.Events))
	}
	if s.Events[0].Category != EventAnomaly || s.Events[1].Category != EventForced ||
		s.Events[1].Count != 2 || s.Events[2].Category != EventHeartbeat ||
		s.Events[3].Category != EventState {
		t.Fatalf("event counters not stable/correct: %#v", s.Events)
	}
}

func TestResumeAppendsProcessSegmentAndPreservesCounts(t *testing.T) {
	r, path := openTestRecorder(t)
	attempted, err := r.WrapAttemptDoer(&countingDoer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempted.Do(context.Background(), rest.Request{
		Method: "GET", Path: "/portfolio/balance",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordEvent(EventHeartbeat, "sent", testStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.EndSegment(testStart.Add(2*time.Minute), "restart requested"); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	resumed, err := Open(path, testMetadata(), SegmentStart{
		ID: "process-2", PID: 202, StartedAt: testStart.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	s := resumed.Snapshot()
	if len(s.Segments) != 2 || s.Segments[0].EndedAt == nil ||
		s.Segments[1].EndedAt != nil || s.Segments[1].PID != 202 {
		t.Fatalf("segments after resume = %#v", s.Segments)
	}
	if len(s.Events) != 1 || s.Events[0].Count != 1 {
		t.Fatalf("events lost on resume: %#v", s.Events)
	}
	if len(s.AttemptedHTTP) != 1 || s.AttemptedHTTP[0].Method != "GET" ||
		s.AttemptedHTTP[0].Count != 1 {
		t.Fatalf("attempted HTTP counts lost on resume: %#v", s.AttemptedHTTP)
	}

	onDisk, err := readEvidence(path)
	if err != nil {
		t.Fatalf("read resumed checkpoint: %v", err)
	}
	if len(onDisk.Segments) != 2 {
		t.Fatalf("durable segments = %d, want 2", len(onDisk.Segments))
	}
}

func TestOpenRefusesCorruptionMismatchAndFinalization(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "qualification.json")
		bad := []byte(`{"metadata":`)
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(path, testMetadata(), SegmentStart{
			ID: "process-1", PID: 1, StartedAt: testStart,
		})
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open corrupt error = %v, want ErrCorrupt", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil || !reflect.DeepEqual(got, bad) {
			t.Fatalf("corrupt checkpoint was changed: bytes=%q err=%v", got, readErr)
		}
	})

	t.Run("metadata mismatch", func(t *testing.T) {
		_, path := openTestRecorder(t)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		other := testMetadata()
		other.ConfigHash = "sha256:different"
		_, err = Open(path, other, SegmentStart{
			ID: "process-2", PID: 2, StartedAt: testStart.Add(time.Minute),
		})
		if !errors.Is(err, ErrMetadataMismatch) {
			t.Fatalf("Open mismatch error = %v, want ErrMetadataMismatch", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("mismatched checkpoint was changed: err=%v", readErr)
		}
	})

	t.Run("finalized", func(t *testing.T) {
		r, path := openTestRecorder(t)
		if err := r.Finalize(testStart.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, err := Open(path, testMetadata(), SegmentStart{
			ID: "process-2", PID: 2, StartedAt: testStart.Add(2 * time.Hour),
		})
		if !errors.Is(err, ErrFinalized) {
			t.Fatalf("Open finalized error = %v, want ErrFinalized", err)
		}
		if err := r.RecordEvent(EventState, "late", testStart.Add(2*time.Hour)); !errors.Is(err, ErrFinalized) {
			t.Fatalf("update finalized error = %v, want ErrFinalized", err)
		}
	})
}

func TestAtomicCheckpointPreservesPreviousFileOnRenameFailure(t *testing.T) {
	r, path := openTestRecorder(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordEvent(EventState, "RUNNING", testStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	realRename := r.ops.rename
	r.ops.rename = func(string, string) error { return errors.New("injected rename failure") }
	if err := r.Checkpoint(); err == nil {
		t.Fatal("Checkpoint succeeded despite injected rename failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("failed atomic checkpoint changed the previous file")
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".qualification.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("failed checkpoint left temporary files: %v", temps)
	}

	r.ops.rename = realRename
	if err := r.Checkpoint(); err != nil {
		t.Fatalf("retry checkpoint: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint mode = %04o, want 0600", info.Mode().Perm())
	}
	onDisk, err := readEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Events) != 1 || onDisk.Events[0].Name != "RUNNING" {
		t.Fatalf("successful retry did not preserve update: %#v", onDisk.Events)
	}
}

func TestCheckpointFileIODoesNotBlockObservations(t *testing.T) {
	r, _ := openTestRecorder(t)
	renameEntered := make(chan struct{})
	secondRenameEntered := make(chan struct{})
	releaseRename := make(chan struct{})
	realRename := r.ops.rename
	var renameCalls atomic.Uint64
	r.ops.rename = func(from, to string) error {
		if renameCalls.Add(1) == 1 {
			close(renameEntered)
			<-releaseRename
		} else {
			close(secondRenameEntered)
		}
		return realRename(from, to)
	}

	checkpointDone := make(chan error, 1)
	go func() { checkpointDone <- r.Checkpoint() }()
	select {
	case <-renameEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint did not reach the injected rename")
	}

	observationDone := make(chan error, 1)
	go func() {
		observationDone <- r.RecordEvent(
			EventHeartbeat, "sent while checkpoint blocked", testStart.Add(time.Minute))
	}()
	select {
	case err := <-observationDone:
		if err != nil {
			t.Fatalf("observation during checkpoint: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("filesystem I/O held the observation mutex")
	}

	// A second checkpoint must wait behind the first, then capture the newer
	// observation.  Otherwise an older delayed rename could overwrite it.
	secondCheckpointDone := make(chan error, 1)
	go func() { secondCheckpointDone <- r.Checkpoint() }()
	select {
	case <-secondRenameEntered:
		t.Fatal("concurrent checkpoints were not serialized")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRename)
	if err := <-checkpointDone; err != nil {
		t.Fatalf("checkpoint after releasing rename: %v", err)
	}
	if err := <-secondCheckpointDone; err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}
	if got := r.Snapshot().Events; len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("observation made during checkpoint was lost from memory: %#v", got)
	}
	onDisk, err := readEvidence(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Events) != 1 || onDisk.Events[0].Count != 1 {
		t.Fatalf("newer serialized checkpoint did not reach disk: %#v", onDisk.Events)
	}
}

func TestRaceSafeUpdatesAndSnapshots(t *testing.T) {
	r, _ := openTestRecorder(t)
	next := &countingDoer{}
	wrapped, err := r.WrapDoer(next)
	if err != nil {
		t.Fatal(err)
	}
	attempted, err := r.WrapAttemptDoer(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := WouldWriteFingerprint{
		Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
		Price: "45", Quantity: "1", State: "ADDING",
	}

	const goroutines = 16
	const iterations = 100
	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := r.RecordWouldWrite(fingerprint, testStart.Add(time.Second)); err != nil {
					errCh <- err
					return
				}
				if err := r.RecordMonitorSample(i%2 == 0, testStart.Add(time.Second)); err != nil {
					errCh <- err
					return
				}
				if err := r.RecordPortfolioWalk(true, i%2 == 0, testStart.Add(time.Second)); err != nil {
					errCh <- err
					return
				}
				if err := r.RecordEvent(EventHeartbeat, "sent", testStart.Add(time.Second)); err != nil {
					errCh <- err
					return
				}
				if _, err := attempted.Do(context.Background(), rest.Request{
					Method: "GET", Path: "/portfolio/balance",
				}); err != nil {
					errCh <- err
					return
				}
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	want := uint64(goroutines * iterations)
	s := r.Snapshot()
	if len(s.WouldWrites) != 1 || s.WouldWrites[0].Observations != want {
		t.Fatalf("would-write observations = %#v, want %d", s.WouldWrites, want)
	}
	if s.Monitor.Checks != want || s.Portfolio.Walks != want ||
		len(s.Events) != 1 || s.Events[0].Count != want ||
		len(s.AttemptedHTTP) != 1 || s.AttemptedHTTP[0].Count != want ||
		len(s.HTTP) != 1 || s.HTTP[0].Count != want || next.calls.Load() != want {
		t.Fatalf("concurrent totals wrong: monitor=%d portfolio=%d events=%#v attempted=%#v HTTP=%#v next=%d want=%d",
			s.Monitor.Checks, s.Portfolio.Walks, s.Events, s.AttemptedHTTP,
			s.HTTP, next.calls.Load(), want)
	}
}

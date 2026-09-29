package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/qual"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

func TestRunRefusesLiveQualificationBeforeReadingConfig(t *testing.T) {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	err := run(fs, filepath.Join(t.TempDir(), "missing.json"),
		filepath.Join(t.TempDir(), "qualification.json"), "", "", "",
		false, false, false, true)
	if err == nil {
		t.Fatal("-live and -qualification were accepted together")
	}
	var refused *refusal
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "zero-write") {
		t.Fatalf("error = %v, want a zero-write start refusal", err)
	}
}

// The composed test for the offline assessor.  It is one test rather than
// several because the three things it asserts are one property: the only way to
// ask this binary about a bundle is an invocation that reads the file, says out
// loud what it did not prove, and cannot report success for evidence that did
// not meet the local bar.
func TestOfflineAssessmentPrintsLocalLimitsAndCannotAwardQ01(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qualification.json")
	recorder, err := qual.Open(path, qual.Metadata{
		SchemaVersion: qual.SchemaVersion, ConfigHash: "sha256:assess",
		BinaryIdentity: "sha256:test", Ticker: "KXTEST-A", Rung: "canary",
		Live: false,
	}, qual.SegmentStart{
		ID: "assessed-segment", PID: os.Getpid(), StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("qual.Open: %v", err)
	}
	if err := recorder.Finalize(time.Now().UTC()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the finalized bundle: %v", err)
	}

	// A structurally valid bundle that plainly did not run for four hours. The
	// assessment must be PRINTED -- an operator needs the reasons -- and the
	// call must still fail, because a zero-length rehearsal that exits 0 is
	// exactly the artifact that gets quoted as "q01 passed".
	var out bytes.Buffer
	err = assessQualificationBundle(path, &out)
	if err == nil {
		t.Fatal("the offline assessor reported success for a bundle that met " +
			"none of the local q01 requirements")
	}

	var got struct {
		Scope                string   `json:"scope"`
		LocalRequirementsMet bool     `json:"local_requirements_met"`
		ExternalOutstanding  []string `json:"external_outstanding"`
		Failures             []struct {
			Code string `json:"code"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the assessment is not JSON: %v\n%s", err, out.String())
	}
	if got.Scope != qual.Q01LocalScope {
		t.Fatalf("assessment scope = %q, want %q", got.Scope, qual.Q01LocalScope)
	}
	if got.LocalRequirementsMet {
		t.Fatal("local_requirements_met is true for a bundle with no elapsed time")
	}
	if len(got.Failures) == 0 {
		t.Fatal("the assessment lists no failures and still did not qualify")
	}
	if len(got.ExternalOutstanding) == 0 {
		t.Fatal("the assessment claims nothing external is outstanding; a local " +
			"verdict that stops naming its own limits is one that reads as q01")
	}

	// Read-only in the literal sense: the judge does not touch the artifact.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the bundle: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("assessing the bundle rewrote it")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the bundle: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %04o, want 0600", info.Mode().Perm())
	}

	// The wiring, asserted through the real flag path and BEFORE the config
	// check: an assessment needs no config, so reaching the no-config refusal
	// here would mean the branch had moved below it or vanished.
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	missing := filepath.Join(dir, "not-a-bundle.json")
	err = run(fs, "", "", missing, "", "", false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "reading qualification evidence") {
		t.Fatalf("run(-assess-qualification %s) error = %v, want the assessor's "+
			"own read failure", missing, err)
	}

	// And it is the ONLY thing that invocation does.
	refusals := []struct {
		name string
		call func() error
	}{
		{"-config", func() error {
			return run(fs, filepath.Join(dir, "config.json"), "", path, "", "",
				false, false, false, false)
		}},
		{"-live", func() error {
			return run(fs, "", "", path, "", "", false, false, false, true)
		}},
		{"-provision", func() error {
			return run(fs, "", "", path, "", "", true, false, false, false)
		}},
		{"-deploy", func() error {
			return run(fs, "", "", path, "", "", false, true, false, false)
		}},
		{"-qualification", func() error {
			return run(fs, "", filepath.Join(dir, "live-bundle.json"), path, "",
				"", false, false, false, false)
		}},
	}
	for _, tc := range refusals {
		err := tc.call()
		var ref *refusal
		if !errors.As(err, &ref) ||
			!strings.Contains(err.Error(), "reads one preserved evidence file") {
			t.Errorf("-assess-qualification with %s error = %v, want a refusal",
				tc.name, err)
		}
	}
}

func TestOpenQualificationRefusesRelativePathAndLiveMode(t *testing.T) {
	c := config{
		Ticker: "KXTEST-A", Rung: rungs["canary"],
		ConfigHash: "sha256:config",
	}
	if _, err := openQualification("qualification.json", c); err == nil ||
		!strings.Contains(err.Error(), "relative") {
		t.Fatalf("relative evidence path error = %v", err)
	}
	c.Live = true
	if _, err := openQualification(filepath.Join(t.TempDir(), "q.json"), c); err == nil ||
		!strings.Contains(err.Error(), "zero-write") {
		t.Fatalf("live qualification error = %v", err)
	}
}

func TestSecondProcessCannotAppendQualificationBeforeLockRefusal(t *testing.T) {
	dir := t.TempDir()
	c := config{
		Ticker: "KXTEST-A", Rung: rungs["canary"],
		ConfigHash: "sha256:config",
		Paths:      paths{Lock: filepath.Join(dir, "harness.lock")},
	}
	first, err := acquireHarnessLock(c)
	if err != nil {
		t.Fatalf("acquire first harness lock: %v", err)
	}
	t.Cleanup(func() { first.Close() })

	evidence := filepath.Join(dir, "qualification.json")
	if _, _, err := lockThenOpenQualification(evidence, c); err == nil {
		t.Fatal("second process acquired the held harness lock")
	}
	if _, err := os.Stat(evidence); !os.IsNotExist(err) {
		t.Fatalf("losing process touched qualification evidence before lock refusal: %v", err)
	}
}

func TestQualificationSegmentLinksOnlyToTheCommittedRun(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	snapshot := h.qual.Snapshot()
	if len(snapshot.Segments) != 1 {
		t.Fatalf("qualification segments = %d, want 1", len(snapshot.Segments))
	}
	if got, want := snapshot.Segments[0].RunID, h.rig.run.RunID(); got != want {
		t.Fatalf("qualification run link = %q, want committed run %q", got, want)
	}
}

func TestQualificationRecordsEveryAnomalySubmissionBoundary(t *testing.T) {
	f := newFixture(t, nil)
	qrec, err := qual.Open(filepath.Join(t.TempDir(), "qualification.json"),
		qual.Metadata{
			SchemaVersion: qual.SchemaVersion, ConfigHash: "sha256:anomaly-flow",
			BinaryIdentity: "sha256:test", Ticker: "KXTEST-A", Rung: "canary",
			Live: false,
		}, qual.SegmentStart{
			ID: "anomaly-flow", PID: os.Getpid(), StartedAt: time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("qual.Open: %v", err)
	}
	f.rig.qual = qrec

	f.sd.submitAnomaly(risk.Anomaly{
		Class: "TEST_NOTICE", Sev: risk.SEV2, Text: "ordinary queued anomaly",
	})
	f.rig.anom.dropped.Add(2)
	f.sd.handleAnomalies()
	// This malformed value models the store refusing an anomaly submission.
	// The fixed failure class must reach the independent qualification file
	// even when neither SQLite nor its anomaly journal accepted the row.
	f.sd.submitAnomaly(risk.Anomaly{})

	counts := make(map[string]uint64)
	for _, event := range qrec.Snapshot().Events {
		if event.Category == qual.EventAnomaly {
			counts[event.Name] = event.Count
		}
	}
	for _, name := range []string{
		"SEV2:TEST_NOTICE", "SEV1:ANOMALY_DROPPED",
		"SEV1:ANOMALY_SUBMISSION_FAILED",
	} {
		if counts[name] == 0 {
			t.Fatalf("qualification anomaly counts = %v, missing %s", counts, name)
		}
	}
}

func qualificationCreate(t *testing.T, price int, count num.Qty,
	role quote.Role, seq uint64) writeRequest {

	t.Helper()
	coid, err := rest.Coid("QUALTEST", 0, quote.SideYes, seq)
	if err != nil {
		t.Fatalf("Coid: %v", err)
	}
	order, err := rest.NewCreateOrder("KXTEST-A", quote.SideYes, price,
		count, count, coid)
	if err != nil {
		t.Fatalf("NewCreateOrder: %v", err)
	}
	return writeRequest{
		Market: "KXTEST-A", Side: quote.SideYes, Role: role,
		Op: quote.OpPlace, Order: order,
	}
}

func TestWouldWriteFingerprintIncludesEveryMaterialCreateField(t *testing.T) {
	one := num.QtyFromFloat(1)
	base := qualificationCreate(t, 40, one, quote.RoleReducing, 1)
	want := wouldWriteFingerprint(base, quote.Running, quote.Reducing, 7)

	// A fresh coid is owner-loop noise and must stay in the same episode.
	same := qualificationCreate(t, 40, one, quote.RoleReducing, 2)
	if got := wouldWriteFingerprint(same, quote.Running, quote.Reducing, 7); got != want {
		t.Fatalf("coid-only change altered the fingerprint:\n got %+v\nwant %+v", got, want)
	}

	changes := []struct {
		name string
		req  writeRequest
		g    quote.GlobalState
		m    quote.MarketState
		gen  uint64
	}{
		{"price", qualificationCreate(t, 41, one, quote.RoleReducing, 3), quote.Running, quote.Reducing, 7},
		{"quantity", qualificationCreate(t, 40, num.QtyFromFloat(2), quote.RoleReducing, 4), quote.Running, quote.Reducing, 7},
		{"role", qualificationCreate(t, 40, one, quote.RoleAdding, 5), quote.Running, quote.Reducing, 7},
		{"global", base, quote.WindingDown, quote.Reducing, 7},
		{"market", base, quote.Running, quote.Settling, 7},
		{"truth generation", base, quote.Running, quote.Reducing, 8},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			if got := wouldWriteFingerprint(tc.req, tc.g, tc.m, tc.gen); got == want {
				t.Fatalf("material %s change was deduplicated: %+v", tc.name, got)
			}
		})
	}
}

func TestWouldWriteFingerprintSortsCancelTargetsAndIncludesRemaining(t *testing.T) {
	one := num.QtyFromFloat(1)
	two := num.QtyFromFloat(2)
	orders := []rest.Order{
		{OrderID: "b", Ticker: "KXTEST-A", Side: quote.SideYes, Price4: 4100, Remaining: two},
		{OrderID: "a", Ticker: "KXTEST-A", Side: quote.SideYes, Price4: 4000, Remaining: one},
	}
	req := writeRequest{
		Market: "KXTEST-A", Side: quote.SideYes, Role: quote.RoleReducing,
		Op: quote.OpCancel, Orders: orders,
	}
	want := wouldWriteFingerprint(req, quote.Running, quote.Reducing, 7)
	req.Orders[0], req.Orders[1] = req.Orders[1], req.Orders[0]
	if got := wouldWriteFingerprint(req, quote.Running, quote.Reducing, 7); got != want {
		t.Fatalf("target ordering changed the cancel fingerprint:\n got %+v\nwant %+v", got, want)
	}
	req.Orders[0].Remaining = two
	if got := wouldWriteFingerprint(req, quote.Running, quote.Reducing, 7); got == want {
		t.Fatalf("changed remaining quantity was deduplicated: %+v", got)
	}
}

// confidence: high

// TestFinalizedEvidenceDoesNotFailTheQualification is `lip-oqq`.
//
// Shutdown freezes the evidence once the trading store has stopped
// (shutdown.go:304), but the owner's snapshot publisher is a separate goroutine
// and gets at least one more tick in afterwards, calling RecordEvent for its
// state summary (run.go:3086-3091). That call returns ErrFinalized, and until
// this fix the fail-closed channel turned it into `serve` returning
// "qualification evidence failed".
//
// SIGTERM is the operator's NORMAL stop, so this fired at the end of every run.
// The first real q01 attempt ended with exactly that line in harness.err, over
// an evidence file that had finalized correctly and was fully assessable -- a
// clean four-to-six-hour qualification would have reported FAILED at the moment
// it completed.
//
// The distinction being asserted: a write refused BECAUSE the artifact is
// already closed is not a failure to record. Every other error still fails
// closed, which the second half checks.
func TestFinalizedEvidenceDoesNotFailTheQualification(t *testing.T) {
	recorder, err := qual.Open(filepath.Join(t.TempDir(), "qualification.json"),
		qual.Metadata{
			SchemaVersion: qual.SchemaVersion, ConfigHash: "sha256:finalize-race",
			BinaryIdentity: "sha256:test", Ticker: "KXTEST-A", Rung: "canary",
			Live: false,
		}, qual.SegmentStart{
			ID: "finalize-race", PID: os.Getpid(), StartedAt: time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("qual.Open: %v", err)
	}
	if err := recorder.Finalize(time.Now().UTC()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// The real post-finalize write, through the real recorder, wrapped exactly
	// as run.go wraps it.
	late := recorder.RecordEvent("state", "global=DRAINED;market=IDLE", time.Now().UTC())
	if late == nil {
		t.Fatal("RecordEvent after Finalize returned nil; this test is no " +
			"longer exercising the race it was written for")
	}

	r := &rig{qualErrors: make(chan error, 1)}
	r.failQualification(fmt.Errorf("recording state summary: %w", late))
	select {
	case got := <-r.qualErrors:
		t.Fatalf("a post-finalize write failed the qualification: %v\n\n"+
			"The evidence artifact was closed properly and is assessable. "+
			"Reporting that as a failure means every clean run ends by "+
			"declaring itself failed (lip-oqq).", got)
	default:
	}

	// Fail-closed is otherwise intact: anything that is NOT the artifact being
	// closed still invalidates the qualification.
	r.failQualification(fmt.Errorf("recording state summary: %w",
		errors.New("disk full")))
	select {
	case <-r.qualErrors:
	default:
		t.Fatal("a genuine evidence failure was swallowed. Only ErrFinalized " +
			"is exempt; everything else must still fail the qualification")
	}
}

// lip-tdz: every rig has the qualification failure channel, and a failure
// reported through it ends serve, fail closed.
func TestQualificationFailureEndsServe(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	h.start()
	h.awaitActionable()
	h.rig.failQualification(errors.New("injected evidence failure"))
	select {
	case <-h.serveDone:
	case <-time.After(seamBudget):
		t.Fatal("serve kept running after a qualification failure")
	}
	if h.serveErr == nil || !strings.Contains(h.serveErr.Error(), "qualification evidence failed") {
		t.Fatalf("serve returned %v, want the qualification failure", h.serveErr)
	}
}

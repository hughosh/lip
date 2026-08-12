package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// TestCauselessStopCannotEnterWindingDown closes the hole that
// `GlobalController` existed to close and did not.
//
// §5.1's RUNNING rule is `if in.Stop { return WindingDown, GTStop }`, so a caller
// that sets `Stop` on the input and supplies no cause used to get a committed
// WINDING_DOWN with nothing on disk. That is every §12 trigger -- `inv_kill`,
// `pnl_kill`, a taker fill, `insufficient_balance`, hard position drift, the
// `harness.stop` sentinel -- one forgotten argument away from a halt that a panic
// and `launchd KeepAlive` erase completely.
//
// The guard is stated over the OUTPUT rather than over the arguments: no
// publishable transition into WINDING_DOWN unless the durable state on disk
// already justifies it.
//
// `M-L-CAUSELESS` lets it through.
func TestCauselessStopCannotEnterWindingDown(t *testing.T) {
	store := &recordingLatch{}
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}

	// Establish that the INPUT genuinely produces WINDING_DOWN in §5.1, or this
	// test proves nothing.
	in := quote.GlobalInput{State: quote.Running, Stop: true}
	if st, _ := quote.NextGlobal(in); st != quote.WindingDown {
		t.Fatalf("the control input produces %v in NextGlobal; this test would "+
			"pass vacuously", st)
	}

	dec := ctrl.Advance(in)
	if dec.Committed {
		t.Fatal("a stop with no cause was committed")
	}
	if dec.State != quote.Running {
		t.Fatalf("state %v, want RUNNING: publishing WINDING_DOWN here is a "+
			"halt that a panic and a launchd restart erase with no operator "+
			"action (HR-009)", dec.State)
	}
	if !dec.BlockAdding || !dec.RetryLatch {
		t.Fatalf("BlockAdding=%v RetryLatch=%v, want both true",
			dec.BlockAdding, dec.RetryLatch)
	}
	if !hasClass(dec.Anomalies, "STOP_CAUSE_MISSING") {
		t.Fatalf("no STOP_CAUSE_MISSING; got %v", classesOf(dec.Anomalies))
	}
	if sev, _ := sevOf(dec.Anomalies, "STOP_CAUSE_MISSING"); sev != risk.SEV1 {
		t.Fatalf("STOP_CAUSE_MISSING severity %v, want SEV1", sev)
	}
	if len(store.ensures) != 0 {
		t.Fatalf("a causeless stop reached the disk: %+v", store.ensures)
	}

	// With a cause committed first, the same input transitions and IS durable.
	if c := ctrl.CommitStop(StopCause{Trigger: "pnl_kill", TsMillis: 5}); !c.Durable {
		t.Fatalf("a well-formed cause was not made durable: %+v", c)
	}
	ok := ctrl.Advance(in)
	if !ok.Committed || ok.State != quote.WindingDown {
		t.Fatalf("a well-formed cause produced %+v", ok)
	}

	// And once the latch IS on disk, an advance with no further commit is fine
	// -- the durable state already justifies the transition.
	after := ctrl.Advance(in)
	if !after.Committed || after.State != quote.WindingDown {
		t.Fatalf("after latching, a causeless stop produced %+v", after)
	}
}

// TestStartupCommitsEveryCauseBeforeReturningDecision is R2.
//
// An earlier version returned the causes ON the adoption for a caller to latch
// later. The foreign-fill test then proved only that a diligent caller COULD do
// the right thing, which is not a property of the code -- and a one-tick RUNNING
// exposure is available to any caller that latches on the next iteration instead
// of this one.
//
// `M-L-STARTUPCAUSE` returns the completed startup before commitment.
func TestStartupCommitsEveryCauseBeforeReturningDecision(t *testing.T) {
	store := &recordingLatch{}
	src := okSource()
	// An OWNED TAKER fill, not a foreign one. Since `lip-a3s` a foreign fill
	// already on the account is reported at startup without latching, so it is
	// no longer a vehicle for testing the COMMIT ORDERING this test is about.
	// An owned taker fill still commits `startup_fill_history`
	// (startup.go:735-745), which exercises the same ordering property.
	src.fills = rest.FillsResult{Walk: completeWalk(),
		Fills: []rest.Fill{takerFill("t1", "ours", "M")}}

	s := newStartup(t, store, src, ownsAll("ours"), keepAll(), newSweeper(true))
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption")
	}

	// The latch is already on disk by the time the licence exists.
	if len(store.ensures) != 1 ||
		store.ensures[0].Trigger != "startup_fill_history" {
		t.Fatalf("the startup fill history was not latched before the "+
			"adoption was returned: %+v", store.ensures)
	}
	// And the decision is the halted one, not RUNNING.
	if at.Decision.State != quote.WindingDown ||
		at.Decision.Trigger != quote.GTLatch {
		t.Fatalf("decision %v/%v, want WINDING_DOWN/GTLatch: a complete "+
			"reconciliation with a foreign fill in it must not expose RUNNING "+
			"for even one tick", at.Decision.State, at.Decision.Trigger)
	}
	if !at.Decision.Committed {
		t.Fatal("the decision was not committed")
	}
}

// TestStartupWithNoCauseReachesRunningThroughTheStateMachine is the other half:
// the ordinary path is adjudicated too, rather than assigned.
func TestStartupWithNoCauseReachesRunningThroughTheStateMachine(t *testing.T) {
	store := &recordingLatch{}
	s := newStartup(t, store, okSource(), ownsAll(), keepAll(), newSweeper(true))

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}
	if at.Decision.State != quote.Running ||
		at.Decision.Trigger != quote.GTReconciled {
		t.Fatalf("decision %v/%v, want RUNNING/GTReconciled",
			at.Decision.State, at.Decision.Trigger)
	}
	if len(store.ensures) != 0 {
		t.Fatalf("a clean startup wrote a latch: %+v", store.ensures)
	}
}

// TestLatchedStartupFailureCannotRegressToStartingOrUnknownRisk is R2's fourth
// bullet.
//
// A process that already holds a durable halt does not get handed STARTING or
// UNKNOWN_RISK by the very procedure H-HALT-4 exists to gate, whatever the reads
// did. A14 outranks everything, including UNKNOWN_RISK's own recovery gate.
func TestLatchedStartupFailureCannotRegressToStartingOrUnknownRisk(t *testing.T) {
	store := &recordingLatch{
		present: true,
		rec:     LatchRecord{Version: 1, Trigger: "taker_fill", TsMillis: 1},
	}
	src := okSource()
	src.positions = rest.PositionsResult{Walk: failedWalk("503")}
	s := newStartup(t, store, src, ownsAll(), keepAll(), newSweeper(true))

	for i := 1; i <= 5; i++ {
		at := s.Step(context.Background(), startupNow)
		if at.Err == nil {
			t.Fatalf("attempt %d succeeded", i)
		}
		if at.Decision.State != quote.WindingDown {
			t.Fatalf("attempt %d produced %v on a latched process; a durable "+
				"halt outranks every startup outcome (A14)", i, at.Decision.State)
		}
		if !at.Retry {
			t.Fatalf("attempt %d stopped retrying", i)
		}
	}

	// And the same on a SUCCESSFUL reconciliation.
	src.positions = rest.PositionsResult{
		Walk: completeWalk(), ByTicker: map[string]num.Qty{},
	}
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}
	// A latched process that completes a full reconciliation and finds the
	// account genuinely empty DRAINS, and that is the `RiskKnown` gate working
	// rather than the failure it was built to prevent.
	//
	// The earlier attempts already took STARTING to WINDING_DOWN through A14, so
	// that edge is asserted above. This pass is complete, FINAL and cancelled
	// nothing, which is the only shape of pass that sets `RiskKnown` -- so both
	// risk flags are ANSWERS here rather than unpopulated zeroes, and §5.1's
	// drain edge is legitimately open. DRAINED does not exit: it idles, keeps
	// monitoring, and re-enters WINDING_DOWN the moment inventory or a live
	// order reappears, so nothing is abandoned by reaching it.
	//
	// The property this test is NAMED for is asserted directly rather than
	// inferred from a single expected state, which is the stronger form: it is
	// STARTING and UNKNOWN_RISK that a latched process may never reach.
	if at.Decision.State == quote.Starting ||
		at.Decision.State == quote.UnknownRisk {
		t.Fatalf("a latched process reconciled into %v; A14 makes the durable "+
			"halt outrank every startup outcome", at.Decision.State)
	}
	if at.Decision.State != quote.Drained ||
		at.Decision.Trigger != quote.GTDrained {
		t.Fatalf("a latched process that completely reconciled a flat, "+
			"order-free account produced %v/%v, want DRAINED/GTDrained",
			at.Decision.State, at.Decision.Trigger)
	}
}

// TestExternalCodeCannotForgeCompleteAdoption is R3, asserted from where it
// matters.
//
// The seal test proves the TYPE cannot be implemented; this proves the
// consequence the type exists for. See seal_test.go for the structural half.
//
// `M-L-ADOPTIONFORGE` removes the private marker method.
func TestExternalCodeCannotForgeCompleteAdoption(t *testing.T) {
	// An in-package forgery is possible by construction -- this package builds
	// them. What must not be possible is an EXTERNAL one, which is what the
	// unexported method prevents and what seal_test.go asserts by reflection.
	typ := reflect.TypeOf((*Adoption)(nil)).Elem()
	if typ.Kind() != reflect.Interface {
		t.Fatalf("Adoption is a %v", typ.Kind())
	}
	unexported := 0
	for i := 0; i < typ.NumMethod(); i++ {
		if !typ.Method(i).IsExported() {
			unexported++
		}
	}
	if unexported == 0 {
		t.Fatal("Adoption has no unexported method, so any package can " +
			"implement it and manufacture the licence to leave STARTING")
	}

	// The zero value is nil, not an empty licence claiming a flat account.
	var zero Adoption
	if zero != nil {
		t.Fatal("the zero Adoption is not nil")
	}
	// A failed attempt hands back nothing at all.
	src := okSource()
	src.positions = rest.PositionsResult{Walk: failedWalk("503")}
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true))
	if at := s.Step(context.Background(), startupNow); at.Adoption != nil {
		t.Fatal("a failed attempt returned a non-nil Adoption")
	}
}

// TestForeignFillWithUnusableFeeStillLatches is R5's second bullet, and it is
// the subtlest of the ten.
//
// `rest.decodeFill` accepts an absent `fee_cost`. An earlier version converted
// EVERY fill before classifying ownership, so a foreign fill with an unreadable
// fee failed conversion and became a generic retryable startup error. It never
// reached `FOREIGN_FILL`, never latched, and after `backfill_h` (24h) it fell
// out of the walk entirely -- at which point the harness resumes with a position
// model that a third party has been trading, and no record that it ever noticed.
//
// Classification is by `order_id` against the ledger and needs no fee at all, so
// it goes first. Conversion then runs only over fills that are ours, where an
// unreadable fee genuinely is a hard error: it is one of H-ORD-8's two witnesses
// going silent.
//
// `M-L-FOREIGNFEE` converts all fills instead of owned fills.
func TestForeignFillWithUnusableFeeStillLatches(t *testing.T) {
	store := &recordingLatch{}
	src := okSource()
	bad := makerFill("t1", "not-ours", "M")
	bad.FeeCost = "" // unreadable, and not ours
	src.fills = rest.FillsResult{Walk: completeWalk(), Fills: []rest.Fill{bad}}

	s := newStartup(t, store, src, ownsAll(), keepAll(), newSweeper(true))
	at := s.Step(context.Background(), startupNow)

	if at.Err != nil {
		t.Fatalf("a foreign fill with an unusable fee failed the whole startup "+
			"as a conversion error instead of latching: %v", at.Err)
	}
	// Since `lip-a3s` a disclaimed fill already on the account is reported at
	// SEV2 and does not latch, so the assertion is that it is still CLASSIFIED
	// -- which is the property this test is actually about. The failure it
	// guards against is the fill never reaching classification at all, because
	// conversion ran first and choked on the unreadable fee.
	if !hasClass(at.Anomalies, "FOREIGN_FILL_INHERITED") {
		t.Fatalf("no FOREIGN_FILL_INHERITED; got %v", classesOf(at.Anomalies))
	}
	if len(store.ensures) != 0 {
		t.Fatalf("a fill already on the account at startup latched: %+v",
			store.ensures)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption")
	}
	if len(at.Adoption.OwnedFills()) != 0 {
		t.Fatal("a foreign fill entered the owned history")
	}
	if n := at.Adoption.Summary().ForeignFills; n != 1 {
		t.Fatalf("the startup summary reports %d foreign fills, want 1", n)
	}

	// An unreadable fee on a fill that IS ours is still a hard error: S2's
	// corroborator of H-ORD-8 cannot be evaluated without it.
	store2 := &recordingLatch{}
	src2 := okSource()
	mine := makerFill("t2", "mine", "M")
	mine.FeeCost = ""
	src2.fills = rest.FillsResult{Walk: completeWalk(), Fills: []rest.Fill{mine}}
	s2 := newStartup(t, store2, src2, ownsAll("mine"), keepAll(), newSweeper(true))
	if at2 := s2.Step(context.Background(), startupNow); at2.Err == nil {
		t.Fatal("an unreadable fee on one of OUR fills did not fail startup; " +
			"maker fees are $0.00 (S2), so the fee is one of the two witnesses " +
			"to a taker fill and it cannot be missing")
	}
}

// TestStartupRewalksAfterEveryCancelSweep is R5's third bullet.
//
// H-ORD-5c can cancel adopted orders, and the position read that preceded the
// cancellation cannot license quoting: an order that FILLED while it was being
// cancelled leaves `q` stale, and `q` sizes every reducer from here on. At S=1
// that is about a dollar; after promotion to S=12 it is about twelve, per
// occurrence, until the next authoritative poll.
//
// `M-L-SWEEPREPLAY` constructs the adoption immediately after a clean sweep.
func TestStartupRewalksAfterEveryCancelSweep(t *testing.T) {
	src := okSource()
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("stale-1", "M"),
	}}
	sweeper := newSweeperOn(src, true)
	s := newStartup(t, &recordingLatch{}, src,
		ownsAll(), &fixedPolicy{decision: AdoptionCancel}, sweeper, "M")

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	positions := 0
	for _, c := range src.calls {
		if c.what == "positions" {
			positions++
		}
	}
	if positions < 2 {
		t.Fatalf("positions was read %d time(s); a pass that cancelled must "+
			"discard its result and read all four endpoints again, because an "+
			"order that filled during cancellation leaves q stale", positions)
	}
	if !hasClass(at.Anomalies, "STARTUP_REWALK") {
		t.Fatalf("no STARTUP_REWALK; got %v", classesOf(at.Anomalies))
	}
	if at.Adoption.Summary().CancelledStale != 1 {
		t.Fatalf("summary reports %d cancelled, want 1",
			at.Adoption.Summary().CancelledStale)
	}

	// The rewalk observed the post-cancel account: nothing kept, nothing
	// resting.
	if n := len(at.Adoption.Kept()); n != 0 {
		t.Fatalf("kept %d orders after cancelling the only one", n)
	}
}

// TestStartupPortfolioContainsEveryKeptOrder is R5's fourth bullet.
//
// An adopted order that exists in `Kept()` and nowhere the risk model can see it
// is an exchange-fillable order that `AnyLiveOrder` reports as absent. §5.1 then
// declares the account DRAINED -- "flat is necessary and NOT sufficient" --
// and H-FAIL-3's "one ignored cancel away from being long again" is exactly the
// position we are in, with the drain authorised on top.
//
// `M-L-ADOPTRESTING` omits them.
func TestStartupPortfolioContainsEveryKeptOrder(t *testing.T) {
	src := okSource()
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("keep-1", "M"),
		ourOrder("keep-2", "M"),
		// The foreign order sits on a DIFFERENT ticker, and it has to.
		//
		// A foreign resting order excludes its market from new selection (F15),
		// and lip-eyq §4 makes the LIFECYCLE -- not the policy -- revoke an
		// adding order in an excluded market. `okSource` holds no position, so
		// q == 0 on M, so BOTH sides are adding there and both kept orders would
		// be revoked into a cancellation. Keeping "theirs" on M would therefore
		// assert that an excluded market's adding orders survive, which is the
		// opposite of the rule.
		//
		// It still proves what this test needs it to prove: a foreign order does
		// not enter OUR resting-order model, asserted on `ids["theirs"]` below.
		foreignOrder("theirs", "N"),
	}}
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), "M")

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	live := at.Adoption.Portfolio().LiveOrders()
	if len(live) != 2 {
		t.Fatalf("the portfolio holds %d resting orders, want 2; an adopted "+
			"order the risk model cannot see is one AnyLiveOrder reports absent, "+
			"and DRAINED rests no reducer", len(live))
	}
	ids := map[string]bool{}
	for _, o := range live {
		ids[o.OrderID] = true
	}
	for _, want := range []string{"keep-1", "keep-2"} {
		if !ids[want] {
			t.Fatalf("kept order %s is absent from the portfolio", want)
		}
	}
	if ids["theirs"] {
		t.Fatal("a foreign order was installed into OUR resting-order model")
	}

	// A cancelled order never enters it either.
	src2 := okSource()
	src2.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("stale-1", "M"),
	}}
	sweeper := newSweeperOn(src2, true)
	s2 := newStartup(t, &recordingLatch{}, src2, ownsAll(),
		&fixedPolicy{decision: AdoptionCancel}, sweeper, "M")
	at2 := s2.Step(context.Background(), startupNow)
	if at2.Err != nil {
		t.Fatalf("Run: %v", at2.Err)
	}
	if n := len(at2.Adoption.Portfolio().LiveOrders()); n != 0 {
		t.Fatalf("a cancelled and swept order is in the portfolio (%d resting)", n)
	}
	if got := at2.Adoption.Managed(); !reflect.DeepEqual(got, []string{"M"}) {
		t.Fatalf("managed %v, want [M] from selection alone", describe(got))
	}
}

// TestSleepDetectorUsesIndependentWallTime is R6, and the break it closes was
// invisible to every test in v1.
//
// `time.Now()` returns a Time carrying BOTH a wall reading and a monotonic
// reading, and `t.Sub(u)` uses the monotonic readings whenever both operands
// have one. The obvious signature -- `Observe(wall time.Time, mono
// time.Duration)` -- therefore computed a MONOTONIC delta and compared it
// against the monotonic delta, so the divergence was ~0 across any sleep and F7
// could never fire in production.
//
// The v1 tests passed because `time.Unix(...)` and `.Add(...)` produce Times with
// NO monotonic reading, where `Sub` correctly falls back to wall arithmetic. The
// bug was present in every production call and absent from every test.
//
// An int64 of Unix milliseconds cannot carry a hidden monotonic reading, so the
// two sources are structurally independent.
//
// `M-L-WALLCLOCK` uses the monotonic delta as the wall delta.
func TestSleepDetectorUsesIndependentWallTime(t *testing.T) {
	// The signature itself is the fix, so assert it: a time.Time parameter is
	// the defect.
	m, ok := reflect.TypeOf(&SleepDetector{}).MethodByName("Observe")
	if !ok {
		t.Fatal("SleepDetector has no Observe")
	}
	for i := 1; i < m.Type.NumIn(); i++ {
		if m.Type.In(i) == reflect.TypeOf(time.Time{}) {
			t.Fatal("SleepDetector.Observe takes a time.Time. Values from " +
				"time.Now() carry a monotonic reading and Sub uses it, so the " +
				"wall delta silently becomes the monotonic delta and F7 cannot " +
				"fire in production -- while tests built on time.Unix pass")
		}
	}

	// A real two-hour host sleep, expressed the way production expresses it: a
	// wall stamp that jumped, and a monotonic source that barely moved.
	var d SleepDetector
	wall := time.Now()
	d.Observe(wall.UnixMilli(), 0)
	eff := d.Observe(wall.Add(2*time.Hour).UnixMilli(), time.Second)

	if !eff.Detected {
		t.Fatal("a two-hour wall jump against a one-second monotonic advance " +
			"was not detected")
	}
	if eff.Downtime < 2*time.Hour-2*time.Second {
		t.Fatalf("downtime %v, want ~2h", eff.Downtime)
	}
	if !eff.Resnapshot || !eff.ReconcileNow {
		t.Fatalf("Resnapshot=%v ReconcileNow=%v", eff.Resnapshot, eff.ReconcileNow)
	}
}

// TestExistingLatchRetryResyncsParentBeforeDurable is R7.
//
// The sequence: the first Ensure creates the file, writes it, fsyncs it, closes
// it, and then fails to fsync the PARENT DIRECTORY. It correctly reports
// not-durable, so the caller blocks adding and retries. The retry sees EEXIST
// and -- in v1 -- returned durable immediately, without ever completing the
// directory sync the first attempt failed. A power cut then loses the directory
// entry, the restart finds no latch, and the harness resumes adding.
//
// `M-L-EXISTDURABLE` returns durable on EEXIST before the parent sync.
func TestExistingLatchRetryResyncsParentBeforeDurable(t *testing.T) {
	dir := t.TempDir()
	l, err := NewFileLatch(filepath.Join(dir, "harness.halt"))
	if err != nil {
		t.Fatalf("NewFileLatch: %v", err)
	}

	// Fail the parent-directory sync on the first attempt only.
	calls := 0
	l.syncDirFn = func(d string) error {
		calls++
		if calls == 1 {
			return errors.New("simulated directory fsync failure")
		}
		return syncDir(d)
	}

	rec := LatchRecord{Version: 1, Trigger: "taker_fill", TsMillis: 7, Market: "M"}

	durable, err := l.Ensure(rec)
	if durable || err == nil {
		t.Fatalf("the first Ensure reported durable=%v err=%v despite the "+
			"parent sync failing", durable, err)
	}
	// The file exists after that failure -- which is exactly why the retry path
	// matters.
	if _, serr := os.Stat(l.Path()); serr != nil {
		t.Fatalf("the failed Ensure left no file, so this test would not "+
			"exercise the EEXIST retry: %v", serr)
	}

	durable, err = l.Ensure(rec)
	if err != nil || !durable {
		t.Fatalf("the retry reported durable=%v err=%v", durable, err)
	}
	if calls != 2 {
		t.Fatalf("the parent directory was synced %d time(s); the retry must "+
			"finish the job the first attempt failed, or a power cut erases the "+
			"halt and the harness resumes adding on restart", calls)
	}

	// And a retry whose sync ALSO fails is still not durable.
	l2, err := NewFileLatch(filepath.Join(t.TempDir(), "harness.halt"))
	if err != nil {
		t.Fatalf("NewFileLatch: %v", err)
	}
	l2.syncDirFn = func(string) error { return errors.New("still failing") }
	if durable, _ := l2.Ensure(rec); durable {
		t.Fatal("a first Ensure with a failing dir sync reported durable")
	}
	if durable, _ := l2.Ensure(rec); durable {
		t.Fatal("an EEXIST retry with a failing dir sync reported durable")
	}
}

// TestStartupSummaryIsADeepCopy is R3's second half.
//
// `Summary()` returned the struct by value, which copies the ints and ALIASES
// every map and slice inside it. A reporting consumer -- the `STARTUP` ping,
// the heartbeat -- could then rewrite the managed set of the adoption that
// produced it.
//
// `M-L-SUMMARYALIAS` returns the internal summary directly.
func TestStartupSummaryIsADeepCopy(t *testing.T) {
	src := okSource()
	src.positions = rest.PositionsResult{
		Walk:     completeWalk(),
		ByTicker: map[string]num.Qty{"HELD": num.QtyFromFloat(2)},
	}
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		foreignOrder("theirs", "EXCLUDED"),
	}}
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), "SELECTED")

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	before := at.Adoption.Managed()
	beforeExcluded := at.Adoption.Excluded()
	beforePositions := at.Adoption.Summary().Positions["HELD"]

	sum := at.Adoption.Summary()
	for i := range sum.Managed {
		sum.Managed[i] = "VANDALISED"
	}
	for i := range sum.Excluded {
		sum.Excluded[i] = "VANDALISED"
	}
	for i := range sum.Reducing {
		sum.Reducing[i] = "VANDALISED"
	}
	sum.Positions["HELD"] = num.QtyFromFloat(999)
	sum.Positions["INVENTED"] = num.QtyFromFloat(7)

	if got := at.Adoption.Managed(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the managed set became %v after a consumer mutated a summary "+
			"(was %v)", describe(got), describe(before))
	}
	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got, beforeExcluded) {
		t.Fatalf("the exclusion set became %v (was %v)",
			describe(got), describe(beforeExcluded))
	}
	fresh := at.Adoption.Summary()
	if fresh.Positions["HELD"] != beforePositions {
		t.Fatalf("the summary's position map is shared: HELD is now %s",
			fresh.Positions["HELD"].Wire())
	}
	if _, invented := fresh.Positions["INVENTED"]; invented {
		t.Fatal("a consumer added a market to the summary's position map")
	}
	for i := range fresh.Managed {
		if fresh.Managed[i] == "VANDALISED" {
			t.Fatal("two calls to Summary() share one slice")
		}
	}
}

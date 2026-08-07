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

// TestBearerSecretsNeverAppearInAnyError closes the disclosure path that is not
// the wire.
//
// `http.Client.Do` returns a `*url.Error` whose `Error()` is `Post "<the whole
// URL>": <cause>`, and the cause underneath is usually a `*net.OpError`
// carrying host and port. Returning or wrapping either publishes the ntfy topic
// -- which is the channel's credential -- or the dead-man endpoint, which anyone
// holding can report a dead harness alive indefinitely. Errors are logged; a
// credential in one is a credential in the log.
//
// `M-P-ERRSECRET` wraps the original error.
func TestBearerSecretsNeverAppearInAnyError(t *testing.T) {
	const topic = "sekrit-topic-err-1"
	const token = "checkin-token-err-2"
	ctx := context.Background()

	// (a) a transport failure: the server is gone.
	gone := httptest.NewServer(newRecorder())
	goneURL := gone.URL
	gone.Close()
	sender := senderTo(t, httptest.NewServer(newRecorder()), topic)
	sender.base = goneURL

	err := sender.Send(ctx, Message{Title: "t", Body: "b",
		Priority: PriorityUrgent})
	if err == nil {
		t.Fatal("Send to a closed server reported success")
	}
	assertNoSecret(t, "ntfy transport error", err, topic, goneURL)

	// (b) the dead man, same failure.
	goneTLS := httptest.NewTLSServer(newRecorder())
	endpoint := goneTLS.URL + "/" + token
	transport := goneTLS.Client().Transport
	goneTLS.Close()
	dm, err := NewHTTPSDeadman(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	c := newBearerClient()
	c.Transport = transport
	dm.http = c
	err = dm.CheckIn(ctx)
	if err == nil {
		t.Fatal("CheckIn to a closed server reported success")
	}
	assertNoSecret(t, "dead-man transport error", err, token, endpoint)

	// (c) the redirect refusal, which names neither url.
	thief := httptest.NewServer(newRecorder())
	defer thief.Close()
	redirector := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, thief.URL+r.URL.Path, http.StatusFound)
		}))
	defer redirector.Close()
	s2 := senderTo(t, redirector, topic)
	err = s2.Send(ctx, Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("the redirect was followed")
	}
	assertNoSecret(t, "redirect refusal", err, topic, redirector.URL, thief.URL)

	// (d) a non-2xx, which may name the status and nothing else.
	rejecting := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
	defer rejecting.Close()
	s3 := senderTo(t, rejecting, topic)
	err = s3.Send(ctx, Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("a 403 reported success")
	}
	assertNoSecret(t, "non-2xx error", err, topic, rejecting.URL)

	// (e) a cancelled context still classifies, without naming anything.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s4 := senderTo(t, httptest.NewServer(newRecorder()), topic)
	err = s4.Send(cancelled, Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("a cancelled send reported success")
	}
	assertNoSecret(t, "cancelled send", err, topic)
	if !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("a cancelled send lost its classification: %v", err)
	}
}

// assertNoSecret fails if any forbidden substring appears in the error text.
func assertNoSecret(t *testing.T, what string, err error, secrets ...string) {
	t.Helper()
	msg := err.Error()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(msg, s) {
			t.Fatalf("the %s contains a bearer secret (%q); errors are logged, "+
				"and a credential in an error is a credential in the log:\n%s",
				what, s, msg)
		}
		// The host and port alone identify the endpoint.
		if host := strings.TrimPrefix(strings.TrimPrefix(s, "https://"),
			"http://"); host != "" && strings.Contains(msg, host) {
			t.Fatalf("the %s contains %q:\n%s", what, host, msg)
		}
	}
}

// TestZeroValueSenderIsRejectedWithoutPanic seals the forged transport.
//
// `&ping.NTFYSender{}` compiles from any package -- every field is unexported --
// so the zero value is constructible and must be REFUSED. Dereferencing its nil
// topic panics, and it panics inside the alert loop: the one code path whose job
// is to survive everything else failing.
//
// `M-P-ZEROSENDER` accepts it.
func TestZeroValueSenderIsRejectedWithoutPanic(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a zero NTFYSender PANICKED instead of returning an "+
					"error: %v", r)
			}
		}()
		if err := (&NTFYSender{}).Send(ctx, Message{Body: "b"}); err == nil {
			t.Fatal("a zero NTFYSender reported a delivery")
		}
	}()

	if _, err := NewService(f.store.Reader(), f.store, &NTFYSender{},
		deadmanTo(t, dsrv), time.Hour); err == nil {
		t.Fatal("NewService accepted a forged zero-value sender; a nil check " +
			"refuses only the honest mistake and admits the one that panics")
	}
	if _, err := NewNTFYSender(Topic{}); err == nil {
		t.Fatal("NewNTFYSender accepted a forged Topic")
	}
}

// TestZeroValueDeadmanIsRejectedWithoutPanic is the same seal on §13.4's side.
//
// `M-P-ZERODEAD` accepts it.
func TestZeroValueDeadmanIsRejectedWithoutPanic(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	srv := httptest.NewServer(newRecorder())
	defer srv.Close()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a zero HTTPSDeadman PANICKED instead of returning an "+
					"error: %v", r)
			}
		}()
		if err := (&HTTPSDeadman{}).CheckIn(ctx); err == nil {
			t.Fatal("a zero HTTPSDeadman reported a check-in")
		}
	}()

	if _, err := NewService(f.store.Reader(), f.store,
		senderTo(t, srv, "topic-zerodead"), &HTTPSDeadman{},
		time.Hour); err == nil {
		t.Fatal("NewService accepted a forged zero-value dead man; §13.4's " +
			"only detector of F18 that survives this process dying would be " +
			"silently absent, and the first Step would panic")
	}
}

// TestNextStepSchedulesFailedAndSuppressedAlertRetries is the scheduling
// contract.
//
// `Step` owns no clock, so every deadline it computes is worthless unless it is
// published. A private 1-60 second retry ladder whose caller only knows about
// the hourly heartbeat is a ladder that resolves to an hour, and a §13.2 bucket
// that reopens in fifteen minutes waits for whatever unrelated event happens to
// trigger the next Step.
//
// `M-P-RETRYWAKE` reports only the heartbeat deadline.
func TestNextStepSchedulesFailedAndSuppressedAlertRetries(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()

	ctx := context.Background()
	t0 := int64(1_700_000_000_000)
	svc := f.service(senderTo(t, srv, "topic-next"), deadmanTo(t, dsrv),
		time.Hour)

	// --- a FAILED alert ------------------------------------------------------
	rec.setStatus(http.StatusInternalServerError)
	f.anomaly("n-1", "FOREIGN_FILL", risk.SEV1, "KXTEST-A", t0)
	eff := svc.Step(ctx, t0, heartbeatFor(quote.Running))
	f.settle()

	if eff.NextStepMs <= t0 {
		t.Fatalf("NextStepMs is %d, not in the future of %d", eff.NextStepMs, t0)
	}
	if eff.NextStepMs-t0 > 60_000 {
		t.Fatalf("a SEV1 push failed and the next step is %d ms away; the "+
			"private 1-60 second ladder is unreachable if the only published "+
			"deadline is the hourly heartbeat", eff.NextStepMs-t0)
	}
	if eff.NextHeartbeatMs-t0 != int64(time.Hour/time.Millisecond) {
		t.Fatalf("NextHeartbeatMs moved off the nominal schedule: %d",
			eff.NextHeartbeatMs-t0)
	}

	// --- a SUPPRESSED alert --------------------------------------------------
	rec.setStatus(http.StatusOK)
	t1 := t0 + 120_000
	f.anomaly("n-2", "POSITION_DRIFT", risk.SEV2, "KXTEST-B", t1)
	eff = svc.Step(ctx, t1, heartbeatFor(quote.Running))
	f.settle()
	if got := len(pushesOf(eff, PushAnomaly)); got == 0 {
		t.Fatalf("the first SEV2 was not pushed (%d pushes)", got)
	}

	t2 := t1 + 1_000
	f.anomaly("n-3", "POSITION_DRIFT", risk.SEV2, "KXTEST-B", t2)
	eff = svc.Step(ctx, t2, heartbeatFor(quote.Running))
	f.settle()
	if got := len(pushesOf(eff, PushAnomaly)); got != 0 {
		t.Fatalf("a suppressed SEV2 was pushed (%d)", got)
	}
	reopen := t1 + int64(sev2BucketMs)
	if eff.NextStepMs > reopen {
		t.Fatalf("the §13.2 bucket reopens at %d and the next step is %d; the "+
			"aggregate would wait for whatever unrelated event happens next",
			reopen-t2, eff.NextStepMs-t2)
	}
}

// TestInitiallyUnhealthyStoreAlertsUrgently covers the harness that was broken
// before it started.
//
// A transition notice that requires a prior HEALTHY observation never fires for
// a process that came up with its database locked or its disk full -- which is
// exactly the deployment nobody is watching.
//
// `M-P-INITIALHEALTH` requires the prior observation.
func TestInitiallyUnhealthyStoreAlertsUrgently(t *testing.T) {
	f := newFixture(t)
	release := f.lockDatabase()
	defer release()

	t0 := int64(1_700_000_000_000)
	f.anomalyAsync("u-1", "OWNER_STALLED", risk.SEV1, "", t0)
	f.awaitUnhealthy()

	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()

	// The service is built AFTER the store broke: its first observation is an
	// unhealthy one.
	svc := f.service(senderTo(t, srv, "topic-initial"), deadmanTo(t, dsrv),
		time.Hour)
	eff := svc.Step(context.Background(), t0, heartbeatFor(quote.Running))

	health := pushesOf(eff, PushHealth)
	if len(health) != 1 {
		t.Fatalf("a service whose FIRST observation is an unhealthy store sent "+
			"%d urgent health notices, want one; a harness that starts broken "+
			"would otherwise say nothing about it", len(health))
	}
	if health[0].Priority != PriorityUrgent {
		t.Fatalf("the initial health notice priority is %q, want %q",
			health[0].Priority, PriorityUrgent)
	}
	if !strings.Contains(health[0].Body, "persistence is unavailable") {
		t.Fatalf("the initial health notice does not say what is wrong:\n%s",
			health[0].Body)
	}
	if len(health[0].AnomalyIDs) != 0 {
		t.Fatal("the health notice delivered anomaly rows; it is status " +
			"telemetry, and the record whose journalling failed is precisely " +
			"the one not yet safe to send")
	}
}

// TestFailedStatusPushesRetryBeforeTheHour is the third scheduling hole.
//
// A failed heartbeat that waits for the next hourly slot takes the dead-man
// check-in with it, so the external watchdog alarms on a harness that is alive
// and well. A failed health notice that is never retried means the operator is
// never told that persistence went away. Both retry on the private 1-60 second
// ladder.
//
// `M-P-STATUSRETRY` discards the failure.
func TestFailedStatusPushesRetryBeforeTheHour(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()
	deadRec := newRecorder()
	dsrv := httptest.NewTLSServer(deadRec)
	defer dsrv.Close()

	ctx := context.Background()
	t0 := int64(1_700_000_000_000)
	svc := f.service(senderTo(t, srv, "topic-status"), deadmanTo(t, dsrv),
		time.Hour)

	// --- a failed heartbeat --------------------------------------------------
	rec.setStatus(http.StatusInternalServerError)
	eff := svc.Step(ctx, t0, heartbeatFor(quote.Running))
	beats := pushesOf(eff, PushHeartbeat)
	if len(beats) != 1 || beats[0].Err == nil {
		t.Fatalf("expected one FAILED heartbeat push, got %+v", beats)
	}
	if eff.NextStepMs-t0 > 60_000 {
		t.Fatalf("a failed heartbeat scheduled its retry %d ms out; the ladder "+
			"is 1-60 seconds and an hour is long enough for the watchdog to "+
			"alarm", eff.NextStepMs-t0)
	}

	at := eff.NextStepMs
	rec.setStatus(http.StatusOK)
	eff = svc.Step(ctx, at, heartbeatFor(quote.Running))
	beats = pushesOf(eff, PushHeartbeat)
	if len(beats) != 1 {
		t.Fatalf("the failed heartbeat was not retried before the next hourly "+
			"slot (%d pushes at %d ms after the failure)", len(beats), at-t0)
	}
	if beats[0].Err != nil {
		t.Fatalf("the heartbeat retry failed: %v", beats[0].Err)
	}
	// The NOMINAL schedule did not move: a retry is not a heartbeat.
	if eff.NextHeartbeatMs != t0+int64(time.Hour/time.Millisecond) {
		t.Fatalf("the nominal heartbeat schedule moved to %d; a retry must not "+
			"push the hourly cadence out", eff.NextHeartbeatMs-t0)
	}

	// --- a failed dead-man check-in ------------------------------------------
	deadRec.setStatus(http.StatusInternalServerError)
	at = t0 + int64(time.Hour/time.Millisecond)
	eff = svc.Step(ctx, at, heartbeatFor(quote.Running))
	if eff.Deadman || eff.DeadmanErr == nil {
		t.Fatalf("expected a failed check-in, got deadman=%v err=%v",
			eff.Deadman, eff.DeadmanErr)
	}
	if eff.NextStepMs-at > 60_000 {
		t.Fatalf("a failed dead-man check-in scheduled its retry %d ms out",
			eff.NextStepMs-at)
	}
	deadRec.setStatus(http.StatusOK)
	before := deadRec.count()
	eff = svc.Step(ctx, eff.NextStepMs, heartbeatFor(quote.Running))
	if deadRec.count() == before {
		t.Fatal("a failed dead-man check-in was not retried before the next " +
			"hourly heartbeat; the external watchdog would alarm on a harness " +
			"that is alive and correctly refusing to add risk")
	}
	if !eff.Deadman {
		t.Fatalf("the check-in retry did not succeed: %v", eff.DeadmanErr)
	}

	// --- a failed health notice ----------------------------------------------
	release := f.lockDatabase()
	defer release()
	f.anomalyAsync("s-1", "OWNER_STALLED", risk.SEV1, "", t0)
	f.awaitUnhealthy()

	rec.setStatus(http.StatusInternalServerError)
	at = eff.NextStepMs
	eff = svc.Step(ctx, at, heartbeatFor(quote.Running))
	health := pushesOf(eff, PushHealth)
	if len(health) != 1 || health[0].Err == nil {
		t.Fatalf("expected one FAILED health notice, got %+v", health)
	}
	if eff.NextStepMs-at > 60_000 {
		t.Fatalf("a failed health notice scheduled its retry %d ms out",
			eff.NextStepMs-at)
	}

	rec.setStatus(http.StatusOK)
	at = eff.NextStepMs
	eff = svc.Step(ctx, at, heartbeatFor(quote.Running))
	health = pushesOf(eff, PushHealth)
	if len(health) != 1 {
		t.Fatalf("the failed health notice was not retried (%d notices); the "+
			"operator would never learn that persistence went away",
			len(health))
	}
	if health[0].Err != nil {
		t.Fatalf("the health notice retry failed: %v", health[0].Err)
	}
	// Delivered: it is not sent a third time.
	eff = svc.Step(ctx, at+1_000, heartbeatFor(quote.Running))
	if got := len(pushesOf(eff, PushHealth)); got != 0 {
		t.Fatalf("the health notice was re-sent after succeeding (%d)", got)
	}
}

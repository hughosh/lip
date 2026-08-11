package ping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTopicNeverLeavesDestinationURL is the credential boundary.
//
// The ntfy topic IS the channel's credential: anyone holding it can read every
// alert this harness sends and forge one. Its ONLY permitted occurrence is the
// path of the request that delivers to it. Not the body, not the title, not any
// other header, not the query string -- a notification is forwarded, screenshot
// and left on a lock screen, and each of those is a place the credential would
// then be.
//
// `M-P-TOPIC` appends it to the body, which is the shape this leak actually
// takes: somebody adds the destination to the message for debugging.
func TestTopicNeverLeavesDestinationURL(t *testing.T) {
	const topic = "sekrit-topic-9x"
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()

	sender := senderTo(t, srv, topic)
	err := sender.Send(context.Background(), Message{
		Title:    "SEV1 FOREIGN_FILL KXTEST-A",
		Body:     "someone else is trading the account",
		Priority: PriorityUrgent,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("recorded %d requests, want one", len(reqs))
	}
	got := reqs[0]

	if got.Path != "/"+topic {
		t.Fatalf("destination path %q, want %q", got.Path, "/"+topic)
	}
	if strings.Contains(got.Body, topic) {
		t.Fatalf("the ntfy topic appears in the notification BODY; it is the "+
			"credential for the channel, and a body is forwarded, screenshot "+
			"and shown on a lock screen:\n%s", got.Body)
	}
	if strings.Contains(got.Query, topic) {
		t.Fatalf("the ntfy topic appears in the query string %q", got.Query)
	}
	for name, values := range got.Header {
		for _, v := range values {
			if strings.Contains(v, topic) {
				t.Fatalf("the ntfy topic appears in header %s: %q", name, v)
			}
		}
	}
	if got.Header.Get("Priority") != PriorityUrgent {
		t.Fatalf("Priority header %q", got.Header.Get("Priority"))
	}

	// The guard also refuses a caller who puts it there.
	if err := sender.Send(context.Background(), Message{
		Title: "x", Body: "the topic is " + topic,
	}); err == nil {
		t.Fatal("a message whose body contained the topic was sent")
	}

	// And the type itself offers no way to render it.
	for _, name := range []string{"String", "Format", "MarshalText",
		"MarshalJSON", "GoString", "Reveal", "Value"} {
		if _, ok := typeMethod(Topic{}, name); ok {
			t.Fatalf("Topic has a %s method; providing one invites %%s, and "+
				"the only correct rendering of a bearer credential is not to "+
				"render it", name)
		}
	}
}

// TestBearerTransportsDoNotFollowRedirects covers both live transports.
//
// Each carries a bearer credential IN ITS URL -- the ntfy topic is the path, the
// dead-man check-in is the whole endpoint. Go's default client re-sends a
// redirected request to the new host, so one 302 from a compromised or merely
// misconfigured answer turns a delivery into a credential disclosure, and the
// harness would report the delivery successful.
//
// `M-P-REDIRECT` restores the default policy.
func TestBearerTransportsDoNotFollowRedirects(t *testing.T) {
	// --- ntfy ---------------------------------------------------------------
	thief := newRecorder()
	thiefSrv := httptest.NewServer(thief)
	defer thiefSrv.Close()

	redirector := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, thiefSrv.URL+r.URL.Path, http.StatusFound)
		}))
	defer redirector.Close()

	sender := senderTo(t, redirector, "topic-redirect")
	err := sender.Send(context.Background(), Message{
		Title: "t", Body: "b", Priority: PriorityUrgent})
	if err == nil {
		t.Fatal("the ntfy sender followed a redirect and reported success")
	}
	if thief.count() != 0 {
		t.Fatalf("the redirect target received %d requests carrying the ntfy "+
			"topic in the URL", thief.count())
	}

	// --- dead man -----------------------------------------------------------
	dthief := newRecorder()
	dthiefSrv := httptest.NewTLSServer(dthief)
	defer dthiefSrv.Close()

	dredirect := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, dthiefSrv.URL+r.URL.Path, http.StatusFound)
		}))
	defer dredirect.Close()

	dead := deadmanTo(t, dredirect)
	if err := dead.CheckIn(context.Background()); err == nil {
		t.Fatal("the dead-man transport followed a redirect and reported a " +
			"successful check-in")
	}
	if dthief.count() != 0 {
		t.Fatalf("the redirect target received %d dead-man check-ins",
			dthief.count())
	}
}

// TestAlertServiceRejectsMissingDeadman is §13.4 having no production no-op.
//
// The check-in is the only detector of F18 -- "the harness stopped and nobody
// noticed" -- that survives this process dying. A nil-tolerant constructor
// degrades to no detector at all in exactly the deployment that forgot to
// configure one, and the harness would run unattended believing it was watched.
//
// `M-P-DEADMAN` accepts a nil endpoint.
func TestAlertServiceRejectsMissingDeadman(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(newRecorder())
	defer srv.Close()
	dsrv := httptest.NewTLSServer(newRecorder())
	defer dsrv.Close()
	sender := senderTo(t, srv, "topic-deadman")

	if _, err := NewService(f.store.Reader(), f.store, sender, nil,
		time.Hour); err == nil {
		t.Fatal("NewService accepted a nil dead-man endpoint. §13.4's check-in " +
			"is the only F18 detector that survives this process dying, and " +
			"no sentinel boolean substitutes for proof that a missed check-in " +
			"actually fires")
	}
	// The other collaborators are equally required.
	if _, err := NewService(nil, f.store, sender, deadmanTo(t, dsrv),
		time.Hour); err == nil {
		t.Fatal("NewService accepted a nil reader")
	}
	if _, err := NewService(f.store.Reader(), f.store, nil,
		deadmanTo(t, dsrv), time.Hour); err == nil {
		t.Fatal("NewService accepted a nil sender")
	}
	if _, err := NewService(f.store.Reader(), f.store, sender,
		deadmanTo(t, dsrv), 0); err == nil {
		t.Fatal("NewService accepted a zero heartbeat interval; F18 is " +
			"detected by the ABSENCE of a heartbeat that never comes")
	}
	// And the endpoint itself is validated.
	if _, err := NewHTTPSDeadman(""); err == nil {
		t.Fatal("NewHTTPSDeadman accepted an empty endpoint")
	}
	if _, err := NewHTTPSDeadman("http://example.invalid/checkin"); err == nil {
		t.Fatal("NewHTTPSDeadman accepted a plaintext endpoint; the URL is a " +
			"bearer credential broadcast on every heartbeat")
	}
}

// TestNTFYTopicLoadsOnlyFromAnExplicitAbsoluteEnvFile covers §13's credential
// source rules.
func TestNTFYTopicLoadsOnlyFromAnExplicitAbsoluteEnvFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	if _, err := LoadNTFYTopic("relative/env"); err == nil {
		t.Fatal("a relative env path was accepted; which credential is loaded " +
			"must not depend on the working directory")
	}
	if _, err := LoadNTFYTopic(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("a missing env file was accepted")
	}
	if _, err := LoadNTFYTopic(write("absent", "KALSHI_KEY=x\n")); err == nil {
		t.Fatal("an env file with no NTFY_TOPIC was accepted; §13 has no " +
			"fallback channel, so a harness that cannot address the operator " +
			"must fail to start rather than run silent")
	}
	if _, err := LoadNTFYTopic(write("empty", "NTFY_TOPIC=\n")); err == nil {
		t.Fatal("an empty NTFY_TOPIC was accepted")
	}
	if _, err := LoadNTFYTopic(
		write("dup", "NTFY_TOPIC=one\nNTFY_TOPIC=two\n")); err == nil {
		t.Fatal("a duplicated NTFY_TOPIC was accepted; two values mean the " +
			"file was edited and the old line left behind, and choosing " +
			"between them by shell precedence picks a channel by accident")
	}
	if _, err := LoadNTFYTopic(
		write("slash", "NTFY_TOPIC=a/b\n")); err == nil {
		t.Fatal("a topic containing a slash was accepted; it becomes a URL " +
			"path segment and would silently redirect the alert")
	}

	good, err := LoadNTFYTopic(write("good", "# c\nexport NTFY_TOPIC=\"ok-1\"\n"))
	if err != nil {
		t.Fatalf("a well-formed env file was rejected: %v", err)
	}
	if !good.Valid() {
		t.Fatal("a loaded topic reports itself invalid")
	}
	if (Topic{}).Valid() {
		t.Fatal("the zero Topic is valid")
	}
	if _, err := NewNTFYSender(Topic{}); err == nil {
		t.Fatal("a sender was built from the zero Topic")
	}
}

// TestDeadmanLoadsOnlyFromAnExplicitAbsoluteEnvFile is the production source
// boundary for the second bearer credential.
//
// DEADMAN_URL is the whole check-in endpoint, not merely an address: anyone who
// learns it can suppress the external alarm indefinitely. Loader failures are
// startup errors and are therefore likely to be logged or pasted into an
// incident; no rejected value may appear in them.
func TestDeadmanLoadsOnlyFromAnExplicitAbsoluteEnvFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	if _, err := LoadHTTPSDeadman("relative/env"); err == nil {
		t.Fatal("a relative env path was accepted; which dead-man credential is " +
			"loaded must not depend on the working directory")
	}
	if _, err := LoadHTTPSDeadman(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("a missing env file was accepted")
	}
	if _, err := LoadHTTPSDeadman(
		write("deadman-absent", "NTFY_TOPIC=channel-only\n")); err == nil {
		t.Fatal("an env file with no DEADMAN_URL was accepted; a harness with no " +
			"external observer must fail to start rather than run unattended")
	}
	if _, err := LoadHTTPSDeadman(
		write("deadman-empty", "DEADMAN_URL=\n")); err == nil {
		t.Fatal("an empty DEADMAN_URL was accepted")
	}

	const (
		firstSecret  = "https://first-deadman-secret.example/checkin"
		secondSecret = "https://second-deadman-secret.example/checkin"
	)
	_, err := LoadHTTPSDeadman(write("deadman-duplicate",
		"DEADMAN_URL="+firstSecret+"\nDEADMAN_URL="+secondSecret+"\n"))
	if err == nil {
		t.Fatal("duplicate DEADMAN_URL entries were accepted; choosing either " +
			"one by shell precedence could check in with the wrong observer")
	}
	assertNoSecret(t, "duplicate dead-man loader error", err,
		firstSecret, secondSecret)

	for name, endpoint := range map[string]string{
		"plaintext": "http://plaintext-deadman-secret.example/checkin",
		"malformed": "https://malformed-deadman-secret.example/%zz",
	} {
		_, err := LoadHTTPSDeadman(write("deadman-"+name,
			"export DEADMAN_URL=\""+endpoint+"\"\n"))
		if err == nil {
			t.Fatalf("a %s DEADMAN_URL was accepted", name)
		}
		assertNoSecret(t, name+" dead-man loader error", err, endpoint)
	}

	dead, err := LoadHTTPSDeadman(write("deadman-good",
		"# both credentials may share the explicit file\n"+
			"NTFY_TOPIC=channel-1\n"+
			"export DEADMAN_URL=\"https://watchdog.example/checkin/token-1\"\n"))
	if err != nil {
		t.Fatalf("a well-formed DEADMAN_URL was rejected: %v", err)
	}
	if !dead.valid() {
		t.Fatal("the loaded dead-man transport reports itself invalid")
	}
}

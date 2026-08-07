package wsx

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"lip/feed"
)

// genSigner writes a fresh RSA key and credentials file to a temp dir and
// returns a real `feed.Signer` over them, plus the public key to verify with.
//
// A real signer and a real key, because the thing under test is the HANDSHAKE:
// PSS with a salt length equal to the digest, the exact signed message, and
// the headers reaching the server. A fake signer proves the plumbing and
// nothing about whether the exchange would accept us.
func genSigner(t *testing.T) (*feed.Signer, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "kalshi.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath,
		[]byte("KALSHI_API_KEY_ID=test-key-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := feed.NewSignerFrom(keyPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	return s, &key.PublicKey
}

// verifyHandshake reproduces what Kalshi does with the three headers.
func verifyHandshake(pub *rsa.PublicKey, h http.Header) error {
	keyID := h.Get("KALSHI-ACCESS-KEY")
	ts := h.Get("KALSHI-ACCESS-TIMESTAMP")
	sig := h.Get("KALSHI-ACCESS-SIGNATURE")
	if keyID == "" || ts == "" || sig == "" {
		return errors.New("a handshake header is missing")
	}
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		return errors.New("timestamp is not milliseconds since the epoch")
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return err
	}
	// The signed message is timestamp + METHOD + path, concatenated with no
	// separators, over the websocket path with NO query string.
	digest := sha256.Sum256([]byte(ts + "GET" + feed.WSPath))
	return rsa.VerifyPSS(pub, crypto.SHA256, digest[:], raw, &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	})
}

// TestLiveTransportCompletesASignedHandshake exercises the production dialer
// against a real local websocket server.
//
// Everything below the Socket interface is untested by every other test in this
// package, because every other test substitutes at that seam. This is the one
// that says the seam has a working implementation underneath it: a real
// handshake, real PSS signing with the salt length the exchange requires,
// permessage-deflate negotiated, real frames, and a real ping/pong.
func TestLiveTransportCompletesASignedHandshake(t *testing.T) {
	signer, pub := genSigner(t)

	gotHeaders := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotHeaders <- r.Header.Clone()
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
				CompressionMode: websocket.CompressionContextTakeover,
			})
			if err != nil {
				return
			}
			defer c.CloseNow()
			ctx := r.Context()
			// Echo whatever the client subscribes with, then answer a ping
			// from inside the read loop the way a real server does.
			for {
				typ, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				if err := c.Write(ctx, typ, b); err != nil {
					return
				}
			}
		}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nowMs := time.Now().UnixMilli()
	hdr, err := signer.WSHeaders(nowMs)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}

	sock, err := NewLiveDialer().Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), h)
	if err != nil {
		t.Fatalf("the production dialer could not complete a real handshake: %v", err)
	}
	defer sock.Close()

	select {
	case got := <-gotHeaders:
		if err := verifyHandshake(pub, got); err != nil {
			t.Fatalf("the server could not verify our handshake: %v", err)
		}
		if got.Get("KALSHI-ACCESS-TIMESTAMP") != strconv.FormatInt(nowMs, 10) {
			t.Fatal("the signed timestamp is not the one we passed in; the " +
				"signer must not read a clock of its own")
		}
		if !strings.Contains(got.Get("Sec-WebSocket-Extensions"),
			"permessage-deflate") {
			t.Fatal("permessage-deflate was not offered; the shadow rig " +
				"negotiates it by default, so omitting it changes both the " +
				"handshake and delivery timing relative to what we are " +
				"measured against")
		}
	case <-ctx.Done():
		t.Fatal("the server never saw a handshake")
	}

	sub, err := subscribeDelta([]string{fxTicker})
	if err != nil {
		t.Fatal(err)
	}
	if err := sock.Write(ctx, sub); err != nil {
		t.Fatal(err)
	}
	echo, err := sock.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(echo) != string(sub) {
		t.Fatalf("round trip: got %s, want %s", echo, sub)
	}

	// A pong is delivered through the READ path, so Ping only completes while
	// something is reading concurrently. That is not a quirk of the test: it is
	// why the session runs its reader on its own goroutine and waits for the
	// pong on a channel, and a session that pinged inline would deadlock
	// against exactly this.
	readDone := make(chan error, 1)
	go func() {
		_, err := sock.Read(ctx)
		readDone <- err
	}()
	if err := sock.Ping(ctx); err != nil {
		t.Fatalf("ping over a real socket: %v", err)
	}
	sock.Close()
	<-readDone
}

// TestLiveTransportRefusesRedirects is the harness's own defect, found while
// testing `rest` and repaired here before it could be repeated.
//
// A 307 preserves method and body, so a redirected request is a SECOND request
// to a host our signature does not cover. Over REST that meant a redirected
// create was a second create to an unsigned host. Over a websocket the
// consequence is worse: a signed handshake completing against an unsigned host,
// and then a book we quote against.
func TestLiveTransportRefusesRedirects(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			reached = true
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			c.CloseNow()
		}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
	defer redirector.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := NewLiveDialer().Dial(ctx,
		"ws"+strings.TrimPrefix(redirector.URL, "http"), http.Header{})
	if err == nil {
		t.Fatal("the dialer followed a redirect; the handshake is signed for " +
			"one host and a redirect completes it against another")
	}
	if reached {
		t.Fatal("the redirect target received our signed handshake")
	}
}

// TestLiveTransportClassifiesAnsweredRejections keeps the two failure classes
// apart.
//
// A server that ANSWERS and is refused -- a 401 on a bad signature -- is not a
// network failure, and retrying it every second for a week is the worst
// available response to it. The classification does not change the reconnect
// policy in this package (the supervisor never gives up), but it is what makes
// the cause legible in the anomaly an operator reads at 3am.
func TestLiveTransportClassifiesAnsweredRejections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
		}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := NewLiveDialer().Dial(ctx,
		"ws"+strings.TrimPrefix(srv.URL, "http"), http.Header{})
	if err == nil {
		t.Fatal("a 401 handshake succeeded")
	}
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("a rejected handshake was not classified as one: %T %v",
			err, err)
	}
	if he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", he.StatusCode)
	}
}

// TestSystemClockReadsBothClocksTogether covers the production Clock.
//
// Mono is monotonic and is the only thing compared for age; WallMs is what the
// signature is computed over. Substituting either for the other is a live
// defect -- an NTP correction read as staleness would age out every portfolio
// endpoint at once and stop all placement, including the reducer's.
func TestSystemClockReadsBothClocksTogether(t *testing.T) {
	c := NewSystemClock()
	a := c.Now()
	if a.WallMs <= 0 {
		t.Fatalf("wall clock = %d", a.WallMs)
	}
	if a.Mono < 0 {
		t.Fatalf("monotonic reading is negative: %v", a.Mono)
	}
	timer := c.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(2 * time.Second):
		t.Fatal("the system timer never fired")
	}
	b := c.Now()
	if b.Mono < a.Mono {
		t.Fatal("the monotonic reading went backwards")
	}
}

// TestCleanCloseClassificationCoversBothNormalStatuses pins the one thing
// `clean` is allowed to decide.
func TestCleanCloseClassificationCoversBothNormalStatuses(t *testing.T) {
	if !IsCleanClose(ErrCleanClose) {
		t.Fatal("ErrCleanClose is not a clean close")
	}
	if IsCleanClose(errors.New("broken pipe")) {
		t.Fatal("a transport error was classified as a clean close")
	}
	if IsCleanClose(nil) {
		t.Fatal("a nil error was classified as a clean close")
	}
}

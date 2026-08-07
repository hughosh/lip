package rest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lip/feed"
)

// ---------------------------------------------------------------------------
// F7 — the real transport, which had no test at all
// ---------------------------------------------------------------------------
//
// Every other test in this package substitutes `scriptedDoer`, so nothing
// reached `HTTPDoer`. That left the production transport outside the test
// surface entirely: forcing `Response{Status: http.StatusOK}` for every
// response was a compiling mutation that survived all 68 tests, and in
// production it turns the H-ORD-2b recovery 409 into a fake 200.

const testKeyID = "test-key-id"

// newTestSigner builds a real feed.Signer over a freshly generated key, so the
// signature this package produces is verified rather than assumed.
func newTestSigner(t *testing.T) (*feed.Signer, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "kalshi.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath,
		[]byte("KALSHI_API_KEY_ID="+testKeyID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := feed.NewSignerFrom(keyPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	return s, &key.PublicKey
}

// verifySig checks a signature over the exact message auth.py signs:
// timestamp + METHOD + path, concatenated with no separators.
func verifySig(pub *rsa.PublicKey, ts, method, path, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(ts + method + path))
	return rsa.VerifyPSS(pub, crypto.SHA256, digest[:], sig, &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	})
}

// The signature covers the prefixed path WITHOUT the query string, and the
// query still reaches the server. Signing the query yields a 401 that looks
// exactly like a credential problem, which is an expensive thing to debug at
// 3am against a live account.
func TestHTTPDoerSignsThePathWithoutTheQuery(t *testing.T) {
	signer, pub := newTestSigner(t)

	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Clone(r.Context())
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"cursor":"","orders":[]}`))
		}))
	defer srv.Close()

	d := NewHTTPDoer(signer, 5*time.Second)
	d.Host = srv.URL
	d.Now = func() time.Time { return time.UnixMilli(1_700_000_000_123) }

	q := url.Values{"status": {StatusResting}, "limit": {"1000"}}
	if _, err := d.Do(context.Background(), Request{
		Method: "GET", Path: "/portfolio/orders", Query: q,
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("the server never saw a request")
	}

	// The query must be on the wire.
	if got.URL.Query().Get("status") != StatusResting ||
		got.URL.Query().Get("limit") != "1000" {
		t.Fatalf("query did not reach the server: %q", got.URL.RawQuery)
	}
	if got.URL.Path != APIPrefix+"/portfolio/orders" {
		t.Fatalf("path %q, want %q", got.URL.Path, APIPrefix+"/portfolio/orders")
	}

	ts := got.Header.Get("KALSHI-ACCESS-TIMESTAMP")
	sig := got.Header.Get("KALSHI-ACCESS-SIGNATURE")
	if got.Header.Get("KALSHI-ACCESS-KEY") != testKeyID {
		t.Fatalf("key id header %q", got.Header.Get("KALSHI-ACCESS-KEY"))
	}
	if ts != "1700000000123" {
		t.Fatalf("timestamp %q — the injected clock was not used", ts)
	}

	// Signed over the path with NO query.
	if err := verifySig(pub, ts, "GET", APIPrefix+"/portfolio/orders", sig); err != nil {
		t.Fatalf("signature does not verify over the query-excluded path: %v", err)
	}
	// And NOT over the path with the query, which is the mistake being pinned.
	if err := verifySig(pub, ts, "GET",
		APIPrefix+"/portfolio/orders?"+q.Encode(), sig); err == nil {
		t.Fatal("the signature also verifies with the query included; the " +
			"signed message must be the path alone")
	}
}

// Mutation B's permanent home. The transport reports what the exchange said,
// never a normalised or assumed status.
func TestHTTPDoerPassesEveryStatusThroughUnchanged(t *testing.T) {
	signer, _ := newTestSigner(t)

	for _, status := range []int{200, 201, 400, 401, 404, 409, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"code":"order_already_exists"}}`))
				}))
			defer srv.Close()

			d := NewHTTPDoer(signer, 5*time.Second)
			d.Host = srv.URL
			resp, err := d.Do(context.Background(), Request{
				Method: "POST", Path: "/portfolio/events/orders",
				Body: []byte(`{}`),
			})
			if err != nil {
				t.Fatalf("a status is an answer, not an error: %v", err)
			}
			if resp.Status != status {
				t.Fatalf("transport reported HTTP %d for a %d response; "+
					"normalising a status turns the H-ORD-2b recovery 409 "+
					"into a fake 200 and erases a live order",
					resp.Status, status)
			}
			if len(resp.Body) == 0 {
				t.Fatal("the body must be passed through so error.code can be read")
			}
		})
	}
}

// A create is a write. Following a redirect on a POST can re-send the body to
// another host, and a 307/308 preserves the method — so a redirected create is
// a second create. `net/http` follows redirects by default, so this has to be
// turned off explicitly.
func TestHTTPDoerDoesNotFollowRedirects(t *testing.T) {
	signer, _ := newTestSigner(t)

	secondHit := false
	second := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			secondHit = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, second.URL+"/trade-api/v2/portfolio/events/orders", 307)
		}))
	defer first.Close()

	d := NewHTTPDoer(signer, 5*time.Second)
	d.Host = first.URL
	resp, err := d.Do(context.Background(), Request{
		Method: "POST", Path: "/portfolio/events/orders", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("a redirect is an answer: %v", err)
	}
	if secondHit {
		t.Fatal("the transport followed a 307 on a POST; a 307 preserves the " +
			"method, so the redirected request is a SECOND create — and it " +
			"would be sent to a host we did not sign for")
	}
	if resp.Status != 307 {
		t.Fatalf("want the 307 surfaced as a response, got %d", resp.Status)
	}
}

// The body and content type reach the server intact.
func TestHTTPDoerSendsTheBody(t *testing.T) {
	signer, _ := newTestSigner(t)

	var body []byte
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ = io.ReadAll(r.Body)
			ctype = r.Header.Get("Content-Type")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		}))
	defer srv.Close()

	d := NewHTTPDoer(signer, 5*time.Second)
	d.Host = srv.URL
	want := `{"ticker":"T1"}`
	if _, err := d.Do(context.Background(), Request{
		Method: "POST", Path: "/portfolio/events/orders", Body: []byte(want),
	}); err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("body on the wire was %q, want %q", body, want)
	}
	if ctype != "application/json" {
		t.Fatalf("content type %q", ctype)
	}
}

// A transport failure is ambiguous and must NOT be reported as NotSent: the
// exchange may have acted before we lost the answer.
func TestHTTPDoerNetworkFailureIsAmbiguous(t *testing.T) {
	signer, _ := newTestSigner(t)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing is listening now

	d := NewHTTPDoer(signer, 2*time.Second)
	d.Host = addr
	_, err := d.Do(context.Background(), Request{
		Method: "POST", Path: "/portfolio/events/orders", Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("want an error against a closed listener")
	}
	if !WasSent(err) {
		t.Fatal("a connection failure must stay ambiguous: the request left " +
			"this process and the exchange may have acted on it")
	}
}

// A request that cannot be signed provably never left the process.
func TestHTTPDoerUnsignableRequestIsNotSent(t *testing.T) {
	d := &HTTPDoer{
		Client: http.DefaultClient,
		Signer: nil, // no credentials
		Host:   "http://127.0.0.1:1",
		Now:    func() time.Time { return time.UnixMilli(1) },
	}
	_, err := d.Do(context.Background(), Request{
		Method: "POST", Path: "/portfolio/events/orders", Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("want an error with no signer")
	}
	if WasSent(err) {
		t.Fatal("an unsigned request never reached the exchange and must be " +
			"reported as NotSent, so a create can be definitely rejected " +
			"instead of pinning capital as UNKNOWN")
	}
}

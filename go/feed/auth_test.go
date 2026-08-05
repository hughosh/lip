package feed

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The real credentials at ~/.kalshi are never read here. Every test below
// generates its own throwaway key.

func testSigner(t *testing.T, pemBlock []byte, env string) (*Signer, error) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k.pem")
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(keyPath, pemBlock, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewSignerFrom(keyPath, envPath)
}

func genPKCS1(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

// The signed message is ts + METHOD + path concatenated with no separators, and
// the salt length equals the digest length — cryptography's PSS.DIGEST_LENGTH.
//
// Go's default is PSSSaltLengthAuto, which for a 2048-bit key salts to the
// maximum instead. VerifyPSS with PSSSaltLengthEqualsHash enforces the exact
// length, so this test fails if the default is ever restored.
func TestSignerProducesAVerifiablePSSSignature(t *testing.T) {
	key, block := genPKCS1(t)
	s, err := testSigner(t, block, "KALSHI_API_KEY_ID=abc-123\n")
	if err != nil {
		t.Fatal(err)
	}

	h, err := s.WSHeaders(1784934732641)
	if err != nil {
		t.Fatal(err)
	}
	if h["KALSHI-ACCESS-KEY"] != "abc-123" {
		t.Errorf("key id = %q, want abc-123", h["KALSHI-ACCESS-KEY"])
	}
	if h["KALSHI-ACCESS-TIMESTAMP"] != "1784934732641" {
		t.Errorf("timestamp = %q, want 1784934732641", h["KALSHI-ACCESS-TIMESTAMP"])
	}

	sig, err := base64.StdEncoding.DecodeString(h["KALSHI-ACCESS-SIGNATURE"])
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	digest := sha256.Sum256([]byte("1784934732641" + "GET" + WSPath))
	if err := rsa.VerifyPSS(&key.PublicKey, crypto.SHA256, digest[:], sig,
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}); err != nil {
		t.Fatalf("signature does not verify over ts+GET+%s with a digest-length salt: %v",
			WSPath, err)
	}

	// A different message must not verify, or the test above proves nothing.
	other := sha256.Sum256([]byte("1784934732641" + "POST" + WSPath))
	if err := rsa.VerifyPSS(&key.PublicKey, crypto.SHA256, other[:], sig,
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}); err == nil {
		t.Error("a signature over GET verified against POST; the message is not being signed")
	}
}

func TestSignerUppercasesTheMethod(t *testing.T) {
	key, block := genPKCS1(t)
	s, err := testSigner(t, block, "KALSHI_API_KEY_ID=x\n")
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Headers(42, "get", "/trade-api/v2/portfolio/balance")
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.StdEncoding.DecodeString(h["KALSHI-ACCESS-SIGNATURE"])
	digest := sha256.Sum256([]byte("42GET/trade-api/v2/portfolio/balance"))
	if err := rsa.VerifyPSS(&key.PublicKey, crypto.SHA256, digest[:], sig,
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}); err != nil {
		t.Fatalf("method was not uppercased before signing: %v", err)
	}
}

func TestSignerAcceptsPKCS8(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := testSigner(t, block, "KALSHI_API_KEY_ID=x\n"); err != nil {
		t.Fatalf("PKCS#8 key rejected: %v. Python's load_pem_private_key takes "+
			"either encoding and the file on disk is not inspected here", err)
	}
}

// The env parser is auth.py's _read_env: strip, skip blanks and #, split on the
// FIRST '=', then strip surrounding quotes.
func TestEnvParserMatchesPython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "" +
		"# a comment\n" +
		"\n" +
		"  KALSHI_API_KEY_ID = 'quoted-id'  \n" +
		"OTHER=\"double\"\n" +
		"EQUALS=a=b=c\n" +
		"NOEQUALS\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"KALSHI_API_KEY_ID": "quoted-id",
		"OTHER":             "double",
		"EQUALS":            "a=b=c",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["NOEQUALS"]; ok {
		t.Error("a line with no '=' produced a key")
	}
	if _, ok := env["# a comment"]; ok {
		t.Error("a comment produced a key")
	}
}

func TestSignerRejectsAMissingKeyID(t *testing.T) {
	_, block := genPKCS1(t)
	if _, err := testSigner(t, block, "SOMETHING_ELSE=1\n"); err == nil {
		t.Error("a missing KALSHI_API_KEY_ID was accepted")
	}
}

// Python's str.splitlines() breaks on far more than LF. A credentials file with
// CR line endings would otherwise hand Go a key id with the next assignment
// glued onto it, and the rig would never connect.
//
//	>>> "A=1\rB=2\nC=3\r\nD=4\x1cE=5".splitlines()
//	['A=1', 'B=2', 'C=3', 'D=4', 'E=5']
func TestPySplitlinesMatchesPython(t *testing.T) {
	got := pySplitlines("A=1\rB=2\nC=3\r\nD=4\x1cE=5")
	want := []string{"A=1", "B=2", "C=3", "D=4", "E=5"}
	if len(got) != len(want) {
		t.Fatalf("pySplitlines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pySplitlines = %q, want %q", got, want)
		}
	}
	// Discriminating power: a plain LF split must disagree.
	if len(strings.Split("A=1\rB=2\nC=3\r\nD=4\x1cE=5", "\n")) == len(want) {
		t.Fatal("strings.Split on LF agrees; the fixture cannot detect the hazard")
	}
}

// A credentials file with CR endings must still parse.
func TestEnvParserHandlesCRLineEndings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("KALSHI_API_KEY_ID=abc\rOTHER=2\r"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if env["KALSHI_API_KEY_ID"] != "abc" {
		t.Errorf("key id = %q, want \"abc\" (CR-separated file)", env["KALSHI_API_KEY_ID"])
	}
}

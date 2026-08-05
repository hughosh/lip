// Package feed is the live half of the rig: the Kalshi websocket client, its
// RSA-PSS handshake, and the REST call that resolves the market universe.
//
// Nothing here decides what a row contains. Frames go to core.Rig unmodified,
// which is what keeps the whole measurement path inside the differentially
// tested surface — this package only gets bytes from the exchange to it.
//
// Credentials are referenced BY PATH and never logged. The private key is loaded
// into a crypto/rsa key and nothing in this package prints, serialises or
// transmits it.
//
//	~/.kalshi/kalshi.pem  RSA private key (mode 600)
//	~/.kalshi/env         KALSHI_API_KEY_ID=...
package feed

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	RestHost = "https://api.elections.kalshi.com"
	WSURL    = "wss://external-api-ws.kalshi.com/trade-api/ws/v2"
	WSPath   = "/trade-api/ws/v2"
)

// Signer signs Kalshi requests. One per process is plenty. Port of auth.py.
type Signer struct {
	keyID string
	key   *rsa.PrivateKey
}

// NewSigner loads the credentials from the default paths.
func NewSigner() (*Signer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewSignerFrom(
		filepath.Join(home, ".kalshi", "kalshi.pem"),
		filepath.Join(home, ".kalshi", "env"),
	)
}

func NewSignerFrom(keyPath, envPath string) (*Signer, error) {
	env, err := readEnv(envPath)
	if err != nil {
		return nil, err
	}
	keyID, ok := env["KALSHI_API_KEY_ID"]
	if !ok {
		return nil, fmt.Errorf("KALSHI_API_KEY_ID not in %s", envPath)
	}
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	key, err := parsePrivateKey(pemBytes)
	if err != nil {
		// The error is deliberately not wrapped with anything derived from the
		// file contents.
		return nil, fmt.Errorf("parse %s: %w", keyPath, err)
	}
	return &Signer{keyID: keyID, key: key}, nil
}

// pySplitlines reproduces Python's str.splitlines() boundary set.
//
// strings.Split(s, "\n") is not equivalent: Python also breaks on a lone CR,
// on CRLF as ONE break, and on \v \f \x1c \x1d \x1e U+0085 U+2028 U+2029.
func pySplitlines(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		w := 1
		switch rs[i] {
		case '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				w = 2 // CRLF is a single boundary
			}
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		default:
			continue
		}
		out = append(out, string(rs[start:i]))
		i += w - 1
		start = i + 1
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// readEnv mirrors auth.py's _read_env: strip each line, skip blanks and
// comments, split on the FIRST '=', then strip surrounding quotes from the
// value.
func readEnv(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	// Python's str.splitlines() breaks on more than LF: CR alone, CRLF, and the
	// Unicode line boundaries \v \f \x1c \x1d \x1e    . A credentials
	// file with CR line endings would otherwise give Go a key id with the next
	// assignment glued to it, and no connection at all.
	for _, line := range pySplitlines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "'\"")
	}
	return out, nil
}

// parsePrivateKey accepts both PEM encodings an RSA key is normally emitted in.
// Python's load_pem_private_key handles either and the file on disk is not
// inspected here, so both are supported rather than guessed at.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not a PKCS#1 or PKCS#8 RSA key")
	}
	k, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PKCS#8 key is %T, not RSA", any)
	}
	return k, nil
}

// Headers returns the auth headers for `method path`, where path is the full API
// path with no query string. auth.py:47-66.
//
// The signed message is the concatenation timestamp + METHOD + path with no
// separators. The padding is PSS with MGF1-SHA256 and a salt length EQUAL TO THE
// DIGEST — cryptography's PSS.DIGEST_LENGTH, which is rsa.PSSSaltLengthEqualsHash
// here. Go's default is PSSSaltLengthAuto, which for a 2048-bit key salts to the
// maximum instead and produces signatures Kalshi rejects.
//
// nowMs is passed in rather than read here so the caller owns the clock.
func (s *Signer) Headers(nowMs int64, method, path string) (map[string]string, error) {
	ts := fmt.Sprintf("%d", nowMs)
	digest := sha256.Sum256([]byte(ts + strings.ToUpper(method) + path))
	sig, err := rsa.SignPSS(rand.Reader, s.key, crypto.SHA256, digest[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"KALSHI-ACCESS-KEY":       s.keyID,
		"KALSHI-ACCESS-TIMESTAMP": ts,
		"KALSHI-ACCESS-SIGNATURE": base64.StdEncoding.EncodeToString(sig),
	}, nil
}

func (s *Signer) WSHeaders(nowMs int64) (map[string]string, error) {
	return s.Headers(nowMs, "GET", WSPath)
}

// confidence: high

package tape

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func gunzip(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := gz.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// --- P26: the tape line format ---------------------------------------------

func TestP26_FrameIsInterpolatedVerbatimAndStripped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.jsonl.gz")
	w, err := NewWriter(path, map[string]float64{"AAA": 1000.0}, []string{"AAA"})
	if err != nil {
		t.Fatal(err)
	}

	// Kalshi frames carry a TRAILING NEWLINE. Without the trim the envelope's
	// closing brace lands on its own line and neither line is valid JSON — the
	// bug the archive tapes still carry.
	//
	// The key order below is deliberately NOT alphabetical: a writer that
	// re-encodes through a JSON marshaller would reorder it, and the tape would
	// stop being the bytes the exchange sent.
	frame := "{\"type\":\"trade\",\"seq\":7,\"msg\":{\"b\":2,\"a\":1}}\n"
	if err := w.Frame(1784934732641, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSuffix(gunzip(t, path), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (header + frame): %q", len(lines), lines)
	}

	var hdr struct {
		Universe map[string]float64 `json:"universe"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	if hdr.Universe["AAA"] != 1000.0 {
		t.Errorf("header universe = %v, want AAA:1000", hdr.Universe)
	}

	want := `{"recv_ms":1784934732641,"m":{"type":"trade","seq":7,"msg":{"b":2,"a":1}}}`
	if lines[1] != want {
		t.Errorf("frame line mismatch (P26)\n got: %s\nwant: %s", lines[1], want)
	}
}

// The writer's output must be readable by this package's own reader, including
// the header, or the differential gate has no input.
func TestP26_RoundTripsThroughFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.jsonl.gz")
	w, err := NewWriter(path, map[string]float64{"AAA": 300.0, "BBB": 1000.0}, []string{"AAA", "BBB"})
	if err != nil {
		t.Fatal(err)
	}
	sent := []string{
		`{"type":"orderbook_snapshot","msg":{"market_ticker":"AAA"}}`,
		`{"type":"trade","msg":{"trade_id":"t1"}}`,
	}
	for i, f := range sent {
		if err := w.Frame(int64(1000+i), []byte(f+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var got []string
	n, truncated, err := Frames(path, func(env Envelope) error {
		if len(env.M) > 0 {
			got = append(got, string(env.M))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a cleanly closed tape reported truncated; Close must flush the gzip trailer")
	}
	if n != 3 {
		t.Errorf("read %d envelopes, want 3 (header + 2 frames)", n)
	}
	for i := range sent {
		if i >= len(got) || got[i] != sent[i] {
			t.Errorf("frame %d round-tripped as %q, want %q", i, got, sent[i])
		}
	}

	uni, exact, err := ScanUniverse([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if !exact {
		t.Error("ScanUniverse did not find the header; targets would default to 0 " +
			"and Qualifies() would short-circuit before the walk")
	}
	if uni["AAA"] != 300.0 || uni["BBB"] != 1000.0 {
		t.Errorf("universe = %v, want AAA:300 BBB:1000", uni)
	}
}

// --- P26: the universe header is Python's json.dumps, byte for byte ---------
//
// Expected values were captured from CPython directly:
//
//	json.dumps({"universe": {"AAA": 1000.0}})  ->  {"universe": {"AAA": 1000.0}}
//
// Go's encoder would emit compact JSON with sorted keys and "1000". Nothing
// downstream can see the difference — both decode to the same map of the same
// float64s — but the tape's entire purpose is to be a faithful record.
func TestP26_HeaderMatchesPythonJSONDumps(t *testing.T) {
	cases := []struct {
		universe map[string]float64
		order    []string
		want     string
	}{
		{map[string]float64{"AAA": 1000.0}, []string{"AAA"},
			`{"universe": {"AAA": 1000.0}}`},
		// Insertion order, NOT sorted: Python iterates the dict.
		{map[string]float64{"B": 300.0, "A": 1000.0}, []string{"B", "A"},
			`{"universe": {"B": 300.0, "A": 1000.0}}`},
		// repr() float forms: a bare 0.5, an exponent, and a non-integral value.
		{map[string]float64{"X": 0.5, "Y": 1e21, "Z": 12.25}, []string{"X", "Y", "Z"},
			`{"universe": {"X": 0.5, "Y": 1e+21, "Z": 12.25}}`},
		{map[string]float64{}, []string{}, `{"universe": {}}`},
	}
	for _, c := range cases {
		got := string(pythonHeader(c.universe, c.order))
		if got != c.want+"\n" {
			t.Errorf("header mismatch\n got: %q\nwant: %q", got, c.want+"\n")
		}
	}
}

// pyFloat must match CPython repr(), which json.dumps uses for floats. Go's 'g'
// verb breaks to exponent form at exponent 6 and CPython's repr does not break
// until 1e16 — so 1000000.0 is the case that separates them. Values captured
// from CPython directly.
func TestP26_PyFloatMatchesCPythonRepr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1000.0, "1000.0"},
		{300.0, "300.0"},
		{0.5, "0.5"},
		{12.25, "12.25"},
		{3.0, "3.0"},
		{123456789.0, "123456789.0"},
		{1000000.0, "1000000.0"},     // Go 'g' would give 1e+06
		{1e15, "1000000000000000.0"}, // still positional in CPython
		{1e16, "1e+16"},              // the break point
		{1e21, "1e+21"},
		{0.0001, "0.0001"}, // still positional
		{1e-05, "1e-05"},   // the low break point
		{1.5e-09, "1.5e-09"},
		// NOT the literal -0.0: Go evaluates untyped constant expressions at
		// arbitrary precision and its constant system has no signed zero, so
		// `-0.0` folds to +0.0 and the case would silently test nothing. P23.
		{math.Copysign(0, -1), "-0.0"},
	}
	discriminating := 0
	for _, c := range cases {
		if got := pyFloat(c.in); got != c.want {
			t.Errorf("pyFloat(%v) = %q, want %q (CPython repr)", c.in, got, c.want)
		}
		if g := strconv.FormatFloat(c.in, 'g', -1, 64); g != c.want &&
			g+".0" != c.want {
			discriminating++
		}
	}
	if discriminating == 0 {
		t.Fatal("no case distinguishes CPython repr from Go's 'g' verb")
	}
	t.Logf("%d of %d cases expose the 'g'-verb hazard", discriminating, len(cases))
}

// json.dumps spells the non-finite values this way. Not legal JSON, but it is
// what Python emits by default.
func TestP26_PyFloatNonFinite(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.NaN(), "NaN"},
	} {
		if got := pyFloat(c.in); got != c.want {
			t.Errorf("pyFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// pyString must match json.dumps at its defaults. encoding/json differs in BOTH
// directions: Go HTML-escapes < > &, which Python does not, and Python defaults
// to ensure_ascii=True and escapes every non-ASCII rune, which Go does not.
// Values captured from CPython directly.
func TestP26_PyStringMatchesJSONDumps(t *testing.T) {
	cases := []struct{ in, want string }{
		{"PLAIN-123", `"PLAIN-123"`},
		{"a<b>c&d", `"a<b>c&d"`}, // encoding/json would emit < > &
		{"quote\"back\\slash", `"quote\"back\\slash"`},
		{"tab\ttext", `"tab\ttext"`},
		{"caf\u00e9", `"caf\u00e9"`},     // ensure_ascii=True
		{"\U0001F600", `"\ud83d\ude00"`}, // astral -> surrogate pair
		{"ctrl\x01", `"ctrl\u0001"`},     // control character
	}
	for _, c := range cases {
		if got := pyString(c.in); got != c.want {
			t.Errorf("pyString(%q) = %s, want %s (json.dumps)", c.in, got, c.want)
		}
	}
	// Discriminating power against the obvious wrong answer.
	if b, _ := json.Marshal("a<b>c&d"); string(b) == pyString("a<b>c&d") {
		t.Fatal("encoding/json agrees with pyString on <>&; the case cannot " +
			"detect Go's HTML escaping")
	}
}

// pyStrip is Python's str.strip(), which is NOT bytes.TrimSpace. Python's
// whitespace set includes the four separators U+001C-U+001F; Go's stops at
// \t \n \v \f \r and space.
//
//	>>> '  {"a":1}\x1c\x1f \n '.strip()
//	'{"a":1}'
//
// This test exists because gate 7 mutation m25 SURVIVED: pyStrip was added in
// response to a review finding and shipped with nothing defending it. The
// mutation reverted the character set to TrimSpace's and no gate objected. A
// rule with no test is not a rule.
func TestP26_PyStripMatchesPythonStrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  {\"a\":1}\x1c\x1f \n ", `{"a":1}`},
		{"{\"a\":1}\n", `{"a":1}`},
		{"\x1c\x1d\x1e\x1f", ""},
		{"{\"a\":1}", `{"a":1}`},
		{"\t\v\f\r {\"a\":1} \r\f\v\t", `{"a":1}`},
	}
	separatorCase := false
	for _, c := range cases {
		if got := string(pyStrip([]byte(c.in))); got != c.want {
			t.Errorf("pyStrip(%q) = %q, want %q (Python str.strip)", c.in, got, c.want)
		}
		if string(bytes.TrimSpace([]byte(c.in))) != c.want {
			separatorCase = true
		}
	}
	// Discriminating power: without a case containing U+001C-U+001F this test
	// would pass against bytes.TrimSpace and defend nothing — which is precisely
	// how m25 survived.
	if !separatorCase {
		t.Fatal("no case distinguishes Python str.strip from bytes.TrimSpace")
	}
}

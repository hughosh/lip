package tape

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Writer captures every received frame. Port of rig.py:281-287 and 511-519.
//
// Without the tape none of the rig's attribution decisions can be revisited,
// and the port itself cannot be validated — the differential gate replays one
// tape through both implementations, so the capture is not a diagnostic extra,
// it is the oracle's input.
//
// compresslevel 6, not gzip's default 9: at the observed feed rate level 9 costs
// 0.61% of a core and level 6 costs 0.22% for 8% more disk. That measurement
// made compression the single largest CPU line item in the Python rig's own
// code — larger than JSON decoding and the whole measurement path combined.
type Writer struct {
	fh *os.File
	gz *gzip.Writer
}

// NewWriter opens `path` for append and writes the universe header.
//
// Target Size is a REST field carried on no websocket frame, so a tape without
// the header cannot reproduce the `reference` gate: every target reads back as
// 0.0 and Qualifies() short-circuits to 0 before the walk. A tape captured
// without it is still a valid differential fixture, but it exercises strictly
// less of the code.
// `order` is the tickers in REST response order. Python's header is
// json.dumps over a dict, so it carries insertion order, ", " / ": " separators
// and repr-formatted floats ("1000.0"); Go's encoder would emit compact JSON
// with sorted keys and "1000". Nothing downstream can see the difference —
// both decode to the same map of the same float64s — but the tape is the one
// artifact whose whole purpose is being a faithful record, so it is written the
// way Python writes it. If `order` is nil the map's keys are used, sorted, so
// the output is at least deterministic.
func NewWriter(path string, universe map[string]float64, order []string) (*Writer, error) {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewWriterLevel(fh, 6)
	if err != nil {
		fh.Close()
		return nil, err
	}
	w := &Writer{fh: fh, gz: gz}

	if _, err := w.gz.Write(pythonHeader(universe, order)); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// pythonHeader reproduces `json.dumps({"universe": tickers})` byte for byte.
func pythonHeader(universe map[string]float64, order []string) []byte {
	if order == nil {
		order = make([]string, 0, len(universe))
		for t := range universe {
			order = append(order, t)
		}
		sort.Strings(order)
	}
	var b bytes.Buffer
	b.WriteString(`{"universe": {`)
	first := true
	seen := make(map[string]bool, len(order))
	for _, t := range order {
		if seen[t] {
			continue
		}
		seen[t] = true
		v, ok := universe[t]
		if !ok {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(pyString(t))
		b.WriteString(": ")
		b.WriteString(pyFloat(v))
	}
	b.WriteString("}}\n")
	return b.Bytes()
}

// pyStrip is Python's str.strip() with no argument, which is NOT
// bytes.TrimSpace. Python strips every character whose Unicode category is
// whitespace, and that set includes the four file/group/record/unit separators
// U+001C-U+001F; Go's TrimSpace stops at \t \n \v \f \r and space. A frame
// ending in one of those separators would be trimmed by Python and kept by Go,
// putting a stray byte inside the tape's JSON envelope.
//
// Only the ASCII range is handled here: the input is a websocket frame that
// must already be valid UTF-8 JSON, so a non-ASCII Unicode space could only
// appear inside a string literal, where neither implementation strips it.
func pyStrip(b []byte) []byte {
	isSpace := func(c byte) bool {
		switch c {
		case '\t', '\n', '\v', '\f', '\r', ' ',
			0x1c, 0x1d, 0x1e, 0x1f:
			return true
		}
		return false
	}
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

// pyString quotes a string as json.dumps does at its defaults.
//
// encoding/json is not a substitute in either direction. Go HTML-escapes `<`,
// `>` and `&` to <, > and &, which Python does not; Python
// defaults to ensure_ascii=True and escapes every non-ASCII rune to \uXXXX,
// which Go does not. Kalshi tickers are currently plain ASCII alphanumerics and
// dashes, so neither rule fires today — which is exactly why it would go
// unnoticed if a ticker ever stopped being plain.
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r < 0x7f:
				b.WriteRune(r)
			case r <= 0xffff:
				// ensure_ascii=True.
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				// Python emits a surrogate pair for astral planes.
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`,
					0xd800+(r>>10), 0xdc00+(r&0x3ff))
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyFloat formats a float64 as CPython's repr() does, which json.dumps uses for
// floats.
//
// Go's 'g' verb is NOT a substitute. It switches to exponent form once the
// exponent reaches 6, so 1000000.0 becomes "1e+06"; CPython's repr stays
// positional until 1e16 and writes "1000000.0". At the low end both break to
// exponent below 1e-4. The thresholds are the whole content of this function.
//
//	repr(1000000.0)          -> 1000000.0        Go 'g' -> 1e+06
//	repr(1e15)               -> 1000000000000000.0
//	repr(1e16)               -> 1e+16
//	repr(0.0001)             -> 0.0001
//	repr(1e-05)              -> 1e-05
//
// json.dumps spells the non-finite values Infinity / -Infinity / NaN, which are
// not legal JSON but are what Python emits by default.
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	abs := math.Abs(f)
	if f != 0 && (abs < 1e-4 || abs >= 1e16) {
		// Exponent form. Python writes at least two exponent digits and always
		// a sign, which is what Go's 'e' does with a shortest mantissa.
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// Frame writes one received frame, stamped with the local receive time.
//
// The frame is interpolated VERBATIM — the tape is only a faithful replay input
// if it preserves the exact bytes the exchange sent. Re-encoding it through a
// JSON marshaller would reorder keys and renormalise numbers. See port-spec.md
// P26.
//
// Kalshi frames carry a TRAILING NEWLINE. Without the trim the envelope's
// closing brace lands on its own line, so every record spans two lines and
// neither is valid JSON. That was the archive tapes' framing bug.
func (w *Writer) Frame(recvMs int64, frame []byte) error {
	var b bytes.Buffer
	b.Grow(len(frame) + 32)
	fmt.Fprintf(&b, `{"recv_ms":%d,"m":`, recvMs)
	b.Write(pyStrip(frame))
	b.WriteString("}\n")
	_, err := w.gz.Write(b.Bytes())
	return err
}

// Close flushes the gzip trailer.
//
// The Python rig is normally stopped with SIGTERM, which skips its `finally` and
// leaves a truncated final member — recoverable, but every tape has to be
// re-gzipped before it can be gated. Closing properly here removes that step; it
// changes no row, so it is not a divergence the gate could object to.
func (w *Writer) Close() error {
	if err := w.gz.Close(); err != nil {
		w.fh.Close()
		return err
	}
	return w.fh.Close()
}

// confidence: high

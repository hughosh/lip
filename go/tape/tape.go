// Package tape reads captured raw websocket tapes.
//
// A tape is a gzip stream of JSON envelopes, one per received frame:
//
//	{"recv_ms": 1784934732641, "m": <the exact bytes the exchange sent>}
//
// optionally preceded by a header recording the subscribed universe:
//
//	{"universe": {"TICKER": 1000.0, ...}}
//
// The header exists because Target Size is a REST field carried on no websocket
// frame, so a tape without it cannot reproduce the `reference` gate.
//
// # TWO FRAMING QUIRKS THIS READER TOLERATES
//
// Kalshi frames carry a trailing newline. An earlier capture writer interpolated
// them without stripping, which put the envelope's closing brace on its own
// line — so the tape was NOT line-delimited JSON, and every record spanned two
// lines with neither one parseable. The fix strips the frame, but the old tapes
// in lip/archive/ still have the broken framing and remain useful as fixtures.
//
// A tape being appended to by a live rig also has a truncated final gzip member.
//
// Decoding as a stream of concatenated JSON values, rather than splitting on
// newlines, handles both without special-casing: whitespace between values is
// skipped wherever it falls, and a partial value at the end simply ends the
// stream. This mirrors replay.py's raw_decode loop.
package tape

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
)

type Envelope struct {
	RecvMs   int64              `json:"recv_ms"`
	M        json.RawMessage    `json:"m"`
	Universe map[string]float64 `json:"universe"`
}

// Frames calls fn for each envelope in the tape.
//
// It returns the number of envelopes read and whether the stream ended early —
// a truncated gzip member or an incomplete trailing value, both of which mean
// "the tape is still being written", not "the tape is corrupt". Only a failure
// to open the file is returned as an error.
func Frames(path string, fn func(Envelope) error) (n int, truncated bool, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer fh.Close()

	gz, err := gzip.NewReader(fh)
	if err != nil {
		return 0, false, err
	}
	defer gz.Close()
	gz.Multistream(true)

	dec := json.NewDecoder(gz)
	for {
		var env Envelope
		if derr := dec.Decode(&env); derr != nil {
			// io.EOF is a clean end. Anything else here — ErrUnexpectedEOF, a
			// flate checksum failure, a half-written object — is the tail of a
			// tape still being appended to.
			return n, derr != io.EOF, nil
		}
		n++
		if ferr := fn(env); ferr != nil {
			return n, false, ferr
		}
	}
}

// ScanUniverse recovers the subscribed universe, with real Target Sizes when
// the tape carries a header.
//
// Tapes written after the header fix begin with {"universe": {...}}, which
// reproduces the `reference` gate exactly. Older tapes do not, so this falls
// back to the snapshot frames: the rig receives exactly one snapshot per
// subscribed market, which recovers the ticker set but not the targets. Those
// default to 0.0, which forces gate=0 — matching replay.py's behaviour, so a
// differential run over an old tape still compares like for like.
func ScanUniverse(paths []string) (map[string]float64, bool, error) {
	tickers := map[string]float64{}
	exact := false

	for _, p := range paths {
		var scanErr error
		_, _, err := Frames(p, func(env Envelope) error {
			if len(env.Universe) > 0 {
				for k, v := range env.Universe {
					tickers[k] = v
				}
				exact = true
				return nil
			}
			if len(env.M) == 0 {
				return nil
			}
			var probe struct {
				Type string `json:"type"`
				Msg  struct {
					MarketTicker string `json:"market_ticker"`
				} `json:"msg"`
			}
			if err := json.Unmarshal(env.M, &probe); err != nil {
				return nil // not our concern here; Handle reports decode failures
			}
			if probe.Type == "orderbook_snapshot" && probe.Msg.MarketTicker != "" {
				if _, seen := tickers[probe.Msg.MarketTicker]; !seen {
					tickers[probe.Msg.MarketTicker] = 0.0
				}
			}
			return nil
		})
		if err != nil {
			return nil, false, err
		}
		if scanErr != nil {
			return nil, false, scanErr
		}
	}
	return tickers, exact, nil
}

// confidence: high

// Command replay deterministically replays a captured tape through the Go core.
//
// It mirrors replay.py's CLI and semantics so the two can be pointed at the same
// tape and their output DBs compared directly. No network, no clock: the output
// is a pure function of the tape.
//
// `fill` rows are fully determined by the tape. `reference` rows additionally
// need each market's Target Size, which is REST-only — recovered from the tape
// header when present, otherwise defaulted to 0.0, which forces gate=0 on both
// implementations alike.
//
// Forward mids (mid_1m/5m/30m) are resolved by a wall-clock task in the live rig
// and are not populated here at all, on either side.
//
// Usage:
//
//	replay raw-*.jsonl.gz --out /tmp/replay-go.db
//	replay raw-*.jsonl.gz --bench
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"lip/core"
	"lip/store"
	"lip/tape"
)

// nullSink discards rows, for benchmarking the handler without SQLite.
type nullSink struct{}

func (nullSink) Fill(core.FillRow)                  {}
func (nullSink) PendingMid(_, _, _ string, _ int64) {}
func (nullSink) Reference(core.ReferenceRow)        {}

func main() {
	out := flag.String("out", "", "DB to write (default: none, benchmark only)")
	universePath := flag.String("universe", "", "JSON {ticker: target_size} to restore the real gate")
	bench := flag.Bool("bench", false, "report throughput only")
	latency := flag.Bool("latency", false, "record per-frame handler latency percentiles")
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "usage: replay [--out DB] [--universe JSON] [--bench] [--latency] TAPE...")
		os.Exit(2)
	}
	sort.Strings(paths)

	tickers, err := loadUniverse(*universePath, paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var sink core.Sink = nullSink{}
	var st *store.Store
	if *out != "" {
		st, err = store.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sink = st
	}

	rig := core.NewRig(sink, tickers)

	// Per-frame handler latency, for §10.3's L1. Preallocated so the
	// measurement does not itself allocate inside the loop; run --latency
	// SEPARATELY from the allocation and throughput measurements, because two
	// clock reads per frame and a 9 MiB sample buffer perturb both.
	var durs []int64
	if *latency {
		durs = make([]int64, 0, 1<<21)
	}

	var frames, errCount, truncated int
	t0 := time.Now()
	for _, p := range paths {
		n, trunc, err := tape.Frames(p, func(env tape.Envelope) error {
			if len(env.M) == 0 {
				return nil
			}
			frames++
			var herr error
			if *latency {
				// Timed around Handle alone, not the gzip and JSON framing
				// around it: the quoting bot's tick-to-decision path is this
				// call, and a GC pause landing inside it is exactly the tail
				// event worth catching.
				start := time.Now()
				herr = rig.Handle(env.M)
				durs = append(durs, int64(time.Since(start)))
			} else {
				herr = rig.Handle(env.M)
			}
			if herr != nil {
				errCount++
				if errCount <= 5 {
					fmt.Fprintf(os.Stderr, "[handle] %v\n", herr)
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", p, err)
			os.Exit(1)
		}
		if trunc {
			truncated++
		}
		_ = n
	}
	elapsed := time.Since(t0)

	fills, tt, refs := int64(0), int64(0), int64(0)
	if st != nil {
		if err := st.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "store:", err)
			os.Exit(1)
		}
		fills, tt, refs, err = store.Summarise(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	// Allocation counters for the optimisation phase's L1 metric (§10.3). These
	// are near-deterministic run to run, unlike wall clock, so a small
	// improvement in them is real where the same margin in frames/s is noise.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	s := rig.Stats
	fmt.Fprintf(os.Stderr, "frames        %d  in %.2fs  -> %.0f/s\n",
		frames, elapsed.Seconds(), float64(frames)/elapsed.Seconds())
	fmt.Fprintf(os.Stderr, "allocs        %d  (%.2f/frame)  total %.0f MiB  peak heap %.0f MiB\n",
		ms.Mallocs, float64(ms.Mallocs)/float64(max(frames, 1)),
		float64(ms.TotalAlloc)/(1<<20), float64(ms.HeapSys)/(1<<20))

	// GC is the mechanism behind the tail: a pause lands inside whichever
	// Handle call is unlucky, so NumGC and the worst pause are the leading
	// indicators for p99.9 and max.
	var maxPause uint64
	for _, p := range ms.PauseNs {
		if p > maxPause {
			maxPause = p
		}
	}
	fmt.Fprintf(os.Stderr, "gc            %d cycles  total pause %.1f ms  worst pause %.0f us\n",
		ms.NumGC, float64(ms.PauseTotalNs)/1e6, float64(maxPause)/1e3)

	if *latency && len(durs) > 0 {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		q := func(p float64) float64 {
			i := int(p * float64(len(durs)))
			if i >= len(durs) {
				i = len(durs) - 1
			}
			return float64(durs[i]) / 1000.0 // ns -> us
		}
		var total int64
		for _, d := range durs {
			total += d
		}
		fmt.Fprintf(os.Stderr,
			"latency/us    p50 %.2f  p90 %.2f  p99 %.2f  p99.9 %.2f  p99.99 %.2f  max %.2f  mean %.2f  (n=%d)\n",
			q(0.50), q(0.90), q(0.99), q(0.999), q(0.9999),
			float64(durs[len(durs)-1])/1000.0,
			float64(total)/float64(len(durs))/1000.0, len(durs))
	}
	fmt.Fprintf(os.Stderr, "trades seen   %d  (universe-filtered)\n", s.Trades)
	fmt.Fprintf(os.Stderr, "fills         %d   trade_through %d\n", fills, tt)
	fmt.Fprintf(os.Stderr, "reference     %d\n", refs)
	fmt.Fprintf(os.Stderr, "gaps %d  quarantined %d  stale %d  handler errors %d  truncated tapes %d\n",
		s.Gaps, s.Quarantined, rig.StaleCount(), errCount, truncated)

	if !*bench && *out != "" {
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	}
}

func loadUniverse(path string, tapes []string) (map[string]float64, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var m map[string]float64
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "universe: %d tickers from %s\n", len(m), path)
		return m, nil
	}
	t0 := time.Now()
	m, exact, err := tape.ScanUniverse(tapes)
	if err != nil {
		return nil, err
	}
	src := "snapshots (targets=0, gate forced 0)"
	if exact {
		src = "tape header (targets exact)"
	}
	fmt.Fprintf(os.Stderr, "universe: %d tickers from %s (%.1fs scan)\n",
		len(m), src, time.Since(t0).Seconds())
	return m, nil
}

// confidence: high

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
)

// The pilot config file, and why it is not just `cfg.Params` as JSON.
//
// `cfg.Params` is the §16 table as a Go struct, and unmarshalling a file
// straight onto it looks like the obvious thing to do. It is a trap, because
// two of its field types are SCALED INTEGERS:
//
//   - `num.Qty` is fixed-point hundredths of a contract (`num.QtyScale` = 100),
//     so `"S": 12` would configure 0.12 contracts, not 12.
//   - `num.Money` is a fixed-point dollar amount, so `"CapitalMax": 100` would
//     configure a fraction of a cent, not $100.
//
// Both would be accepted silently by `encoding/json` and both would then be
// checked by `Params.Validate()`, which tests RELATIONS between the knobs --
// and a set of values all wrong by the same factor of 100 satisfies most
// relations perfectly. The operator would have written the numbers from the
// spec table and got a harness configured somewhere else entirely.
//
// `num` already ships the conversions for exactly this: `QtyFromFloat` and
// `MoneyFromDollars`, the latter documented as taking "a configured or
// human-entered dollar amount". So the file is in HUMAN units -- contracts and
// dollars -- and this type is the boundary that converts them.
//
// Every field is a POINTER. Absent and zero are different: `"PnLKill": 0` is a
// loss floor of zero dollars, which is a real (if aggressive) setting, and it
// must not be indistinguishable from not having written the key at all.
type fileConfig struct {
	// --- what this run is -------------------------------------------------

	// Ticker is the single operator-chosen market of the pilot profile.
	//
	// There is no selection algorithm here and there is not meant to be. Market
	// selection does not exist in Go at all -- it is `q1select.py` -- and
	// `risk.Snapshot.SelectedTickers()` only reads a flag someone else set.
	// pilot-plan §7.1 is "one operator-chosen ticker", so this is a decision
	// the operator makes and records, not one the harness derives.
	Ticker *string `json:"ticker"`

	// Rung names the capital ladder step (pilot-plan §1). It is asserted
	// against the sizing below rather than deriving it, so a config that says
	// "canary" and sizes like the pilot is refused instead of quietly obeyed.
	Rung *string `json:"rung"`

	// --- sizing, in CONTRACTS (§6.2) --------------------------------------

	S       *float64 `json:"s"`
	SMax    *float64 `json:"s_max"`
	InvSoft *float64 `json:"inv_soft"`
	InvHard *float64 `json:"inv_hard"`
	InvKill *float64 `json:"inv_kill"`

	// --- money, in DOLLARS -------------------------------------------------

	CapitalMax *float64 `json:"capital_max"`
	// PnLKill is a LOSS FLOOR and is therefore negative (§12, H-HALT-5).
	PnLKill *float64 `json:"pnl_kill"`

	// --- operational (§16) -------------------------------------------------

	NMarkets     *int     `json:"n_markets"`
	DrainTimeout *float64 `json:"drain_timeout_h"`
	Heartbeat    *float64 `json:"heartbeat_s"`

	// --- paths -------------------------------------------------------------
	//
	// Every one is required and none has a default. A trading binary that
	// invents the path to its own durable halt latch can be pointed at a fresh
	// empty one by being run from a different directory, which is H-HALT-4
	// erased by a `cd`.
	Paths pathConfig `json:"paths"`
}

type pathConfig struct {
	DB         *string `json:"db"`
	AnomalyLog *string `json:"anomaly_log"`
	Latch      *string `json:"latch"`
	Lock       *string `json:"lock"`
	// Key and Env are the credentials `feed.NewSignerFrom` reads. Named
	// explicitly rather than defaulted to ~/.kalshi so that which account this
	// process can reach is a property of the config, visible in a diff.
	Key *string `json:"key"`
	Env *string `json:"env"`
}

// config is the validated result: the §16 params plus this run's identity.
type config struct {
	Params cfg.Params
	Ticker string
	Rung   rung
	Paths  paths
}

type paths struct {
	DB, AnomalyLog, Latch, Lock, Key, Env string
}

// rung is the capital ladder of pilot-plan §1.
//
// It exists because "build once; raise the knob" makes the knob the only thing
// standing between a $1 canary and a $100 pilot, and a knob with nothing
// asserting what it means is a knob that gets raised by accident.
type rung struct {
	name  string
	maxS  num.Qty
	human string
}

var rungs = map[string]rung{
	"canary": {name: "canary", maxS: num.QtyFromFloat(1),
		human: "1 market, S=1, ~$1: does the order path work at all -- fills, " +
			"attribution, is_taker=false, verified cancel, restart adoption"},
	"pilot": {name: "pilot", maxS: num.QtyFromFloat(12),
		human: "1 market, S=12, $100: do we get paid, and does the scoring " +
			"model predict the payout"},
	"second": {name: "second", maxS: num.QtyFromFloat(12),
		human: "2 markets, S=12: does anything break with concurrency"},
	"scale": {name: "scale", maxS: num.QtyFromFloat(100),
		human: "6 markets, larger S, $500 -- ONLY after a real payout is observed"},
}

func rungNames() []string { return []string{"canary", "pilot", "second", "scale"} }

// loadConfig reads the file, overlays it on §16's defaults and validates.
//
// Unknown keys are a HARD ERROR. A misspelt knob that silently keeps its
// default is a config file that lies: the operator reads it back, sees the
// value they intended, and the process is running on something else. That is
// the same failure mode as a rotted mutation anchor, and it is worth the same
// refusal.
func loadConfig(path string) (config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("reading config: %w", err)
	}

	var fc fileConfig
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return config{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	// §16's table is the baseline. The file names DEVIATIONS from it, so a diff
	// of the file against an empty one is the whole config review.
	p := cfg.Default()

	if fc.S != nil {
		p.S = num.QtyFromFloat(*fc.S)
	}
	if fc.SMax != nil {
		p.SMax = num.QtyFromFloat(*fc.SMax)
	}
	if fc.InvSoft != nil {
		p.InvSoft = num.QtyFromFloat(*fc.InvSoft)
	}
	if fc.InvHard != nil {
		p.InvHard = num.QtyFromFloat(*fc.InvHard)
	}
	if fc.InvKill != nil {
		p.InvKill = num.QtyFromFloat(*fc.InvKill)
	}
	if fc.CapitalMax != nil {
		p.CapitalMax = num.MoneyFromDollars(*fc.CapitalMax)
	}
	if fc.PnLKill != nil {
		p.PnLKill = num.MoneyFromDollars(*fc.PnLKill)
	}
	if fc.NMarkets != nil {
		p.NMarkets = *fc.NMarkets
	}
	if fc.DrainTimeout != nil {
		p.DrainTimeout = time.Duration(*fc.DrainTimeout * float64(time.Hour))
	}
	if fc.Heartbeat != nil {
		p.Heartbeat = time.Duration(*fc.Heartbeat * float64(time.Second))
	}

	// Validate() checks the RELATIONS between knobs (§16). It runs after the
	// overlay and never before: a file that changes inv_hard without inv_soft
	// is exactly the case it exists to refuse.
	if err := p.Validate(); err != nil {
		return config{}, fmt.Errorf("config %s is not a valid §16 parameter "+
			"set: %w", path, err)
	}

	c := config{Params: p}

	if fc.Ticker == nil || *fc.Ticker == "" {
		return config{}, errors.New("config names no ticker: the pilot profile " +
			"is one operator-chosen market, and there is no selection " +
			"algorithm in this binary to derive one")
	}
	c.Ticker = *fc.Ticker

	if fc.Rung == nil {
		return config{}, fmt.Errorf("config names no rung; one of %v. Capital "+
			"is a ladder rather than a decision (pilot-plan §1) and the rung is "+
			"what says which step this run is on", rungNames())
	}
	r, ok := rungs[*fc.Rung]
	if !ok {
		return config{}, fmt.Errorf("unknown rung %q; one of %v",
			*fc.Rung, rungNames())
	}
	c.Rung = r

	// The rung is ASSERTED against the sizing, not used to derive it. A config
	// that says canary and sizes like the pilot is a config whose author
	// believed one thing while the harness would have done another.
	if p.S > r.maxS {
		return config{}, fmt.Errorf("rung %q allows S up to %s contracts but "+
			"the config sets S=%s. %s. Raising size is a deliberate act: name "+
			"the rung that permits it",
			r.name, r.maxS.Wire(), p.S.Wire(), r.human)
	}

	if c.Paths, err = resolvePaths(fc.Paths); err != nil {
		return config{}, err
	}
	return c, nil
}

// resolvePaths requires every path, absolutely.
//
// A relative path here is a path that means something different depending on
// the working directory the supervisor happened to use, and one of these is the
// durable halt latch. H-HALT-4 survives a restart only if the restarted process
// looks in the same place.
func resolvePaths(pc pathConfig) (paths, error) {
	need := []struct {
		name string
		v    *string
		why  string
	}{
		{"db", pc.DB, "the five-record store"},
		{"anomaly_log", pc.AnomalyLog, "the append-only anomaly journal"},
		{"latch", pc.Latch, "the durable halt latch (H-HALT-4)"},
		{"lock", pc.Lock, "the single-instance lock"},
		{"key", pc.Key, "the RSA private key used to sign every request"},
		{"env", pc.Env, "the file holding KALSHI_API_KEY_ID"},
	}
	var out paths
	dst := []*string{&out.DB, &out.AnomalyLog, &out.Latch, &out.Lock,
		&out.Key, &out.Env}

	for i, n := range need {
		if n.v == nil || *n.v == "" {
			return paths{}, fmt.Errorf("config sets no paths.%s (%s); there is "+
				"no default, because a trading binary that invents the path to "+
				"its own halt latch can be handed a fresh empty one by being "+
				"started from a different directory", n.name, n.why)
		}
		if !filepath.IsAbs(*n.v) {
			return paths{}, fmt.Errorf("paths.%s is %q, which is relative; it "+
				"would resolve differently under launchd than under a shell, "+
				"and for the latch that means H-HALT-4 erased by a working "+
				"directory", n.name, *n.v)
		}
		*dst[i] = filepath.Clean(*n.v)
	}
	return out, nil
}

// confidence: high

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
	"lip/harness/risk"
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

	// EarlyCloseLead is how long before `close_time` this harness stops ADDING
	// in a market the exchange reports as `can_close_early`. Hours.
	//
	// # Why there is a second lead at all
	//
	// §16's `close_lead` is 1h -- cut from 4h by HR-011 to bound the window in
	// which a resting reducer faces a stale book. That number is chosen against
	// a close we can SEE COMING. H-CLOSE-4's markets are the ones we cannot:
	// they settle before `close_time` on external information -- a resolution
	// source, an event outcome -- and no amount of polling predicts WHEN.
	//
	// A survey of the live universe on 2026-08-07 is what makes this a real
	// case rather than a hypothetical: 192 of the 200 active LIP programmes are
	// `can_close_early`. Treating it as the exceptional market would leave the
	// pilot with eight candidates, and treating an unpredictable close as
	// though the arithmetic held would leave inventory to settle at whatever
	// the resolution turned out to be.
	//
	// So the operator's rule: in those markets, stop adding EARLY -- four hours
	// out rather than one -- and let the exit stay alive. It is expressed
	// through §5.2's market-scoped stop, which is exactly "adding side
	// cancelled and confirmed absent, capped reducer rests" (A8), and it
	// composes with `close_lead` and `final_lead` rather than replacing either:
	// SETTLING still begins at `close_time - close_lead`, and H-CLOSE-3's final
	// cancel still runs at `final_lead`.
	//
	// # Why it is HERE and not in cfg.Params
	//
	// §16 is the table the `run` row records verbatim, and `params_test.go`
	// asserts `cfg.Params` against a fixed map field-for-field -- "adding a knob
	// here would be a spec deviation dressed as configuration". This is a
	// deployment decision about which markets the pilot will hold overnight, so
	// it belongs with the ticker and the rung.
	//
	// Absent means the default below. Zero DISABLES the early backoff, which is
	// the spec's literal reading (H-CLOSE-4 accepts the risk and asks selection
	// to prefer against these markets), and is therefore a value worth being
	// able to express.
	EarlyCloseLead *float64 `json:"early_close_lead_h"`

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
	// LiveOK is H-VER-1's second key: the sentinel whose PRESENCE, together
	// with an explicit `-live`, is what permits a write.
	//
	// The PATH is configuration; the FILE is not, and nothing in this binary
	// ever creates it. `-provision` deliberately does not, because a
	// provisioning step that armed the machine would make "set it up" and "let
	// it trade" the same act. The operator creates it by hand, and removes it
	// to disarm the next write.
	LiveOK *string `json:"live_ok"`
	// Stop is §12's out-of-band halt: the file whose PRESENCE asks a running
	// harness to wind down.
	//
	// It is the mirror image of `live_ok` and the pairing is the point. One file
	// arms the process and the other stops it, neither is ever created by this
	// binary, and both are operator actions expressed as a filesystem fact
	// rather than as a signal -- because a signal needs a pid, and the operator
	// reaching for this at 3am has a config file and a shell.
	//
	// It is NOT a drain. §5.1 is explicit that DRAINED does not exit the
	// process, and `SignalController.Confirm` refuses a permit to any cause that
	// is not SIGTERM or SIGINT, so this can stop the harness adding and can
	// never end it.
	Stop *string `json:"stop"`
}

// config is the validated result: the §16 params plus this run's identity.
type config struct {
	Params cfg.Params
	Ticker string
	Rung   rung
	// EarlyCloseLead is the operator's H-CLOSE-4 backoff. See the field comment
	// on `fileConfig`. Zero disables it.
	EarlyCloseLead time.Duration
	Paths          paths
	// Live is H-VER-1's first key, and it comes from the INVOCATION rather than
	// from the file -- `fileConfig` has no corresponding field and
	// `DisallowUnknownFields` means a config that tried to set one is refused.
	//
	// Its zero value is read-only, which is what makes every caller that has
	// not been updated safe by default: a test that builds a `config` literal,
	// a future subcommand, and this struct's own zero value all produce a
	// process that cannot place an order.
	Live bool
}

type paths struct {
	DB, AnomalyLog, Latch, Lock, Key, Env string
	// LiveOK is the write-arming sentinel (H-VER-1). Its path is required and
	// validated; its existence is checked freshly at every write and never
	// here, so an operator can arm and disarm without touching the config.
	LiveOK string
	// Stop is §12's out-of-band halt sentinel. Same discipline as LiveOK: the
	// path is validated here and the file's existence is checked freshly by the
	// owner, never here, so the operator can halt a running process without
	// touching the config or finding its pid.
	Stop string
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
	// stopOnFirstOwnedFill is pilot-plan §7.9's bound on the canary: the FIRST
	// directional entry after adoption latches WINDING_DOWN, whatever its size.
	//
	// It is a property of the RUNG and not a knob, because no assignment of
	// §16's numbers delivers it. Fills are fractional down to the 0.01-contract
	// quantum (num.QtyScale = 100), F17 compares with a strict `>`, and
	// `inv_kill` must sit strictly above `inv_hard` which must sit strictly
	// above `inv_soft` which must be positive. So there is no ordering in which
	// every 0.01 fill breaches `inv_kill`, and a fill between `inv_hard` and
	// `inv_kill` breaches only the MARKET-scoped brake, which self-clears at
	// flat (quote/machine.go) and lets the harness resume adding.
	//
	// It lives on the compiled table rather than in `fileConfig` for the reason
	// the table exists at all: the `"rung"` string is what a config uses to name
	// its exposure, and a separate boolean would let a file call itself canary
	// while disabling the one bound that word promises. §16 is the recorded
	// parameter table and this is not one of its knobs -- `params_test.go`
	// asserts that set field-for-field.
	stopOnFirstOwnedFill bool
}

var rungs = map[string]rung{
	"canary": {name: "canary", maxS: num.QtyFromFloat(1),
		stopOnFirstOwnedFill: true,
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

// defaultEarlyCloseLead is four hours: §16's `close_lead` as it stood before
// HR-011 cut it to one, kept for exactly the markets HR-011's argument does not
// reach -- the ones whose close is not predictable from the schedule at all.
const defaultEarlyCloseLead = 4 * time.Hour

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

	// H-CAP-8, and it is a SECOND validation rather than part of the first
	// because `Validate()` disclaims it in its own doc comment: the rule needs
	// a price bound and the capital model, which live in `harness/risk`.
	//
	// # Why the loader and not the run arm
	//
	// `CheckFundable` is a pure function of the FILE -- S, n_markets and
	// capital_max, against a reserve the file cannot even set. Nothing from the
	// invocation enters it, so it belongs with the file's own validation.
	// `checkRung` is the mirror image and stays in the run arm for the opposite
	// reason: it asserts the file against a flag, and a flag is the one input
	// this function does not have.
	//
	// The placement also decides `-deploy` and `-provision`, both of which run
	// through here before the switch in `main`. That is the point rather than a
	// side effect: an unfundable config written into a plist is a start that
	// fails FOREVER under `KeepAlive`, throttled by launchd and watched by
	// nothing -- the exact failure lip-3yo removed for the rung, and it would
	// otherwise have been left open here. Refusing before `provision` is the
	// same argument one step earlier: no durable store for a configuration that
	// can never legally start.
	//
	// §16 sets `capital_reserve` at 0.25 and `fileConfig` has no field for it,
	// so only the first of `CheckFundable`'s two checks can fire on this path:
	// passing it means `n · S ≤ 0.7576 · capital_max`, which already satisfies
	// the round-trip bound. The second check is reachable only by a caller that
	// can drive the reserve, and `capital_test.go` is that caller.
	if err := risk.CheckFundable(p); err != nil {
		return config{}, fmt.Errorf("config %s cannot fund its own reducer: %w",
			path, err)
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

	if c.Paths, err = resolvePaths(fc.Paths, path); err != nil {
		return config{}, err
	}

	// The close is checked LAST, after the paths. Both are required, so the
	// order only decides which failure an operator is shown first -- and a
	// mistyped path is the more basic error, the one that makes every other
	// answer about this configuration provisional.
	// The close itself is READ, not configured. `harness-spec.md` §9's source
	// table names the endpoint outright -- `market.close_time` from
	// `/markets/{ticker}` -- and `schedule_poll_s` (30s) is the cadence
	// H-CLOSE-0 already validates `final_lead` against. An earlier revision of
	// this file made it a required config field on the belief that no such read
	// existed in this binary; that was true of the code and false of the
	// design, and a safety-critical timestamp copied by hand is one that goes
	// stale silently.
	c.EarlyCloseLead = defaultEarlyCloseLead
	if fc.EarlyCloseLead != nil {
		c.EarlyCloseLead = time.Duration(*fc.EarlyCloseLead * float64(time.Hour))
		if c.EarlyCloseLead < 0 {
			return config{}, fmt.Errorf("early_close_lead_h is %v, which is "+
				"negative; a lead is a duration BEFORE the close, and zero is "+
				"already how the early backoff is turned off",
				*fc.EarlyCloseLead)
		}
	}
	// A backoff shorter than the ordinary lead is not a backoff. It would be
	// reached AFTER `close_lead` had already taken the market to SETTLING and
	// cancelled the adding side, so it could never fire -- a knob that reads as
	// set and does nothing.
	if c.EarlyCloseLead > 0 && c.EarlyCloseLead <= p.CloseLead {
		return config{}, fmt.Errorf("early_close_lead_h is %v, at or inside "+
			"§16's close_lead %v. The early backoff exists to stop adding "+
			"SOONER than the ordinary lead in a market whose close cannot be "+
			"predicted; at or below close_lead the market is already SETTLING "+
			"with its adding side cancelled by the time this would fire, so "+
			"the knob would read as set and do nothing",
			c.EarlyCloseLead, p.CloseLead)
	}
	return c, nil
}

// resolvePaths requires every path, absolutely.
//
// A relative path here is a path that means something different depending on
// the working directory the supervisor happened to use, and one of these is the
// durable halt latch. H-HALT-4 survives a restart only if the restarted process
// looks in the same place.
func resolvePaths(pc pathConfig, configPath string) (paths, error) {
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
		{"live_ok", pc.LiveOK, "the write-arming sentinel (H-VER-1)"},
		{"stop", pc.Stop, "the out-of-band halt sentinel (§12 harness.stop)"},
	}
	var out paths
	dst := []*string{&out.DB, &out.AnomalyLog, &out.Latch, &out.Lock,
		&out.Key, &out.Env, &out.LiveOK, &out.Stop}

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

	// BOTH sentinels must be their OWN file. Every other path here is something
	// the harness creates or requires in order to run at all, so a sentinel
	// aliased onto one of them would exist for reasons that have nothing to do
	// with what it signals: provisioning the store would arm the machine, and so
	// would having credentials. The config file itself is included because it is
	// the one path guaranteed to exist whenever the binary runs.
	//
	// Compared AFTER cleaning, so `/a/b` and `/a/./b` do not slip past.
	others := []struct{ name, path string }{
		{"db", out.DB}, {"anomaly_log", out.AnomalyLog}, {"latch", out.Latch},
		{"lock", out.Lock}, {"key", out.Key}, {"env", out.Env},
	}
	if configPath != "" {
		if abs, err := filepath.Abs(configPath); err == nil {
			others = append(others, struct{ name, path string }{
				"the config file", filepath.Clean(abs)})
		}
	}
	// The two sentinels are checked against the shared list AND against each
	// other, and the second half is the one that matters most. They are opposite
	// instructions -- one permits the next write, the other stops the process
	// adding -- so a config that aliased them would arm the harness with the
	// same act that halts it, and disarm it by lifting the halt.
	sentinels := []struct {
		name, path, why string
	}{
		{"live_ok", out.LiveOK, "the write-arming sentinel must be a file that " +
			"exists for NO other reason: sharing it means the harness arms " +
			"itself the moment it is provisioned or given credentials, and " +
			"H-VER-1's second key stops being a key at all"},
		{"stop", out.Stop, "the halt sentinel must be a file that exists for NO " +
			"other reason: sharing it means the harness stops adding the moment " +
			"it is provisioned or given credentials, and §12's out-of-band halt " +
			"becomes a condition nobody asked for"},
	}
	for i, s := range sentinels {
		for _, o := range others {
			if o.path == s.path {
				return paths{}, fmt.Errorf("paths.%s is %q, which is also %s. %s",
					s.name, s.path, o.name, s.why)
			}
		}
		for _, t := range sentinels[i+1:] {
			if t.path == s.path {
				return paths{}, fmt.Errorf("paths.%s and paths.%s are both %q. "+
					"They are OPPOSITE instructions -- one permits the next "+
					"write, the other stops the harness adding -- so aliasing "+
					"them means arming the harness is the same act as halting "+
					"it, and disarming it is the same act as lifting the halt",
					s.name, t.name, s.path)
			}
		}
	}
	return out, nil
}

// confidence: high

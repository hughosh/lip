package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
)

// writeConfig puts a config file in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "pilot.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// goodTail is the absolute, complete path block every valid config needs.
// Tests that are not about paths use it so a path error cannot be mistaken for
// the failure under test.
func goodTail(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	return `"paths":{` +
		`"db":"` + filepath.Join(d, "harness.db") + `",` +
		`"anomaly_log":"` + filepath.Join(d, "anomaly.jsonl") + `",` +
		`"latch":"` + filepath.Join(d, "harness.halt") + `",` +
		`"lock":"` + filepath.Join(d, "harness.lock") + `",` +
		`"key":"` + filepath.Join(d, "kalshi.pem") + `",` +
		`"env":"` + filepath.Join(d, "env") + `",` +
		`"live_ok":"` + filepath.Join(d, "live_ok") + `"}`
}

// TestConfigSizesAreContractsAndDollarsNotRawQuanta is the whole reason this
// package does not unmarshal straight onto cfg.Params.
//
// `num.Qty` is fixed-point HUNDREDTHS of a contract and `num.Money` is a
// fixed-point dollar amount. A JSON decoder pointed at `cfg.Params` accepts
// `"S": 1` and stores `Qty(1)`, which is one hundredth of a contract -- and
// `Params.Validate()` does not catch it, because it checks RELATIONS between
// knobs and a whole config wrong by the same factor of 100 keeps every relation
// intact. The operator would have copied the numbers out of §16 and got a
// harness sized somewhere else entirely.
//
// So the file is in human units and the boundary converts. This test is what
// pins that: it asserts the STORED value against the conversion helpers `num`
// ships for exactly this purpose, not against a raw integer.
func TestConfigSizesAreContractsAndDollarsNotRawQuanta(t *testing.T) {
	p := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"pilot",
		"s":12,"s_max":24,"inv_soft":18,"inv_hard":36,"inv_kill":48,
		"capital_max":100,"pnl_kill":-25,`+goodTail(t)+`}`)

	c, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if want := num.QtyFromFloat(12); c.Params.S != want {
		t.Fatalf("S is %d (%s contracts), want %d (12 contracts). A raw JSON "+
			"overlay onto cfg.Params would store 12 here, which is 0.12 "+
			"contracts -- the config would be 100x small and Validate() would "+
			"not notice", c.Params.S, c.Params.S.Wire(), want)
	}
	if got := c.Params.S.Float(); got != 12 {
		t.Fatalf("S reads back as %v contracts, want 12", got)
	}
	if want := num.MoneyFromDollars(100); c.Params.CapitalMax != want {
		t.Fatalf("CapitalMax is %d, want %d ($100). Raw JSON would store 100 "+
			"here, which is a fraction of a cent", c.Params.CapitalMax, want)
	}
	if want := num.MoneyFromDollars(-25); c.Params.PnLKill != want {
		t.Fatalf("PnLKill is %d, want %d (-$25); it is a LOSS FLOOR and is "+
			"negative by construction (§12, H-HALT-5)", c.Params.PnLKill, want)
	}
}

// TestUnknownConfigKeyIsRefused: a misspelt knob that silently keeps its default
// is a config file that lies. The operator reads it back, sees the value they
// meant, and the process is running on something else.
func TestUnknownConfigKeyIsRefused(t *testing.T) {
	p := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"canary","s":1,
		"inv_kil":48,`+goodTail(t)+`}`)

	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("a config with a misspelt key (inv_kil) was accepted; the knob " +
			"silently keeps its §16 default and the file no longer describes " +
			"the running process")
	}
	if !strings.Contains(err.Error(), "inv_kil") {
		t.Fatalf("the error does not name the offending key: %v", err)
	}
}

// TestRungIsAssertedAgainstSizeRatherThanDerivingIt is pilot-plan §1's
// "build once; raise the knob" made refusable.
//
// The knob is the only thing between a $1 canary and a $100 pilot. A config
// that names the canary while sizing like the pilot is one whose author
// believed one thing while the harness would have done another, and the harness
// is the party holding the money.
func TestRungIsAssertedAgainstSizeRatherThanDerivingIt(t *testing.T) {
	p := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"canary","s":12,`+goodTail(t)+`}`)

	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("rung \"canary\" accepted S=12; the canary rung is S=1, and " +
			"raising size must be a deliberate act that names the rung " +
			"permitting it")
	}
	for _, want := range []string{"canary", "deliberate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not explain itself (missing %q): %v",
				want, err)
		}
	}

	// ...and the same size on the rung that permits it is fine.
	ok := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"pilot","s":12,`+goodTail(t)+`}`)
	if _, err := loadConfig(ok); err != nil {
		t.Fatalf("rung \"pilot\" refused S=12, which it permits: %v", err)
	}
}

// TestRelativePathsAreRefused. A relative path resolves differently under
// launchd than under a shell, and one of these is the durable halt latch --
// H-HALT-4 survives a restart only if the restarted process looks in the same
// place. A latch found at a fresh empty path is a halt that self-cleared.
func TestRelativePathsAreRefused(t *testing.T) {
	d := t.TempDir()
	p := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"canary","s":1,
		"paths":{"db":"`+filepath.Join(d, "harness.db")+`",
		"anomaly_log":"`+filepath.Join(d, "a.jsonl")+`",
		"latch":"harness.halt",
		"lock":"`+filepath.Join(d, "l.lock")+`",
		"key":"`+filepath.Join(d, "k.pem")+`",
		"env":"`+filepath.Join(d, "env")+`",
		"live_ok":"`+filepath.Join(d, "live_ok")+`"}}`)

	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("a relative latch path was accepted")
	}
	if !strings.Contains(err.Error(), "latch") {
		t.Fatalf("the error does not name the offending path: %v", err)
	}
}

// TestEveryPathIsRequired: there is no default for any of them, and the reason
// differs per path but lands in the same place -- a trading binary that invents
// where its own durable state lives can be pointed at an empty copy.
func TestEveryPathIsRequired(t *testing.T) {
	for _, missing := range []string{
		"db", "anomaly_log", "latch", "lock", "key", "env", "live_ok",
	} {
		d := t.TempDir()
		all := map[string]string{
			"db":          filepath.Join(d, "harness.db"),
			"anomaly_log": filepath.Join(d, "a.jsonl"),
			"latch":       filepath.Join(d, "h.halt"),
			"lock":        filepath.Join(d, "l.lock"),
			"key":         filepath.Join(d, "k.pem"),
			"env":         filepath.Join(d, "env"),
			"live_ok":     filepath.Join(d, "live_ok"),
		}
		delete(all, missing)
		var b strings.Builder
		b.WriteString(`{"ticker":"KXTEST-A","rung":"canary","s":1,"paths":{`)
		first := true
		for k, v := range all {
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(`"` + k + `":"` + v + `"`)
		}
		b.WriteString(`}}`)

		if _, err := loadConfig(writeConfig(t, b.String())); err == nil {
			t.Fatalf("config with no paths.%s was accepted", missing)
		}
	}
}

// TestConfigWithoutTickerIsRefused. There is no selection algorithm in this
// binary -- selection is q1select.py -- so an absent ticker cannot be derived,
// only guessed.
func TestConfigWithoutTickerIsRefused(t *testing.T) {
	p := writeConfig(t, `{"rung":"canary","s":1,`+goodTail(t)+`}`)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("a config naming no ticker was accepted")
	}
}

// TestUnnamedRungIsRefused. Capital is a ladder rather than a decision, and a
// run that does not say which step it is on has not made the decision.
func TestUnnamedRungIsRefused(t *testing.T) {
	p := writeConfig(t, `{"ticker":"KXTEST-A","s":1,`+goodTail(t)+`}`)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("a config naming no rung was accepted")
	}
	bad := writeConfig(t, `{"ticker":"KXTEST-A","rung":"enormous","s":1,`+
		goodTail(t)+`}`)
	if _, err := loadConfig(bad); err == nil {
		t.Fatal("an unknown rung was accepted")
	}
}

// TestUnsetKnobsKeepTheSpecDefault. The file names DEVIATIONS from §16, so a
// diff of it against an empty one is the whole config review.
func TestUnsetKnobsKeepTheSpecDefault(t *testing.T) {
	p := writeConfig(t, `{"ticker":"KXTEST-A","rung":"canary","s":1,`+
		goodTail(t)+`}`)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	def := cfg.Default()
	if c.Params.RequoteInterval != def.RequoteInterval {
		t.Fatalf("an unset knob drifted from its §16 default: requote_interval "+
			"is %v, want %v", c.Params.RequoteInterval, def.RequoteInterval)
	}
	if c.Params.DrainTimeout != def.DrainTimeout {
		t.Fatalf("an unset knob drifted: drain_timeout is %v, want %v",
			c.Params.DrainTimeout, def.DrainTimeout)
	}
}

// TestEarlyCloseLeadDefaultsToTheLeadHR011Removed.
//
// §16's `close_lead` was cut 4h -> 1h by HR-011 to bound how long a resting
// reducer faces a stale book. That trade is made against a close we can see
// coming; `can_close_early` markets are the ones we cannot, and 192 of the 200
// active LIP programmes carry the flag. So the pre-HR-011 four hours survives as
// the backoff for exactly those markets, and the default is what an operator who
// writes no knob at all gets.
func TestEarlyCloseLeadDefaultsToTheLeadHR011Removed(t *testing.T) {
	p := writeConfig(t, `{"ticker":"KXTEST-A","rung":"canary","s":1,`+
		goodTail(t)+`}`)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.EarlyCloseLead != 4*time.Hour {
		t.Fatalf("early_close_lead defaulted to %v, want 4h", c.EarlyCloseLead)
	}
	if c.EarlyCloseLead <= c.Params.CloseLead {
		t.Fatalf("the early backoff (%v) is not ahead of §16's close_lead (%v), "+
			"so it could never fire", c.EarlyCloseLead, c.Params.CloseLead)
	}
}

// TestEarlyCloseLeadInsideCloseLeadIsRefused.
//
// A backoff at or inside the ordinary lead is a knob that reads as set and does
// nothing: by the time it would fire, `close_lead` has already taken the market
// to SETTLING and cancelled the adding side. Refusing it is the difference
// between a configuration that is wrong and one that is quietly inert.
func TestEarlyCloseLeadInsideCloseLeadIsRefused(t *testing.T) {
	def := cfg.Default()
	for _, h := range []float64{
		def.CloseLead.Hours(),     // exactly at close_lead
		def.CloseLead.Hours() / 2, // inside it
	} {
		body := fmt.Sprintf(`{"ticker":"KXTEST-A","rung":"canary","s":1,`+
			`"early_close_lead_h":%v,`+goodTail(t)+`}`, h)
		if _, err := loadConfig(writeConfig(t, body)); err == nil {
			t.Fatalf("early_close_lead_h=%v was accepted against close_lead %v",
				h, def.CloseLead)
		}
	}
}

// TestEarlyCloseBackoffCanBeTurnedOff. Zero is the spec's literal reading:
// H-CLOSE-4 accepts the early-close risk and asks selection to prefer against
// those markets rather than backing off in them. It is a real position, so it
// has to be expressible.
func TestEarlyCloseBackoffCanBeTurnedOff(t *testing.T) {
	p := writeConfig(t, `{"ticker":"KXTEST-A","rung":"canary","s":1,`+
		`"early_close_lead_h":0,`+goodTail(t)+`}`)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig refused a disabled early backoff: %v", err)
	}
	if c.EarlyCloseLead != 0 {
		t.Fatalf("early_close_lead_h=0 stored %v", c.EarlyCloseLead)
	}
}

// TestNoCloseTimeKnobExists is the deliberate ABSENCE.
//
// §9's source table names `/markets/{ticker}` as where `close_time` comes from,
// and an earlier revision of this file made it a config field on the belief that
// this binary had no such read. A hand-copied close is a safety-critical
// timestamp that goes stale silently, so the key is refused outright rather than
// accepted and ignored -- which, given `DisallowUnknownFields`, is what a config
// carrying it now gets.
func TestNoCloseTimeKnobExists(t *testing.T) {
	body := `{"ticker":"KXTEST-A","rung":"canary","s":1,` +
		`"close_time":"2026-08-09T21:00:00Z",` + goodTail(t) + `}`
	_, err := loadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("a config naming close_time was accepted; the schedule is READ " +
			"from /markets/{ticker} at schedule_poll_s, and a file that also " +
			"names it is a second source of truth for the same fact")
	}
	if !strings.Contains(err.Error(), "close_time") {
		t.Fatalf("the error does not name the offending key: %v", err)
	}
}

// TestTheShippedExampleConfigLoads keeps `config.example.json` honest.
//
// The example exists because `-provision` cannot be invoked without a config,
// and an operator's first act is to copy it. That makes it code: `fileConfig`
// gains a required key, or a knob's units change, and the example silently
// becomes a file that no longer loads -- discovered by the operator, at the
// moment they were trying to start a trading process, with `DisallowUnknownFields`
// giving them a parse error rather than an explanation.
//
// It is asserted here rather than reviewed, because nothing else in the tree
// reads this file at all.
func TestTheShippedExampleConfigLoads(t *testing.T) {
	// `go test` runs in the package directory; the example is at the repo root.
	const rel = "../../../config.example.json"
	if _, err := os.Stat(rel); err != nil {
		t.Fatalf("the example config is missing: %v.\n\nIt is the only thing "+
			"an operator has to copy before -provision, and there is no "+
			"default config anywhere in this binary", err)
	}

	c, err := loadConfig(rel)
	if err != nil {
		t.Fatalf("the shipped example config does not load: %v", err)
	}

	// The example must stay a CANARY. It is the file that gets copied, and a
	// copied file that sizes like the pilot is how a $1 experiment becomes a
	// $100 one without anybody deciding to raise the rung.
	if c.Rung.name != "canary" {
		t.Fatalf("the example config is rung %q, want canary", c.Rung.name)
	}
	if c.Params.S > num.QtyFromFloat(1) {
		t.Fatalf("the example config sets S=%s; the file an operator copies "+
			"first must be the smallest rung on the ladder", c.Params.S.Wire())
	}

	// And it must NOT name a real market. A shipped example carrying a live
	// ticker is one `cp` away from quoting a market nobody selected for this
	// run -- `q1select.py` chooses it, and the choice is per-run.
	if !strings.Contains(c.Ticker, "REPLACE") {
		t.Fatalf("the example config names ticker %q, which does not look "+
			"like a placeholder; the ticker is chosen per run by q1select.py "+
			"and shipping a real one invites it being traded by default",
			c.Ticker)
	}
}

// TestLiveOKPathIsRequiredAbsoluteAndDedicated is H-VER-1's second key stated as
// what would silently destroy it.
//
// The sentinel's whole value is that it exists for NO other reason. Aliased onto
// a path the harness needs anyway, it stops being a key: provisioning the store
// would arm the machine, and so would having credentials on it. A relative path
// is the same failure by another route -- it resolves against the working
// directory, so the same config arms under a shell and disarms under launchd.
func TestLiveOKPathIsRequiredAbsoluteAndDedicated(t *testing.T) {
	base := func(d string) map[string]string {
		return map[string]string{
			"db":          filepath.Join(d, "harness.db"),
			"anomaly_log": filepath.Join(d, "a.jsonl"),
			"latch":       filepath.Join(d, "h.halt"),
			"lock":        filepath.Join(d, "l.lock"),
			"key":         filepath.Join(d, "k.pem"),
			"env":         filepath.Join(d, "env"),
			"live_ok":     filepath.Join(d, "live_ok"),
		}
	}
	write := func(t *testing.T, d string, m map[string]string) string {
		t.Helper()
		var b strings.Builder
		b.WriteString(`{"ticker":"KXTEST-A","rung":"canary","s":1,"paths":{`)
		first := true
		for k, v := range m {
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(`"` + k + `":"` + v + `"`)
		}
		b.WriteString("}}")
		p := filepath.Join(d, "config.json")
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("relative", func(t *testing.T) {
		d := t.TempDir()
		m := base(d)
		m["live_ok"] = "live_ok"
		if _, err := loadConfig(write(t, d, m)); err == nil {
			t.Fatal("a relative live_ok was accepted; the same config would " +
				"arm under a shell and disarm under launchd")
		}
	})

	for _, alias := range []string{"db", "latch", "key", "env"} {
		t.Run("aliased onto "+alias, func(t *testing.T) {
			d := t.TempDir()
			m := base(d)
			m["live_ok"] = m[alias]
			_, err := loadConfig(write(t, d, m))
			if err == nil {
				t.Fatalf("live_ok was accepted while pointing at paths.%s. "+
					"That file exists because the harness needs it, so the "+
					"second key would be present the moment the harness was "+
					"usable at all", alias)
			}
			if !strings.Contains(err.Error(), "live_ok") {
				t.Fatalf("the error does not name live_ok: %v", err)
			}
		})
	}

	t.Run("aliased onto the config file", func(t *testing.T) {
		d := t.TempDir()
		m := base(d)
		m["live_ok"] = filepath.Join(d, "config.json")
		if _, err := loadConfig(write(t, d, m)); err == nil {
			t.Fatal("live_ok was accepted while pointing at the config file " +
				"itself, which is the one path guaranteed to exist whenever " +
				"the binary runs")
		}
	})
}

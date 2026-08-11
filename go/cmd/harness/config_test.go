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
		`"live_ok":"` + filepath.Join(d, "live_ok") + `",` +
		`"stop":"` + filepath.Join(d, "harness.stop") + `"}`
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
		"live_ok":"`+filepath.Join(d, "live_ok")+`",
		"stop":"`+filepath.Join(d, "harness.stop")+`"}}`)

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
		"stop",
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
			"stop":        filepath.Join(d, "harness.stop"),
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
			"stop":        filepath.Join(d, "harness.stop"),
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

// TestLoadConfigRefusesAConfigurationItCannotFund is H-CAP-8 reaching the
// startup path (lip-lpf).
//
// `risk.CheckFundable` implemented the rule and was tested against it, and for
// the whole life of the tree its only callers were in `capital_test.go`. The
// spec is specific about both the moment and the action -- "REJECTED AT
// STARTUP ... checked as arithmetic, not discovered at fill time", and "the
// harness REFUSES TO START rather than discovering it while holding inventory"
// -- so a rule with no production caller was not a partial implementation of
// it. It was none of it.
//
// The fixture is the case that made the gap visible: rung `scale` permits S up
// to 100 contracts, and with §16's $100 capital_max and 25% reserve a single
// market at S=100 costs
//
//	1 market · 100 contracts · $0.99 = $99.00
//
// against $75 deployable. Every check the tree had before this one accepts it.
//
// What the operator got instead of a refusal was a start, and then a reducer
// silently sized to ZERO at the first fill (`quote/skew.go`: "an unfundable
// reducer is not sized") -- or an `insufficient_balance` reject, which H-CAP-5
// escalates to a global WINDING_DOWN while inventory is already held. Both are
// the "discovered at fill time" behaviour the rule exists to replace.
func TestLoadConfigRefusesAConfigurationItCannotFund(t *testing.T) {
	p := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"scale",
		"s":100,"s_max":100,"inv_soft":18,"inv_hard":36,"inv_kill":48,
		"capital_max":100,"pnl_kill":-25,"n_markets":1,`+goodTail(t)+`}`)

	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("a config whose worst permitted simultaneous fill set costs " +
			"$99.00 against $75.00 deployable was loaded without complaint. " +
			"H-CAP-8 requires the harness to refuse to start on it, and the " +
			"shortfall would instead surface as a reducer sized to zero at " +
			"the first fill")
	}
	if !strings.Contains(err.Error(), "H-CAP-8") {
		t.Fatalf("the refusal does not name the rule it enforces: %v", err)
	}

	// The other half of the claim, and the reason this is a SECOND check
	// rather than a line inside `Validate()`: the very same parameters are a
	// valid §16 set. `Validate` checks relations between knobs and there is no
	// relation here to violate -- S is positive, S_max is not below it, the
	// inventory ladder increases, capital_max is positive. The configuration is
	// internally consistent and unfundable at the same time, which is exactly
	// the state `params.go`'s doc comment says it is not in the business of
	// detecting.
	consistent := cfg.Default()
	consistent.NMarkets = 1
	consistent.S = num.QtyFromFloat(100)
	consistent.SMax = num.QtyFromFloat(100)
	consistent.CapitalMax = num.MoneyFromDollars(100)
	if err := consistent.Validate(); err != nil {
		t.Fatalf("the fixture was supposed to be a valid §16 set that is "+
			"merely unfundable, but Validate() rejects it: %v. The test no "+
			"longer proves the fundability check is load-bearing", err)
	}
}

// TestLoadConfigAcceptsTheSection103Configuration is the other direction, and
// it is not a formality.
//
// H-CAP-8 is a refusal, and a refusal wired in too broadly takes the pilot with
// it: §10.3's opening configuration is 6 markets at S=12 against $100, which
// costs
//
//	6 markets · 12 contracts · $0.99 = $71.28
//
// against $75.00 deployable -- inside the bound, by $3.72. That margin is the
// whole of the headroom the shipped parameters were derived to have (lip-afr),
// so a fundability check that rejected here would be rejecting the
// configuration the spec recommends, which is the HR-005 shape this rule was
// rewritten to escape.
func TestLoadConfigAcceptsTheSection103Configuration(t *testing.T) {
	p := writeConfig(t, `{
		"ticker":"KXTEST-A","rung":"pilot",
		"s":12,"s_max":24,"inv_soft":18,"inv_hard":36,"inv_kill":48,
		"capital_max":100,"pnl_kill":-25,`+goodTail(t)+`}`)

	c, err := loadConfig(p)
	if err != nil {
		t.Fatalf("§10.3's own opening configuration was refused as "+
			"unfundable: %v. 6 · 12 · $0.99 = $71.28 against $75.00 "+
			"deployable, so the spec would be recommending a configuration "+
			"the harness forbids", err)
	}
	// n_markets is absent from the file above, so this also pins that the
	// check runs on the OVERLAID parameters and not on the file's own fields:
	// the 6 it divides by comes from §16's default.
	if c.Params.NMarkets != 6 {
		t.Fatalf("n_markets = %d, want §16's default of 6", c.Params.NMarkets)
	}
}

// TestTheFundabilityBoundIsSection103sDerivation pins the edge rather than the
// two sides of it.
//
// §10.3 derives S = 12 as the largest quote that fits, and the derivation is
// the reason the number is 12 and not a round 10 or 15. A check that refuses
// far-out configurations while accepting one contract too many would pass both
// tests above and still leave the bound in the wrong place, so the assertion
// that matters is at S = 12 against S = 13:
//
//	6 · 12 · $0.99 = $71.28  <=  $75.00   accepted
//	6 · 13 · $0.99 = $77.22   >  $75.00   refused
//
// Both figures are written out here as literals. Deriving them by calling
// `CheckFundable` would make this test agree with the implementation by
// construction and prove nothing about where the bound actually is.
func TestTheFundabilityBoundIsSection103sDerivation(t *testing.T) {
	// `scale` for both halves: it permits S up to 100, so the rung gate is
	// silent and the ONLY difference between the two configs is S.
	body := func(t *testing.T, s int) string {
		return `{"ticker":"KXTEST-A","rung":"scale",` +
			`"s":` + fmt.Sprint(s) + `,"s_max":24,` +
			`"inv_soft":18,"inv_hard":36,"inv_kill":48,` +
			`"capital_max":100,"pnl_kill":-25,"n_markets":6,` +
			goodTail(t) + `}`
	}

	if _, err := loadConfig(writeConfig(t, body(t, 12))); err != nil {
		t.Fatalf("S=12 was refused: %v. $71.28 is inside $75.00 deployable, "+
			"and S=12 is the size §10.3 derived as the largest that fits", err)
	}

	_, err := loadConfig(writeConfig(t, body(t, 13)))
	if err == nil {
		t.Fatal("S=13 was accepted. 6 · 13 · $0.99 = $77.22 against $75.00 " +
			"deployable, so the bound is not where §10.3 put it and S=12 is " +
			"no longer the largest quote that fits")
	}
	if !strings.Contains(err.Error(), "H-CAP-8") {
		t.Fatalf("the refusal does not name the rule it enforces: %v", err)
	}
}

// TestHarnessStopPathIsRequiredAbsoluteAndDedicated is §12's out-of-band halt
// held to the same standard as H-VER-1's arming sentinel, and for the mirror
// reason.
//
// `live_ok` must be dedicated because a shared path would ARM the harness for
// reasons that have nothing to do with arming. `stop` must be dedicated because
// a shared path would HALT it for reasons that have nothing to do with halting:
// point it at the db and the harness stops adding the moment it is provisioned,
// point it at the key and it stops the moment it has credentials. A halt nobody
// requested is indistinguishable, from the outside, from a harness that does not
// work.
//
// The last subtest is the one neither sentinel's own rules would catch. They are
// OPPOSITE instructions, so a config that aliases them makes arming and halting
// the same act -- and makes `rm` both the disarm and the resume.
func TestHarnessStopPathIsRequiredAbsoluteAndDedicated(t *testing.T) {
	base := func(d string) map[string]string {
		return map[string]string{
			"db":          filepath.Join(d, "harness.db"),
			"anomaly_log": filepath.Join(d, "a.jsonl"),
			"latch":       filepath.Join(d, "h.halt"),
			"lock":        filepath.Join(d, "l.lock"),
			"key":         filepath.Join(d, "k.pem"),
			"env":         filepath.Join(d, "env"),
			"live_ok":     filepath.Join(d, "live_ok"),
			"stop":        filepath.Join(d, "harness.stop"),
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
		m["stop"] = "harness.stop"
		if _, err := loadConfig(write(t, d, m)); err == nil {
			t.Fatal("a relative stop path was accepted; it would resolve to a " +
				"different file under launchd than under the shell the operator " +
				"created it from, so the halt would be requested in one place " +
				"and looked for in another")
		}
	})

	for _, alias := range []string{"db", "latch", "key", "env"} {
		t.Run("aliased onto "+alias, func(t *testing.T) {
			d := t.TempDir()
			m := base(d)
			m["stop"] = m[alias]
			_, err := loadConfig(write(t, d, m))
			if err == nil {
				t.Fatalf("stop was accepted while pointing at paths.%s. That "+
					"file exists because the harness needs it, so §12's halt "+
					"would be requested the moment the harness was usable at "+
					"all", alias)
			}
			if !strings.Contains(err.Error(), "stop") {
				t.Fatalf("the error does not name stop: %v", err)
			}
		})
	}

	t.Run("aliased onto the config file", func(t *testing.T) {
		d := t.TempDir()
		m := base(d)
		m["stop"] = filepath.Join(d, "config.json")
		if _, err := loadConfig(write(t, d, m)); err == nil {
			t.Fatal("stop was accepted while pointing at the config file " +
				"itself, which is the one path guaranteed to exist whenever " +
				"the binary runs -- so the harness would refuse to add from " +
				"its first tick, forever")
		}
	})

	t.Run("aliased onto live_ok", func(t *testing.T) {
		d := t.TempDir()
		m := base(d)
		m["stop"] = m["live_ok"]
		_, err := loadConfig(write(t, d, m))
		if err == nil {
			t.Fatal("stop and live_ok were accepted as the SAME file.\n\n" +
				"They are opposite instructions. Arming the harness would be " +
				"the same act as halting it, and disarming it by removing the " +
				"file would be the same act as lifting the halt -- so the " +
				"operator's two controls would each undo the other")
		}
		if !strings.Contains(err.Error(), "live_ok") ||
			!strings.Contains(err.Error(), "stop") {
			t.Fatalf("the error does not name both sentinels: %v", err)
		}
	})
}

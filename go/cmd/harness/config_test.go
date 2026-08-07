package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// goodPaths is an absolute, complete path block. Tests that are not about paths
// use it so a path error cannot be mistaken for the failure under test.
func goodPaths(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	return `"paths":{` +
		`"db":"` + filepath.Join(d, "harness.db") + `",` +
		`"anomaly_log":"` + filepath.Join(d, "anomaly.jsonl") + `",` +
		`"latch":"` + filepath.Join(d, "harness.halt") + `",` +
		`"lock":"` + filepath.Join(d, "harness.lock") + `",` +
		`"key":"` + filepath.Join(d, "kalshi.pem") + `",` +
		`"env":"` + filepath.Join(d, "env") + `"}`
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
		"capital_max":100,"pnl_kill":-25,`+goodPaths(t)+`}`)

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
		"inv_kil":48,`+goodPaths(t)+`}`)

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
		"ticker":"KXTEST-A","rung":"canary","s":12,`+goodPaths(t)+`}`)

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
		"ticker":"KXTEST-A","rung":"pilot","s":12,`+goodPaths(t)+`}`)
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
		"env":"`+filepath.Join(d, "env")+`"}}`)

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
		"db", "anomaly_log", "latch", "lock", "key", "env",
	} {
		d := t.TempDir()
		all := map[string]string{
			"db":          filepath.Join(d, "harness.db"),
			"anomaly_log": filepath.Join(d, "a.jsonl"),
			"latch":       filepath.Join(d, "h.halt"),
			"lock":        filepath.Join(d, "l.lock"),
			"key":         filepath.Join(d, "k.pem"),
			"env":         filepath.Join(d, "env"),
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
	p := writeConfig(t, `{"rung":"canary","s":1,`+goodPaths(t)+`}`)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("a config naming no ticker was accepted")
	}
}

// TestUnnamedRungIsRefused. Capital is a ladder rather than a decision, and a
// run that does not say which step it is on has not made the decision.
func TestUnnamedRungIsRefused(t *testing.T) {
	p := writeConfig(t, `{"ticker":"KXTEST-A","s":1,`+goodPaths(t)+`}`)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("a config naming no rung was accepted")
	}
	bad := writeConfig(t, `{"ticker":"KXTEST-A","rung":"enormous","s":1,`+
		goodPaths(t)+`}`)
	if _, err := loadConfig(bad); err == nil {
		t.Fatal("an unknown rung was accepted")
	}
}

// TestUnsetKnobsKeepTheSpecDefault. The file names DEVIATIONS from §16, so a
// diff of it against an empty one is the whole config review.
func TestUnsetKnobsKeepTheSpecDefault(t *testing.T) {
	p := writeConfig(t, `{"ticker":"KXTEST-A","rung":"canary","s":1,`+
		goodPaths(t)+`}`)
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

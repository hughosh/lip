package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lip/harness/num"
)

// fixtureRung is the ladder step every install below has to assert.
//
// `newProvisionConfig` is `cfg.Default()`, which is S=12, and it declares rung
// `pilot`. So the fixture is an ABOVE-CANARY config, and until lip-3yo every
// test in this file installed a plist whose argv `checkRung` refuses -- a job
// that could never have started once, restarted forever by `KeepAlive`. The
// flag is spelled out at each call site rather than defaulted inside
// `newDeployFixture`, because a fixture that supplied the operator's assertion
// for free would be a fixture in which forgetting it is untestable.
const fixtureRung = "pilot"

// newDeployFixture is a provisioned store, a config file and an agent
// directory, all under t.TempDir().
//
// The agent directory is a temp directory and NEVER `~/Library/LaunchAgents`.
// `agentOptions.Dir` exists for this: a test that wrote into the real one would
// install a launchd job on the machine running the test suite, and launchd
// scans that directory.
func newDeployFixture(t *testing.T) (config, string, string) {
	t.Helper()
	c := newProvisionConfig(t)
	var out bytes.Buffer
	if err := provision(c, &out); err != nil {
		t.Fatalf("provision: %v", err)
	}
	configPath := filepath.Join(filepath.Dir(c.Paths.DB), "pilot.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return c, configPath, t.TempDir()
}

// programArguments pulls the ProgramArguments array out of a rendered plist.
func programArguments(t *testing.T, plist string) []string {
	t.Helper()
	const key = "<key>ProgramArguments</key>"
	i := strings.Index(plist, key)
	if i < 0 {
		t.Fatalf("the plist has no ProgramArguments:\n%s", plist)
	}
	rest := plist[i+len(key):]
	j := strings.Index(rest, "</array>")
	if j < 0 {
		t.Fatalf("ProgramArguments is not an array:\n%s", plist)
	}
	re := regexp.MustCompile(`<string>(.*?)</string>`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(rest[:j], -1) {
		out = append(out, m[1])
	}
	return out
}

// argValue returns the single value following `flag` in a rendered argv.
//
// It fails on a repeat rather than taking the first or the last. `flag` accepts
// a repeated option silently and keeps the last one, so an argv that
// accumulated a second `-config` would start a harness against a file the
// operator did not read out of the plist -- and a helper that returned either
// end of that would report the plist as correct.
func argValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	found := ""
	n := 0
	for i, a := range argv {
		if a != flag {
			continue
		}
		if i+1 >= len(argv) {
			t.Fatalf("%s is the last element of %v, so it names no value",
				flag, argv)
		}
		found = argv[i+1]
		n++
	}
	if n != 1 {
		t.Fatalf("%s appears %d times in %v, want exactly 1", flag, n, argv)
	}
	return found
}

// TestInstalledJobExecsCaffeinateAndKeepsAlive is H-DEP-3 and H-DEP-2 read back
// off the file that was actually written.
//
// `lifecycle` already tests `Command()` and `Render()` as pure functions. What
// this asserts is the thing rendering separately from installing makes possible
// to get wrong: that the bytes on disk are those bytes. A plist whose argv does
// not begin `/usr/bin/caffeinate -is` is a harness with no power assertion, and
// the Mac idle-sleeps -- a sleep silently voids everything (F7), and the gap
// biases every measurement taken across it without leaving a mark.
func TestInstalledJobExecsCaffeinateAndKeepsAlive(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)

	var out bytes.Buffer
	if err := installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung}, &out); err != nil {
		t.Fatalf("installAgent: %v", err)
	}

	path := filepath.Join(dir, agentLabel+".plist")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed plist: %v", err)
	}
	plist := string(raw)

	argv := programArguments(t, plist)
	if len(argv) < 4 {
		t.Fatalf("ProgramArguments is %v; want at least caffeinate, -is, the "+
			"harness and its config", argv)
	}
	if argv[0] != "/usr/bin/caffeinate" {
		t.Fatalf("ProgramArguments[0] is %q, want /usr/bin/caffeinate. "+
			"H-DEP-3 makes caffeinate exec the harness so the power assertion "+
			"lives exactly as long as the process; anything else supervises a "+
			"wrapper and asserts nothing about the harness", argv[0])
	}
	if argv[1] != "-is" {
		t.Fatalf("ProgramArguments[1] is %q, want -is. `-i` alone leaves the "+
			"machine free to sleep on the power-management path that fires "+
			"overnight on this host (F7)", argv[1])
	}
	exe, err := harnessExecutable()
	if err != nil {
		t.Fatalf("harnessExecutable: %v", err)
	}
	if argv[2] != exe {
		t.Fatalf("ProgramArguments[2] is %q, want this process's resolved "+
			"executable %q", argv[2], exe)
	}
	if argv[3] != "-config" || argv[4] != configPath {
		t.Fatalf("the job is started as %v; launchd inherits no working "+
			"directory, so the config has to be named absolutely in the argv",
			argv)
	}

	const keepAlive = "<key>KeepAlive</key>"
	k := strings.Index(plist, keepAlive)
	if k < 0 {
		t.Fatalf("the installed plist has no KeepAlive. H-DEP-2 restarts the "+
			"harness on any exit, including exit 0: a drained harness exits "+
			"cleanly, and what stops it coming back up is the durable latch on "+
			"disk, not a supervisor that declined to restart it:\n%s", plist)
	}
	if after := strings.TrimSpace(plist[k+len(keepAlive):]); !strings.HasPrefix(
		after, "<true>") {
		t.Fatalf("KeepAlive is not true; it is followed by %.20q", after)
	}

	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat installed plist: %v", err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("the installed plist is mode %#o, want 0644",
			info.Mode().Perm())
	}

	// Nothing was started, and the operator is told what to run.
	for _, want := range []string{"launchctl bootstrap", "launchctl kickstart"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the install report does not print %q, so the operator "+
				"has no record of what loading the job requires:\n%s",
				want, out.String())
		}
	}
}

// TestInstallRefusesToOverwriteAnInstalledJobWithoutForce.
//
// A plist rewritten in place changes what launchd starts on the NEXT bootstrap
// and changes nothing `launchctl print` reports about the job already loaded.
// The operator who edited the config, re-installed, and saw no error has every
// reason to believe the change took effect -- and the harness goes on quoting
// the previous market with the previous sizing until something restarts it.
func TestInstallRefusesToOverwriteAnInstalledJobWithoutForce(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)
	path := filepath.Join(dir, agentLabel+".plist")

	var out bytes.Buffer
	if err := installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung}, &out); err != nil {
		t.Fatalf("first install: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed plist: %v", err)
	}

	// A second install with a DIFFERENT config, so a silent overwrite is
	// detectable in the bytes rather than only in the return value.
	otherConfig := filepath.Join(filepath.Dir(c.Paths.DB), "other.json")
	if err := os.WriteFile(otherConfig, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write second config: %v", err)
	}

	var second bytes.Buffer
	err = installAgent(c, otherConfig, agentOptions{Dir: dir, Rung: fixtureRung}, &second)
	if err == nil {
		t.Fatalf("installing over the job already at %s SUCCEEDED without a "+
			"force flag", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the refusal does not name the plist it refused: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read installed plist: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the refused install rewrote the plist anyway")
	}

	// With force it replaces, and the replacement is the new config.
	var forced bytes.Buffer
	if err := installAgent(c, otherConfig,
		agentOptions{Dir: dir, Rung: fixtureRung, Force: true}, &forced); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	argv := programArguments(t, func() string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read replaced plist: %v", err)
		}
		return string(raw)
	}())
	if got := argValue(t, argv, "-config"); got != otherConfig {
		t.Fatalf("after a forced install the job still names %q, want %q",
			got, otherConfig)
	}
}

// TestAFailedPlistWriteLeavesThePreviousJobIntact is the atomicity claim.
//
// A plist truncated by a failed write is not merely a bad file: launchd refuses
// to load it, so the job it describes has no KeepAlive and no caffeinate. The
// supervision is ABSENT rather than wrong, which is the failure nobody notices.
// The write therefore lands on a temp file in the same directory and arrives by
// rename, and a failure leaves the previous job byte-identical -- and leaves no
// debris behind either, because launchd scans the directory it would be in.
func TestAFailedPlistWriteLeavesThePreviousJobIntact(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)
	path := filepath.Join(dir, agentLabel+".plist")

	var out bytes.Buffer
	if err := installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung}, &out); err != nil {
		t.Fatalf("first install: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed plist: %v", err)
	}

	// A SHORT write followed by an error, which is the shape a full disk
	// produces. Failing before delegating would be an atomic-nothing no real
	// file ever does.
	boom := errors.New("no space left on device")
	original := plistWriteFn
	plistWriteFn = func(f *os.File, b []byte) (int, error) {
		n, err := f.Write(b[:len(b)/2])
		if err != nil {
			return n, err
		}
		return n, boom
	}
	t.Cleanup(func() { plistWriteFn = original })

	var failed bytes.Buffer
	err = installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung, Force: true},
		&failed)
	if err == nil {
		t.Fatal("a failing write reported a successful install")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the install error does not carry the disk's error: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the previous plist is gone after a failed install: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the failed install damaged the installed job: %d bytes "+
			"before, %d after. launchd refuses a malformed plist, so the job "+
			"would have no KeepAlive and no caffeinate at all",
			len(before), len(after))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read agent dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != agentLabel+".plist" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the failed install left %v in the LaunchAgents directory; "+
			"launchd scans it", names)
	}
}

// TestInstallRefusesAJobWhoseStoreDoesNotExist.
//
// KeepAlive is "restart on any exit, including exit 0" (H-DEP-2), so a job
// installed against an unprovisioned path is not a job that fails once. It is a
// respawn loop against `requireExistingDB`'s refusal, at whatever rate launchd
// throttles to, reported by nothing -- the anomaly journal needs the harness to
// be running to be written to.
func TestInstallRefusesAJobWhoseStoreDoesNotExist(t *testing.T) {
	c := newProvisionConfig(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "pilot.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	var out bytes.Buffer
	err := installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung}, &out)
	if err == nil {
		t.Fatalf("installed a KeepAlive job against the unprovisioned store %s",
			c.Paths.DB)
	}
	if _, statErr := os.Stat(filepath.Join(dir,
		agentLabel+".plist")); statErr == nil {
		t.Fatal("a plist was written despite the refusal")
	}
}

// TestInstallRefusesARelativeConfigPath.
//
// launchd runs the job with no shell and no inherited working directory, so a
// relative path in the argv resolves against something neither the operator nor
// this code chose -- and the file it would resolve to names the durable halt
// latch (H-HALT-4).
func TestInstallRefusesARelativeConfigPath(t *testing.T) {
	c, _, dir := newDeployFixture(t)
	var out bytes.Buffer
	if err := installAgent(c, "pilot.json", agentOptions{Dir: dir, Rung: fixtureRung},
		&out); err == nil {
		t.Fatal("a relative config path was accepted into a launchd argv")
	}
}

// TestReadOnlyDeployNeverCarriesLive is the default that matters most (H-VER-1).
//
// A deployed job is the one invocation nobody watches start. The plist outlives
// the session that wrote it, `KeepAlive` restarts it forever, and the argv in it
// is what runs at 3am after a reboot. So `-deploy` alone installs a READ-ONLY
// job, and arming a deployed job has to be a separate sentence the operator
// typed.
//
// It is never inferred from the sentinel being present at install time.
// `M-ES6-DEPLOYARM` arms every deployed job, which is the shape this would take
// if someone decided the flag was redundant with the file.
func TestReadOnlyDeployNeverCarriesLive(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)

	// The sentinel EXISTS while the deploy runs. A job armed by inference would
	// pass a test that forgot this line.
	if err := os.WriteFile(c.Paths.LiveOK, []byte("armed\n"), 0o600); err != nil {
		t.Fatalf("sentinel: %v", err)
	}

	var out bytes.Buffer
	if err := installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung}, &out); err != nil {
		t.Fatalf("installAgent: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, agentLabel+".plist"))
	if err != nil {
		t.Fatalf("reading the installed plist: %v", err)
	}
	for _, a := range programArguments(t, string(body)) {
		if a == "-live" {
			t.Fatal("a plain -deploy installed a job carrying -live. The " +
				"sentinel happened to exist when the plist was written, and " +
				"that is a runtime key an operator creates and removes " +
				"freely -- baking it into an argv that starts for months is " +
				"exactly the inference H-VER-1 forbids")
		}
	}
}

// TestExplicitLiveDeployCarriesLiveExactlyOnce is the other half: when the
// operator does say so, the flag appears, and it appears ONCE.
//
// Once matters because `flag` accepts a repeated boolean silently, so a
// duplicate would never be reported -- and an argv that accumulated a `-live`
// per deploy would be a plist nobody could read confidently.
func TestExplicitLiveDeployCarriesLiveExactlyOnce(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)

	var out bytes.Buffer
	if err := installAgent(c, configPath,
		agentOptions{Dir: dir, Rung: fixtureRung, Live: true}, &out); err != nil {
		t.Fatalf("installAgent: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, agentLabel+".plist"))
	if err != nil {
		t.Fatalf("reading the installed plist: %v", err)
	}

	args := programArguments(t, string(body))
	n := 0
	for _, a := range args {
		if a == "-live" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the deployed argv carries %d -live flags, want exactly 1: %v",
			n, args)
	}
	// And `-live` is LAST, after the config and the ladder step. The order is
	// asserted literally rather than by index arithmetic off the end, which is
	// what this used to do and what silently stopped meaning anything the
	// moment a flag was inserted before it.
	if args[len(args)-1] != "-live" {
		t.Fatalf("the deployed argv does not end with -live: %v", args)
	}
	if got := argValue(t, args, "-config"); got != configPath {
		t.Fatalf("the deployed argv names config %q, want %q", got, configPath)
	}
}

// ---------------------------------------------------------------------------
// lip-3yo: the deployed argv carries the operator's ladder assertion
// ---------------------------------------------------------------------------
//
// `checkRung` refuses every start whose S exceeds the canary's one contract
// unless `-rung <name>` names the same step the config declares. The deployed
// argv did not carry one. That is not a job that starts wrong -- it is a job
// that cannot start at all, restarted forever by `KeepAlive`, at whatever rate
// launchd throttles to, with the refusal going to `harness.err` and to nothing
// that watches.

// newCanaryDeployFixture is `newDeployFixture` resized to the canary.
//
// Both fields move together and neither is optional: `config.go` asserts a
// config's declared rung against its own sizing, so an S=1 file that still said
// `pilot` would be a config the loader accepts but no operator would write, and
// a test built on one would be describing a tree that cannot exist.
func newCanaryDeployFixture(t *testing.T) (config, string, string) {
	t.Helper()
	c, configPath, dir := newDeployFixture(t)
	c.Params.S = num.QtyFromFloat(1)
	c.Rung = rungs["canary"]
	return c, configPath, dir
}

// harnessArgv is the deployed ProgramArguments with H-DEP-3's caffeinate prefix
// removed -- that is, the argv the harness itself is parsed from.
//
// `TestInstalledJobExecsCaffeinateAndKeepsAlive` is what pins the three
// elements dropped here; this asserts them again only so a change to the prefix
// surfaces as a clear failure rather than as an off-by-one in every test below.
func harnessArgv(t *testing.T, argv []string) []string {
	t.Helper()
	if len(argv) < 3 || argv[0] != "/usr/bin/caffeinate" || argv[1] != "-is" {
		t.Fatalf("ProgramArguments does not begin `/usr/bin/caffeinate -is "+
			"<harness>`: %v", argv)
	}
	return argv[3:]
}

// TestDeployRefusesAnAboveCanaryConfigWithNoRung.
//
// The refusal is measured in BYTES and not in a return value. "Refuses before
// changing a plist" is a claim about the file: an implementation that rendered,
// wrote, and then validated would return an error here too, and would have
// already replaced a working job with one that cannot start.
//
// `-force` is set deliberately, so the overwrite guard is not what refuses and
// the only remaining reason is the missing ladder assertion.
func TestDeployRefusesAnAboveCanaryConfigWithNoRung(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)
	path := filepath.Join(dir, agentLabel+".plist")

	var out bytes.Buffer
	if err := installAgent(c, configPath,
		agentOptions{Dir: dir, Rung: fixtureRung}, &out); err != nil {
		t.Fatalf("first install: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed plist: %v", err)
	}

	var second bytes.Buffer
	err = installAgent(c, configPath,
		agentOptions{Dir: dir, Force: true}, &second)
	if err == nil {
		t.Fatalf("deploying the S=%s config with no rung asserted installed a "+
			"job. `checkRung` refuses that argv at every start, and KeepAlive "+
			"restarts on every exit, so what was installed is a refusal loop",
			c.Params.S.Wire())
	}
	var ref *refusal
	if !errors.As(err, &ref) {
		t.Fatalf("a missing ladder assertion reported an operational failure "+
			"rather than a refusal, which is the distinction exit codes 1 and "+
			"2 exist to draw: %v", err)
	}
	if !strings.Contains(err.Error(), "-rung") {
		t.Fatalf("the deploy was refused for a reason that does not name "+
			"-rung, so this test would pass on a tree where the ladder gate "+
			"is absent: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the refused install removed the previous job: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the refused install rewrote the plist anyway: %d bytes "+
			"before, %d after", len(before), len(after))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read agent dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the refused install left %d files in the LaunchAgents "+
			"directory; launchd scans it", len(entries))
	}
}

// TestDeployRefusesARungThatDisagreesWithTheConfig.
//
// The config declares what the run IS and the flag declares what the operator
// believes they are installing, so a disagreement is a deployment where one of
// the two beliefs is wrong -- and it is never resolved by preferring either.
// Each case is checked for a plist, because the argument for refusing at all is
// that nothing gets written.
func TestDeployRefusesARungThatDisagreesWithTheConfig(t *testing.T) {
	cases := []struct {
		name string
		rung string
	}{
		{"a later step than the config declares", "second"},
		{"an earlier step than the config declares", "canary"},
		{"a step that is not on the ladder at all", "pilto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, configPath, dir := newDeployFixture(t)
			var out bytes.Buffer
			err := installAgent(c, configPath,
				agentOptions{Dir: dir, Rung: tc.rung}, &out)
			if err == nil {
				t.Fatalf("-rung %s was accepted for a config declaring %q",
					tc.rung, c.Rung.name)
			}
			var ref *refusal
			if !errors.As(err, &ref) {
				t.Fatalf("a disagreeing rung reported a failure rather than a "+
					"refusal: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir,
				agentLabel+".plist")); statErr == nil {
				t.Fatal("a plist was written despite the refusal")
			}
		})
	}
}

// TestPilotDeployCarriesTheConfigAndRungInStableOrder.
//
// The expected argv is written out as a LITERAL. Building it by calling
// `agentArgs` would make this test agree with whatever that function does,
// including nothing at all -- which is precisely the shape lip-7zt's
// `M-7ZT-SHORTTTL` survived under, a test expressed in the units of the thing
// it was meant to pin.
func TestPilotDeployCarriesTheConfigAndRungInStableOrder(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)

	var out bytes.Buffer
	if err := installAgent(c, configPath,
		agentOptions{Dir: dir, Rung: "pilot"}, &out); err != nil {
		t.Fatalf("installAgent: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, agentLabel+".plist"))
	if err != nil {
		t.Fatalf("reading the installed plist: %v", err)
	}

	got := harnessArgv(t, programArguments(t, string(body)))
	want := []string{"-config", configPath, "-rung", "pilot"}
	if len(got) != len(want) {
		t.Fatalf("the deployed harness argv is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the deployed harness argv is %v, want %v", got, want)
		}
	}

	// And the path is the absolute one, because launchd inherits no working
	// directory and the file this names carries the halt latch's location.
	if !filepath.IsAbs(got[1]) {
		t.Fatalf("the deployed argv names the config relatively: %q", got[1])
	}

	// The plist summary the operator reads has to say the same thing the file
	// says. A summary that omitted the flag would be the report under which
	// this defect went unnoticed in the first place.
	if !strings.Contains(out.String(), "-rung pilot") {
		t.Fatalf("the install report does not show the rung it wrote:\n%s",
			out.String())
	}

	// And the armed argv, which is the only one where the order of the two
	// flags is observable at all. `-live` stays last: it is the flag that
	// decides whether orders leave the process, and an operator scanning a
	// plist they installed months ago should find it in one place.
	var armed bytes.Buffer
	if err := installAgent(c, configPath,
		agentOptions{Dir: dir, Rung: "pilot", Live: true, Force: true},
		&armed); err != nil {
		t.Fatalf("armed install: %v", err)
	}
	body, err = os.ReadFile(filepath.Join(dir, agentLabel+".plist"))
	if err != nil {
		t.Fatalf("reading the replaced plist: %v", err)
	}
	got = harnessArgv(t, programArguments(t, string(body)))
	want = []string{"-config", configPath, "-rung", "pilot", "-live"}
	if len(got) != len(want) {
		t.Fatalf("the armed harness argv is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the armed harness argv is %v, want %v", got, want)
		}
	}
}

// TestTheDeployedArgvStartsUnderTheRungGate is the anti-refusal-loop test, and
// it is the one this bead exists for.
//
// It goes all the way round: install, read the bytes back off disk, parse them
// with the flag set `main` registers, and put the parsed value through the same
// `checkRung` a real start runs. Nothing here is compared against `agentArgs`
// or against a rung the test chose -- the question is whether the file that was
// written starts, and only the production parser and the production gate can
// answer it.
func TestTheDeployedArgvStartsUnderTheRungGate(t *testing.T) {
	c, configPath, dir := newDeployFixture(t)

	var out bytes.Buffer
	if err := installAgent(c, configPath,
		agentOptions{Dir: dir, Rung: fixtureRung}, &out); err != nil {
		t.Fatalf("installAgent: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, agentLabel+".plist"))
	if err != nil {
		t.Fatalf("reading the installed plist: %v", err)
	}

	fs, cl := newFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	argv := harnessArgv(t, programArguments(t, string(body)))
	if err := fs.Parse(argv); err != nil {
		t.Fatalf("the deployed argv %v does not parse: %v", argv, err)
	}

	if cl.configPath != configPath {
		t.Fatalf("the deployed argv starts the harness against %q, want %q",
			cl.configPath, configPath)
	}
	if err := checkRung(c, cl.rung); err != nil {
		t.Fatalf("the plist this deploy wrote is REFUSED by the binary it "+
			"starts: %v\n\nKeepAlive restarts on every exit, so this is not a "+
			"job that fails once. It is a refusal loop at whatever rate "+
			"launchd throttles to, writing to harness.err, watched by "+
			"nothing -- and the anomaly journal cannot record it because the "+
			"journal needs the harness to be running", err)
	}
	if cl.live {
		t.Fatal("a plain -deploy produced an argv that parses as armed")
	}
}

// TestCanaryDeployRendersExactlyWhatTheOperatorAsserted.
//
// The canary is the one rung an unadorned invocation gets, so a canary deploy
// with nothing asserted must render nothing -- and a deploy that DID assert
// `canary` must render that. Both halves are here because the failure they
// guard against is one implementation: filling the flag in from `c.Rung`.
//
// That implementation is attractive and it is wrong. It passes every test that
// only ever checks `checkRung` succeeds, because a rung copied out of the
// config can never disagree with the config. What it destroys is the second
// assertion -- the operator's -- which is the entire reason pilot-plan §1 makes
// the knob something you say out loud at the moment of starting.
// `M-3YO-DERIVEDRUNG` is that implementation.
func TestCanaryDeployRendersExactlyWhatTheOperatorAsserted(t *testing.T) {
	c, configPath, dir := newCanaryDeployFixture(t)
	plist := filepath.Join(dir, agentLabel+".plist")

	var out bytes.Buffer
	if err := installAgent(c, configPath, agentOptions{Dir: dir}, &out); err != nil {
		t.Fatalf("unadorned canary install: %v", err)
	}
	body, err := os.ReadFile(plist)
	if err != nil {
		t.Fatalf("reading the installed plist: %v", err)
	}
	got := harnessArgv(t, programArguments(t, string(body)))
	want := []string{"-config", configPath}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("an unadorned canary deploy rendered %v, want %v. A rung "+
			"that appears without the operator having typed one was copied "+
			"out of the config, and a copy cannot disagree with its source",
			got, want)
	}

	// The unadorned canary is exactly what `checkRung` lets through with no
	// flag, so the job starts.
	if err := checkRung(c, ""); err != nil {
		t.Fatalf("the unadorned canary argv does not start: %v", err)
	}

	// And when the operator DOES say canary, that is what is written.
	var forced bytes.Buffer
	if err := installAgent(c, configPath,
		agentOptions{Dir: dir, Rung: "canary", Force: true}, &forced); err != nil {
		t.Fatalf("explicit canary install: %v", err)
	}
	body, err = os.ReadFile(plist)
	if err != nil {
		t.Fatalf("reading the replaced plist: %v", err)
	}
	got = harnessArgv(t, programArguments(t, string(body)))
	want = []string{"-config", configPath, "-rung", "canary"}
	if len(got) != len(want) {
		t.Fatalf("an explicit canary deploy rendered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("an explicit canary deploy rendered %v, want %v", got, want)
		}
	}
	if err := checkRung(c, "canary"); err != nil {
		t.Fatalf("the explicit canary argv does not start: %v", err)
	}
}

// TestDeployOptionsCarryTheOperatorsRung pins the wiring point.
//
// `run`'s `-deploy` arm cannot be driven from a test: it builds its options
// with no `Dir`, which is `~/Library/LaunchAgents`, so a test that called it
// would install a launchd job on the machine running the suite. The arm is
// therefore one expression, and this asserts that expression carries every
// thing the operator typed -- including the `rung` that the arm had in scope
// and dropped on the floor for the whole life of the defect.
func TestDeployOptionsCarryTheOperatorsRung(t *testing.T) {
	opts := deployOptions("pilot", true, true)
	if opts.Rung != "pilot" {
		t.Fatalf("deployOptions dropped the rung: %+v", opts)
	}
	if !opts.Force || !opts.Live {
		t.Fatalf("deployOptions dropped -force or -live: %+v", opts)
	}
	if opts.Dir != "" {
		t.Fatalf("deployOptions invented the agent directory %q. Empty means "+
			"~/Library/LaunchAgents, and only a test ever sets it", opts.Dir)
	}

	// The unadorned case is the read-only canary deploy, and it must stay the
	// zero value in every field.
	if got := deployOptions("", false, false); got != (agentOptions{}) {
		t.Fatalf("an unadorned -deploy built %+v, want the zero options", got)
	}
}

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
	if err := installAgent(c, configPath, agentOptions{Dir: dir}, &out); err != nil {
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
	if err := installAgent(c, configPath, agentOptions{Dir: dir}, &out); err != nil {
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
	err = installAgent(c, otherConfig, agentOptions{Dir: dir}, &second)
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
		agentOptions{Dir: dir, Force: true}, &forced); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	argv := programArguments(t, func() string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read replaced plist: %v", err)
		}
		return string(raw)
	}())
	if argv[len(argv)-1] != otherConfig {
		t.Fatalf("after a forced install the job still names %q, want %q",
			argv[len(argv)-1], otherConfig)
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
	if err := installAgent(c, configPath, agentOptions{Dir: dir}, &out); err != nil {
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
	err = installAgent(c, configPath, agentOptions{Dir: dir, Force: true},
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
	err := installAgent(c, configPath, agentOptions{Dir: dir}, &out)
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
	if err := installAgent(c, "pilot.json", agentOptions{Dir: dir},
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
	if err := installAgent(c, configPath, agentOptions{Dir: dir}, &out); err != nil {
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
		agentOptions{Dir: dir, Live: true}, &out); err != nil {
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
	// And it still says which config, in the same stable order.
	if len(args) < 2 || args[len(args)-3] != "-config" {
		t.Fatalf("the argv is not `-config <path> -live`: %v", args)
	}
}

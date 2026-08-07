package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"lip/harness/lifecycle"
)

// `lifecycle.LaunchdPlan` RENDERS the plist and writes nothing -- its own
// comment says installation is this file's job, and says why: rendering
// separately is what lets `M-L-KEEPALIVE` and `M-L-CAFFEINATE` mutate a pure
// function whose output a test reads, where a plist written straight to
// `~/Library/LaunchAgents` would be a side effect no gate inspects.
//
// So this file does the part that touches the disk, and it stops there. It
// does NOT run `launchctl`. Loading the job starts a process that quotes a
// live account with real credentials, and that is an operator decision rather
// than a consequence of having installed a file -- the same argument
// `provision.go` makes about creating a database. The commands are printed for
// the operator to run.
//
// Two refusals here exist because of what `KeepAlive: true` does with a job
// that cannot start (H-DEP-2 -- "restart on any exit, including exit 0"). A
// plist naming a missing `caffeinate`, or a harness whose store has not been
// provisioned, produces a job that launchd faithfully respawns forever. Nothing
// in this system reports that: the harness's own anomaly journal needs the
// harness to be up.

// agentLabel is the launchd job's identity, and there is exactly one of it.
//
// Not derived from the ticker, deliberately. H-DEP-5 is "one instance only ...
// two harnesses on one account is an unrecoverable position-model conflict",
// and per-market labels are how two jobs come to exist -- at which point the
// PID lockfile is the only thing left standing between them, and it discovers
// the conflict after launchd has already started the second process. One label
// means the second market is a config change to one job, and the
// refuse-to-clobber below is where the operator is told.
const agentLabel = "com.lip.harness"

// plistMode is 0644. launchd reads the file as the user and refuses a plist
// that is group- or world-WRITABLE, which is the only permission bit it cares
// about here; there is no secret in a plist, and the credentials it points the
// harness at are named by path (H-DEP-6) and never inlined.
const plistMode = 0o644

// agentDirMode matches what macOS itself creates `~/Library/LaunchAgents` with.
const agentDirMode = 0o700

// plistWriteFn is the write step, as a seam.
//
// Same argument `hstore.journalWriteFn` makes: the failure this file has to be
// correct under -- a write that fails partway, leaving a truncated plist -- is
// not producible on demand from a real filesystem, and a wrapper that failed
// BEFORE delegating would test an atomic-nothing that no disk ever does.
// `TestAFailedPlistWriteLeavesThePreviousJobIntact` drives it.
var plistWriteFn = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// agentOptions are the two things about an install that are not derived.
type agentOptions struct {
	// Dir is where the plist is written. Empty means
	// `~/Library/LaunchAgents`, which is the only value production ever uses;
	// it is a field so that a test writes into `t.TempDir()` and can never
	// install a real launchd job as a side effect of running.
	Dir string
	// Force replaces an installed plist. Off by default -- see installPlist.
	Force bool
}

// installAgent renders the H-DEP-2/H-DEP-3 plist and installs it.
//
// `configPath` is an argument because `config` does not carry the path it was
// loaded from, and the job has to name one: launchd starts the harness with no
// shell, no inherited working directory and no environment worth relying on,
// so every path in the argv is absolute or it is wrong.
func installAgent(c config, configPath string, opts agentOptions,
	out io.Writer) error {

	if !filepath.IsAbs(configPath) {
		return fmt.Errorf("the config path %q is relative; launchd runs the "+
			"job with no inherited working directory, so this would resolve "+
			"against something neither the operator nor this code chose",
			configPath)
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("the config %s the job would be started with cannot "+
			"be read: %w", configPath, err)
	}

	// The store, before the plist. `KeepAlive: true` restarts on any exit, so a
	// job installed against an unprovisioned path is not a job that fails once
	// -- it is a respawn loop against `requireExistingDB`'s refusal, running at
	// whatever rate launchd throttles to, reported by nothing.
	if err := requireExistingDB(c.Paths.DB); err != nil {
		return err
	}
	if err := requireCaffeinate(); err != nil {
		return err
	}

	exe, err := harnessExecutable()
	if err != nil {
		return err
	}

	// H-DEP-4: stdout and stderr go to a file, and the file is not the record
	// -- `harness.db` is. They live beside the database because that directory
	// is the one the operator already has to know about, and it is the one
	// `provision` created with a mode that suits what ends up in these.
	logDir := filepath.Dir(c.Paths.DB)
	if info, err := os.Stat(logDir); err != nil {
		return fmt.Errorf("the directory %s that would hold the job's stdout "+
			"and stderr cannot be read: %w", logDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory, so launchd has nowhere to "+
			"put the job's stdout and stderr", logDir)
	}

	plan := lifecycle.LaunchdPlan{
		Label:      agentLabel,
		Executable: exe,
		Args:       []string{"-config", configPath},
		WorkingDir: logDir,
		StdoutPath: filepath.Join(logDir, "harness.out"),
		StderrPath: filepath.Join(logDir, "harness.err"),
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	body, err := plan.Render()
	if err != nil {
		return err
	}

	dir := opts.Dir
	if dir == "" {
		if dir, err = userLaunchAgentsDir(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, agentDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, agentLabel+".plist")
	if err := installPlist(path, body, opts.Force); err != nil {
		return err
	}
	return writeInstallReport(out, path, plan)
}

// requireCaffeinate is H-DEP-3, checked before anything is written.
//
// > the launchd job execs `/usr/bin/caffeinate -is <harness>`, so the assertion
// > lives exactly as long as the process. The Mac idle-sleeps and a sleep
// > silently voids everything (F7).
//
// `lifecycle.LaunchdPlan.Command` pins that argv, so `caffeinate` is argv[0] of
// the job and there is no fallback: if the binary is missing or not executable,
// launchd cannot exec the job at all. The result is a harness that never
// starts, and the way that is discovered today is by noticing, hours later,
// that no records are being written -- which is indistinguishable from a
// market that never opened.
func requireCaffeinate() error {
	const path = "/usr/bin/caffeinate"
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist.\n\n"+
			"H-DEP-3 makes it argv[0] of the job so the power assertion lives "+
			"exactly as long as the harness process. Without it launchd cannot "+
			"exec the job at all, and a job that never starts is reported by "+
			"nothing -- the anomaly journal needs the harness to be running to "+
			"be written to. Refusing to install a plist that names it", path)
	}
	if err != nil {
		return fmt.Errorf("%s could not be examined: %w", path, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable (mode %#o); the job's argv[0] "+
			"cannot be exec'd, so H-DEP-3's power assertion would never be "+
			"taken and the host would idle-sleep out from under a running "+
			"harness (F7)", path, info.Mode().Perm())
	}
	return nil
}

// harnessExecutable is the path launchd will exec, resolved.
//
// `os.Executable` and then `filepath.EvalSymlinks`, because launchd stores the
// literal argv it was given. A plist naming a symlink supervises whatever that
// symlink points at NEXT -- so a deploy that replaced the link would silently
// change which binary a job installed months earlier runs, and `launchctl`
// would go on reporting the same job. Resolving here records the artefact this
// process actually is.
func harnessExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("this process cannot determine its own "+
			"executable, so there is nothing to tell launchd to supervise: %w",
			err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", exe, err)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("this process's executable resolves to %q, "+
			"which is relative; launchd would resolve it against a working "+
			"directory nobody chose", resolved)
	}
	return resolved, nil
}

// userLaunchAgentsDir is the per-user agent directory.
//
// gui/$UID and not a system daemon: the harness runs as the operator, reads
// credentials from that operator's home (H-DEP-6), and needs a login session's
// keychain and network. A LaunchDaemon would run as root before any of that
// exists.
func userLaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory that holds "+
			"Library/LaunchAgents: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// installPlist writes the plist atomically and refuses to clobber.
//
// The refusal is the important half. A plist rewritten in place changes what
// launchd will start on the NEXT bootstrap and changes nothing that `launchctl
// print` reports about the job already loaded -- so the file and the running
// supervisor disagree, silently, for as long as the current process lives. The
// operator who edited the config, re-installed and saw no error has every
// reason to believe the change took effect, and the harness is still quoting
// the old market with the old sizing until something restarts it.
//
// The write is temp-file-plus-rename in the SAME directory, so it is one
// `rename(2)` on one filesystem. A plist truncated by a failed write is not
// merely a bad file: launchd refuses to load it, so the job it describes has no
// `KeepAlive` and no `caffeinate` -- the supervision is ABSENT rather than
// wrong, which is the failure nobody notices. Leaving the previous file
// untouched is what makes a failed install a no-op instead.
func installPlist(path string, body []byte, force bool) error {
	switch _, err := os.Stat(path); {
	case err == nil && !force:
		return fmt.Errorf("refusing to overwrite the launchd job already "+
			"installed at %s.\n\n"+
			"A job that is currently loaded keeps the OLD ProgramArguments "+
			"until it is booted out, so replacing the file changes what starts "+
			"next time and changes nothing launchctl reports now -- the "+
			"operator sees a successful install and the harness goes on "+
			"running the previous configuration. Read the installed plist, "+
			"boot the job out if it is loaded, and pass the force flag to "+
			"replace it deliberately:\n\n"+
			"  launchctl bootout gui/%d/%s\n", path, os.Getuid(), agentLabel)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s could not be examined: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()

	// discard removes the partial file and reports the ORIGINAL failure, which
	// is the one describing what the disk did. A temp file left behind in
	// `~/Library/LaunchAgents` is not inert: launchd scans that directory.
	discard := func(cause error) error {
		if rmErr := os.Remove(tmpName); rmErr != nil &&
			!errors.Is(rmErr, os.ErrNotExist) {
			return errors.Join(cause, rmErr)
		}
		return cause
	}

	write := func() error {
		if _, err := plistWriteFn(tmp, body); err != nil {
			return fmt.Errorf("writing the plist to %s: %w", tmpName, err)
		}
		// Explicitly, and not through the umask `CreateTemp` applies: it
		// creates 0600, and launchd reads the file as the user but the
		// operator has to be able to read it back without sudo.
		if err := tmp.Chmod(plistMode); err != nil {
			return fmt.Errorf("setting mode %#o on %s: %w",
				plistMode, tmpName, err)
		}
		if err := tmp.Sync(); err != nil {
			return fmt.Errorf("syncing %s: %w", tmpName, err)
		}
		return tmp.Close()
	}
	if err := write(); err != nil {
		tmp.Close()
		return discard(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return discard(fmt.Errorf("installing %s: %w", path, err))
	}
	return nil
}

// writeInstallReport prints the file that was written and the commands that
// were NOT run.
//
// The uid is resolved rather than printed as `$UID`, because the operator
// pastes these into whatever shell is in front of them and `$UID` is not set in
// every one of them. A `bootstrap` that silently expanded to `gui/` would name
// no session at all.
func writeInstallReport(out io.Writer, path string,
	plan lifecycle.LaunchdPlan) error {

	uid := os.Getuid()
	var b strings.Builder
	fmt.Fprintf(&b, "installed %s (mode %#o)\n", path, plistMode)
	fmt.Fprintf(&b, "  Label             %s\n", plan.Label)
	fmt.Fprintf(&b, "  ProgramArguments  %s\n",
		strings.Join(plan.Command(), " "))
	b.WriteString("  KeepAlive         true (H-DEP-2: restart on any exit, " +
		"including exit 0)\n")
	fmt.Fprintf(&b, "  stdout            %s\n", plan.StdoutPath)
	fmt.Fprintf(&b, "  stderr            %s\n", plan.StderrPath)

	b.WriteString("\nNOTHING HAS BEEN STARTED. Loading this job starts a " +
		"process that quotes a live\naccount; that is an operator action, not " +
		"a side effect of writing a file. Run:\n\n")
	fmt.Fprintf(&b, "  launchctl bootstrap gui/%d %s\n", uid, path)
	fmt.Fprintf(&b, "  launchctl kickstart -k gui/%d/%s\n", uid, plan.Label)
	b.WriteString("\nand to stop it -- which stops quoting but does NOT " +
		"clear the durable halt latch\n(H-HALT-4, §10.4):\n\n")
	fmt.Fprintf(&b, "  launchctl bootout gui/%d/%s\n", uid, plan.Label)

	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("%s was installed but the commands to load it could "+
			"not be printed: %w", path, err)
	}
	return nil
}

// confidence: high

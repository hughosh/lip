// Command harness is the 24/7 LIP quoting harness of notes/harness-spec.md.
//
// # The refusal that used to be here
//
// This binary previously refused to start at all, unconditionally, and the
// reason was written down rather than left as a TODO:
//
//	a half-wired trading binary that starts and does almost nothing is exactly
//	the artifact that gets run "just to see", and this one would do so against a
//	real account with real credentials on disk.
//
// The wiring is now done, so that refusal is REPLACED rather than deleted. What
// replaces it is narrower and each clause names one way a start could be an
// accident rather than a decision:
//
//   - there is no default `-config`, so no invocation of this binary means
//     anything without a file an operator wrote;
//   - the instance lock is taken before the first REST request, so a second
//     harness on one account is refused before it can send one (H-DEP-5);
//   - a set halt latch without `-resume` refuses, because H-HALT-4 makes the
//     latch survive the process on purpose and §10.4 makes clearing it an
//     OPERATOR action;
//   - `S` above the canary's one contract refuses without `-rung` naming the
//     ladder step on the command line, so the canary is what an unadorned
//     invocation gets and raising size is a deliberate act at the moment of
//     starting rather than an edit made to a file once (pilot-plan §1: "Build
//     once; raise the knob").
//
// # What this binary still will not do
//
// It will not create its own database. `-provision` does that, once, with an
// operator present -- see `provision.go` for why an absent path and a mistyped
// one are the same thing to `hstore.Open`. And it will not load its own launchd
// job: `-deploy` writes the plist and prints the `launchctl` lines, because
// loading the job STARTS a trading process and that is not a side effect of
// installing a file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lip/harness/num"
)

// The exit codes, and they are a contract rather than a convention: `launchd`
// KeepAlive restarts this process, so what it exits WITH is what decides whether
// a bad configuration restarts forever.
const (
	// exitDrained is the one clean end: a signal whose durable stop reached the
	// disk, followed by an account that went flat with nothing of ours resting.
	exitDrained = 0
	// exitFailed is an operational failure -- the store would not open, the
	// credentials would not load, the startup walk's context ended.
	exitFailed = 1
	// exitRefused is the structural refusal above. It is DISTINCT from
	// exitFailed because the two want opposite responses from a supervisor: a
	// failure may be transient and worth restarting into, and a refusal is a
	// statement that this invocation should not run at all.
	exitRefused = 2
)

// cmdline is this binary's command line, after parsing.
//
// It is a type, and the registration below is a function, for one reason:
// `-deploy` WRITES an argv that this same binary is later started with, and the
// only honest way to assert that the written argv still starts is to parse it
// with the flag set `main` registers. A test that hand-rolled its own parser
// would be checking the plist against its own idea of the flag names, which is
// the "a test must not measure itself" failure in its most literal form -- rename
// `-rung` here and a hand-rolled parser goes on passing while every deployed job
// on the machine stops starting. `TestTheDeployedArgvStartsUnderTheRungGate`
// parses a real plist's ProgramArguments with `newFlagSet`.
type cmdline struct {
	configPath    string
	qualification string
	assess        string
	resume        string
	rung          string
	doProvision   bool
	doDeploy      bool
	force         bool
	live          bool
}

// newFlagSet registers the command line and returns the destination it parses
// into.
//
// `errorHandling` is a parameter rather than the constant `main` wants because
// `flag.ExitOnError` calls `os.Exit` from inside `Parse`, and a test that parsed
// a bad argv under it would take the whole suite down instead of reporting.
// Production passes `ExitOnError`; a test passes `ContinueOnError`. Nothing else
// about the set differs between them.
func newFlagSet(errorHandling flag.ErrorHandling) (*flag.FlagSet, *cmdline) {
	fs := flag.NewFlagSet("harness", errorHandling)
	var cl cmdline

	// No default. A default config path is a path this binary invents, and the
	// file it names carries the location of the durable halt latch: a harness
	// started from a different directory would find a fresh empty one, which is
	// H-HALT-4 erased by a `cd`.
	fs.StringVar(&cl.configPath, "config", "", "path to the pilot config file (required)")
	fs.StringVar(&cl.qualification, "qualification", "", "absolute path to the durable "+
		"read-only qualification evidence bundle; incompatible with -live")
	fs.StringVar(&cl.assess, "assess-qualification", "", "absolute path to a preserved "+
		"qualification evidence bundle. Reads that one file, prints the LOCAL q01 "+
		"assessment as JSON, and exits. Starts no observer and awards no rung")
	fs.StringVar(&cl.resume, "resume", "", "start with the halt latch SET, after "+
		"reading it (§10.4). The value is the reason, recorded in the log")
	fs.StringVar(&cl.rung, "rung", "", "the capital ladder step this invocation is "+
		"for (pilot-plan §1). Required whenever S exceeds the canary's one contract")

	fs.BoolVar(&cl.doProvision, "provision", false, "create the five-record store "+
		"and its anomaly journal, then exit. Refuses if either already exists")
	fs.BoolVar(&cl.doDeploy, "deploy", false, "render and install the launchd job, "+
		"then exit. Prints the launchctl lines; does not run them")
	fs.BoolVar(&cl.force, "force", false, "with -deploy, overwrite an installed plist")

	// H-VER-1's FIRST key. A flag and never a JSON field: a config that armed
	// itself would arm every process that read it, including one an operator
	// started to look at a book. The second key is the `live_ok` sentinel, and
	// both are required before any non-GET request leaves this process.
	fs.BoolVar(&cl.live, "live", false, "permit writes to the exchange. Requires "+
		"the paths.live_ok sentinel to exist as well; without both keys this "+
		"process is structurally read-only and cannot place an order")

	return fs, &cl
}

func main() {
	fs, cl := newFlagSet(flag.ExitOnError)

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(exitRefused)
	}

	if err := run(fs, cl.configPath, cl.qualification, cl.assess, cl.resume,
		cl.rung, cl.doProvision, cl.doDeploy, cl.force, cl.live); err != nil {

		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		var ref *refusal
		if errors.As(err, &ref) {
			os.Exit(exitRefused)
		}
		os.Exit(exitFailed)
	}
	os.Exit(exitDrained)
}

// refusal is a start this binary declines, as distinct from one that failed.
//
// The distinction is for `launchd`, not for the reader. `KeepAlive` restarts on
// exit, so a configuration error that exited like a transient failure would be
// retried forever at whatever cadence launchd chooses -- and a harness that
// cannot start is one nobody is watching restart.
type refusal struct{ err error }

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

func refuse(format string, a ...any) error {
	return &refusal{err: fmt.Errorf(format, a...)}
}

func run(fs *flag.FlagSet, configPath, qualification, assess, resume, rung string,
	doProvision, doDeploy, force, live bool) error {

	// The offline assessor, and it is FIRST because it is the one invocation
	// that reads no config, opens no store, and reaches no network. Every other
	// flag is refused alongside it rather than ignored: an assessment that could
	// be requested from a starting harness would make the process under
	// examination the author of its own verdict, and `-assess-qualification`
	// exists precisely so the judging happens after the run, from the artifact.
	if assess != "" {
		if configPath != "" || qualification != "" || resume != "" || rung != "" ||
			doProvision || doDeploy || force || live {
			return refuse("-assess-qualification reads one preserved evidence " +
				"file and exits. It takes no other flag: an assessment issued by " +
				"a process that was also starting the observer, provisioning a " +
				"store, or arming for writes would be the run grading itself")
		}
		return assessQualificationBundle(assess, os.Stdout)
	}

	if configPath == "" {
		fs.Usage()
		return refuse("no -config given, and there is no default.\n\n" +
			"The file names which account this process can reach, which market " +
			"it quotes, and where its durable halt latch lives. A binary that " +
			"invented any of those could be pointed at a fresh empty latch by " +
			"being started from a different directory, which is H-HALT-4 erased " +
			"by a working directory")
	}
	if doProvision && doDeploy {
		return refuse("-provision and -deploy are separate operator acts: one " +
			"creates the ownership ledger this account's fills are classified " +
			"against, the other installs a job that will start trading. Run them " +
			"one at a time and look at the output of each")
	}
	if qualification != "" && live {
		return refuse("-qualification and -live cannot be given together. " +
			"Qualification is the zero-write rung; an evidence file produced by " +
			"a process armed to write would misstate the operating envelope")
	}
	if doProvision && qualification != "" {
		return refuse("-provision and -qualification are separate acts. " +
			"Provisioning exits without running the observer, so it cannot " +
			"contribute a process segment to qualification evidence")
	}

	// Refused BEFORE the config is read, and so before `-provision` could
	// create anything. Provisioning is the act of making a durable store; the
	// two flags together read as "set it up and let it trade", which is exactly
	// the conflation H-VER-1 exists to prevent. Nothing in this binary ever
	// creates `live_ok`.
	if doProvision && live {
		return refuse("-provision and -live cannot be given together. " +
			"Provisioning creates the ownership ledger; arming permits orders. " +
			"A single command that did both would make setting the harness up " +
			"and letting it trade the same act, and the whole point of the " +
			"live_ok sentinel is that a human creates it deliberately, after " +
			"looking at what was provisioned")
	}

	c, err := loadConfig(configPath)
	if err != nil {
		return &refusal{err: err}
	}

	switch {
	case doProvision:
		return provision(c, os.Stdout)
	case doDeploy:
		abs, err := absConfigPath(configPath)
		if err != nil {
			return err
		}
		// Installing a job which can only restart into a structural refusal is
		// not a successful deployment.  Validate both destinations before the
		// plist is written; this loads no exchange credential and makes no
		// network request.
		if _, err := productionAlertFactory(c.Paths.Env); err != nil {
			return &refusal{err: err}
		}
		// `rung` is carried into the deployed argv rather than dropped here.
		// It used to be dropped, and the result was a plist for any S above
		// the canary that `checkRung` refused at every start -- forever, under
		// `KeepAlive` (lip-3yo). `installAgent` refuses a rung that does not
		// match this config before it writes anything.
		return installAgent(c, abs,
			deployOptions(rung, qualification, force, live), os.Stdout)
	}

	// The ladder gate. `config.go` has already asserted the config's own rung
	// against its sizing, so this is the SECOND assertion and it is a different
	// one: that the operator starting this process, now, at this size, said so
	// at the command line. A config file is edited once and then started many
	// times; the flag is the part that cannot be inherited from last week.
	if err := checkRung(c, rung); err != nil {
		return err
	}

	// The first key, carried from the command line into the process. The second
	// is the `live_ok` sentinel, and it is deliberately NOT checked here: it is
	// stat-ed freshly at every write, so removing the file disarms the next one
	// without needing to find and stop this process.
	c.Live = live

	// Both alert destinations are required for an unattended run.  Load and
	// validate them before the first exchange request so a broken deployment
	// refuses without touching the account.
	makeAlerts, err := productionAlertFactory(c.Paths.Env)
	if err != nil {
		return &refusal{err: err}
	}

	// H-DEP-5 begins here, before either the active-program GET below or the
	// qualification bundle can be touched. newRigWithLock takes ownership once
	// composition begins; until then this deferred close owns every error path.
	heldLock, qrec, err := lockThenOpenQualification(qualification, c)
	if err != nil {
		return &refusal{err: err}
	}
	defer func() {
		if heldLock != nil {
			heldLock.Close()
		}
	}()

	// A plain background context, and NO signal wired into it.
	//
	// `signal.NotifyContext` is the obvious thing to reach for here and it is
	// exactly wrong: it would cancel the context on SIGINT/SIGTERM, and
	// cancelling the context is how this process stops. H-HALT-3 forbids that
	// outright -- "SIGTERM does not exit. It sets `WINDING_DOWN`, keeps the
	// process alive, and exits only when every market is flat or closed" -- so
	// the signal goes to `shutdown.go`'s handler, which commits a durable stop
	// and issues a drain permit, and the exit happens later or never.
	//
	// The consequence is that `serve` normally does not return at all. That is
	// H-FAIL-1's list of process exits, honoured: an authorised drain, or a
	// SIGKILL. There is no third.
	ctx := context.Background()

	// The process anomaly sink, and it is created HERE rather than inside
	// `newRig` because F6's DNS fallback can fire before the rig exists: the
	// active-programme walk is the first request the process makes, and it goes
	// through the cached dialer. One sink, passed to both, is what puts that
	// fallback in the same queue and the same durable journal as everything the
	// rig raises afterwards.
	anom := newAnomalySink()

	ex, err := productionExchange(ctx, c, anom, qrec)
	if err != nil {
		return err
	}

	r, err := newRigWithLock(ctx, c, resume != "", ex, makeAlerts, anom, qrec, heldLock)
	// Ownership transfers at function entry, including its refusal paths.
	heldLock = nil
	if err != nil {
		// Every refusal `newRig` makes is structural: the lock is held, the
		// latch is set, the store does not exist. None of them is retryable by
		// restarting into the same state.
		return &refusal{err: err}
	}

	if resume != "" {
		fmt.Fprintf(os.Stderr, "harness: starting with the halt latch SET at %s, "+
			"on the operator's authority: %s\n"+
			"The latch is NOT cleared. This process will wind the account down; "+
			"it will not quote (H-HALT-4, §10.4).\n", c.Paths.Latch, resume)
	}

	err = r.serve(ctx)
	if qrec != nil {
		if endErr := qrec.EndSegment(time.Now().UTC(),
			qualificationEndReason(err)); endErr != nil && err == nil {
			err = fmt.Errorf("ending qualification evidence segment: %w", endErr)
		}
		if checkpointErr := qrec.Checkpoint(); checkpointErr != nil && err == nil {
			err = fmt.Errorf("checkpointing qualification evidence segment: %w", checkpointErr)
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// checkRung is D2's fourth clause.
//
// It refuses `S` above one contract unless the invocation NAMES the rung, and it
// requires the name to match the config's. The two together are what make the
// ladder a ladder: the config says which step it describes, the flag says which
// step the operator believes they are on, and a disagreement is a start where
// one of those two beliefs is wrong.
//
// One contract is the threshold rather than zero because the canary IS one
// contract -- "1 market, S=1, ~$1: does the order path work at all" -- so the
// unadorned invocation gets exactly the run whose purpose is to find out whether
// anything works, and nothing larger.
func checkRung(c config, named string) error {
	canary := num.QtyFromFloat(1)
	if c.Params.S <= canary && named == "" {
		return nil
	}
	if named == "" {
		return refuse("the config sets S=%s, above the canary's %s, and no "+
			"-rung was given.\n\n"+
			"pilot-plan §1 is \"build once; raise the knob\", which makes the "+
			"knob the only thing between a $1 canary and a $100 pilot. A knob "+
			"with nothing asserting what it means is a knob that gets raised by "+
			"accident, so raising it is an act performed at the moment of "+
			"starting: pass -rung %s to start this configuration.\n\n%s",
			c.Params.S.Wire(), canary.Wire(), c.Rung.name, c.Rung.human)
	}
	if _, known := rungs[named]; !known {
		return refuse("-rung %q is not a ladder step; one of %v", named, rungNames())
	}
	if named != c.Rung.name {
		return refuse("-rung %s was given but %s declares rung %q.\n\n"+
			"One of the two is wrong, and this is not a disagreement to resolve "+
			"by preferring either: the file describes what the run IS and the "+
			"flag describes what the operator believes they are starting. Fix "+
			"whichever is stale",
			named, configLabel(c), c.Rung.name)
	}
	return nil
}

// configLabel is for the message above. The config does not carry its own path
// -- nothing else needs it -- so this names the file by the one field an
// operator can grep for.
func configLabel(c config) string {
	return fmt.Sprintf("the config for %s", c.Ticker)
}

// absConfigPath is what `-deploy` records in the plist.
//
// It must be absolute for the same reason every path in the config must be:
// `launchd` runs the job with no shell and no inherited working directory, so a
// relative path is resolved against something neither the operator nor this code
// chose.
func absConfigPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving the config path %q: %w", path, err)
	}
	return abs, nil
}

// confidence: high

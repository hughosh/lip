package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lip/harness/lifecycle"
	"lip/harness/qual"
	"lip/harness/quote"
)

const qualificationCheckpointEvery = 5 * time.Second

// acquireHarnessLock is shared by the command's pre-network path and newRig's
// direct-test/fallback path. One function keeps H-DEP-5 from becoming two
// similar-looking lock rules whose production copy can be dropped while the
// composition tests continue exercising the other.
func acquireHarnessLock(c config) (*lifecycle.InstanceLock, error) {
	lock, err := lifecycle.AcquireInstanceLock(c.Paths.Lock)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, &refusal{err: err}
	}
	return lock, err
}

// lockThenOpenQualification takes the single-instance lock before the
// evidence file is created or resumed. A refused second process must not append
// a spurious unclean segment to the first process's qualification bundle.
func lockThenOpenQualification(path string, c config) (*lifecycle.InstanceLock,
	*qual.Recorder, error) {

	lock, err := acquireHarnessLock(c)
	if err != nil {
		return nil, nil, err
	}
	recorder, err := openQualification(path, c)
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	return lock, recorder, nil
}

// openQualification creates or resumes the evidence bundle before the first
// REST request. That ordering is what lets the active-program walk be counted
// and makes a corrupt or mismatched prior bundle a start refusal rather than a
// file discovered after the observer is already running.
func openQualification(path string, c config) (*qual.Recorder, error) {
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, refuse("qualification evidence path %q is relative; "+
			"launchd supplies no working directory and a restart must resume the "+
			"same artifact", path)
	}
	if c.Live {
		return nil, refuse("qualification evidence is zero-write, but this " +
			"process is live")
	}
	if c.ConfigHash == "" {
		return nil, refuse("qualification needs the exact config hash")
	}

	binary, err := currentBinaryIdentity()
	if err != nil {
		return nil, err
	}
	segmentID, err := qualificationSegmentID()
	if err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	recorder, err := qual.Open(path, qual.Metadata{
		SchemaVersion:  qual.SchemaVersion,
		ConfigHash:     c.ConfigHash,
		BinaryIdentity: binary,
		Ticker:         c.Ticker,
		Rung:           c.Rung.name,
		Live:           false,
	}, qual.SegmentStart{
		ID: segmentID, PID: os.Getpid(), StartedAt: started,
	})
	if errors.Is(err, qual.ErrCorrupt) || errors.Is(err, qual.ErrMetadataMismatch) || errors.Is(err, qual.ErrFinalized) {
		return nil, &refusal{err: err}
	}
	return recorder, err
}

// assessQualificationBundle is the whole offline consumer of the strict local
// assessor, and its shape is the point.
//
// It reads one file with `qual.LoadEvidence` -- the same strict reader `Open`
// resumes with -- applies the FIXED `AssessQ01Local`, and prints the result. It
// takes no Requirements, so nothing here can shorten the 20 minutes, slow a
// cadence, or lower 99%; it holds no store, no lock and no exchange client, so
// it cannot be the thing that starts an observer; and it refuses to print an
// assessment whose scope or outstanding-evidence list has gone missing, because
// a local verdict that stopped saying what it does NOT prove is the exact
// artifact an operator would mistake for q01 itself.
//
// The unmet case still prints. A caller that has to parse JSON to discover a
// failure is a caller that will read exit status instead, so both are honest:
// the assessment goes to `out`, and the error makes the exit non-zero.
func assessQualificationBundle(path string, out io.Writer) error {
	if !filepath.IsAbs(path) {
		return refuse("the qualification evidence path %q is relative. An "+
			"assessment names the exact artifact it read", path)
	}
	evidence, err := qual.LoadEvidence(path)
	if err != nil {
		return fmt.Errorf("reading qualification evidence %s: %w", path, err)
	}
	assessment, err := qual.AssessQ01Local(evidence)
	if err != nil {
		return fmt.Errorf("assessing qualification evidence %s: %w", path, err)
	}
	if assessment.Scope != qual.Q01LocalScope || len(assessment.ExternalOutstanding) == 0 {
		return fmt.Errorf("the local assessment of %s does not declare its own "+
			"limits (scope %q, %d outstanding external item(s)); refusing to "+
			"print a verdict that could be read as q01 completion",
			path, assessment.Scope, len(assessment.ExternalOutstanding))
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(assessment); err != nil {
		return fmt.Errorf("writing the local assessment of %s: %w", path, err)
	}
	if !assessment.LocalRequirementsMet {
		return fmt.Errorf("%s does not meet the local q01 requirements (%d failure(s))",
			path, len(assessment.Failures))
	}
	return nil
}

// currentBinaryIdentity pins the exact executable bytes. VCS settings are
// appended when Go embedded them, so an operator can read the commit directly;
// the SHA remains authoritative even for a locally built or dirty binary.
func currentBinaryIdentity() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the harness executable: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening the harness executable: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return "", fmt.Errorf("hashing the harness executable: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing the harness executable after hashing: %w", err)
	}

	identity := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
				identity += ";" + setting.Key + "=" + setting.Value
			}
		}
	}
	return identity, nil
}

func qualificationSegmentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("creating qualification segment id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// wouldWriteFingerprint deliberately excludes queue ids, coids and wall time:
// those change merely because the owner reconsidered. Every field that changes
// the desired exchange action is included, so a new price, size, cancel target,
// role, state or truth generation starts a distinct episode.
func wouldWriteFingerprint(req writeRequest, global quote.GlobalState,
	market quote.MarketState, generation uint64) qual.WouldWriteFingerprint {

	state := fmt.Sprintf("role=%s;global=%s;market=%s;truth_generation=%d",
		req.Role, global, market, generation)
	fp := qual.WouldWriteFingerprint{
		Ticker: req.Market, Side: req.Side.String(), State: state,
	}
	if req.Op == quote.OpPlace {
		fp.Kind = qual.WriteCreate
		fp.Price = strconv.Itoa(req.Order.PriceCents())
		fp.Quantity = req.Order.Count().Wire()
		return fp
	}

	fp.Kind = qual.WriteCancel
	targets := make([]string, 0, len(req.Orders))
	for _, order := range req.Orders {
		targets = append(targets, fmt.Sprintf("%s:%d:%s",
			order.OrderID, order.Price4, order.Remaining.Wire()))
	}
	sort.Strings(targets)
	fp.OrderID = strings.Join(targets, ",")
	return fp
}

// runQualificationCheckpoints keeps filesystem I/O off the owner, monitor,
// poll and network goroutines. Recorder.Checkpoint snapshots under its data
// lock and performs the atomic write after releasing it.
func (r *rig) runQualificationCheckpoints(ctx context.Context) {
	if r.qual == nil {
		return
	}
	tick := time.NewTicker(qualificationCheckpointEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := r.qual.Checkpoint(); err != nil {
				r.failQualification(fmt.Errorf("periodic checkpoint: %w", err))
				return
			}
		}
	}
}

func (r *rig) failQualification(err error) {
	if err == nil {
		return
	}
	// `lip-oqq`. A write REFUSED BECAUSE THE EVIDENCE IS ALREADY FINALIZED is
	// not a failure to record; it is the record having been properly closed
	// first. The two are opposite conditions and only one of them invalidates a
	// qualification.
	//
	// Shutdown freezes the evidence once the trading store has stopped
	// (shutdown.go:304), but the owner's snapshot publisher is a separate
	// goroutine and gets at least one more tick in: it calls RecordEvent for
	// the state summary (run.go:3086-3091), receives ErrFinalized, and without
	// this the fail-closed channel turns that into `serve` returning
	// "qualification evidence failed". SIGTERM is the operator's NORMAL stop,
	// so this fired at the end of every run -- including, in the first real
	// attempt, one whose evidence file had finalized perfectly well seconds
	// earlier and was fully assessable.
	//
	// This does NOT weaken the fail-closed rule. Every other error still fails
	// the qualification, including a write refused for any reason other than
	// the artifact being closed. What changes is that "the artifact is closed"
	// stops being reported as "the artifact is broken".
	if errors.Is(err, qual.ErrFinalized) {
		return
	}
	select {
	case r.qualErrors <- err:
	default:
	}
}

func qualificationEndReason(err error) string {
	if err == nil {
		return "process ended"
	}
	return "process error: " + strings.TrimSpace(err.Error())
}

// confidence: high

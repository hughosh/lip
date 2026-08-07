package lifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"lip/harness/risk"
)

// latchVersion is the exact schema version this build writes and accepts.
//
// A version mismatch is not a compatibility problem to be tolerated. The file's
// only job is to answer "did a previous incarnation stop?", and a file we cannot
// parse is a file whose answer we do not know -- which, for this question,
// resolves to latched. See classify.
const latchVersion = 1

// LatchRecord is what `harness.halt` contains: H-HALT-4's "trigger, timestamp
// and market".
//
// It is a JSON object rather than a bare marker file because the operator who
// finds this on disk at 02:00 needs to know WHICH stop fired. A taker fill, a
// foreign fill, `pnl_kill` and a SIGTERM all latch, and the operator action of
// §10.4 is different in each case.
type LatchRecord struct {
	Version  int    `json:"version"`
	Trigger  string `json:"trigger"`
	TsMillis int64  `json:"ts_ms"`
	Market   string `json:"market"`
}

// LatchStore is the durable halt latch, behind an interface so the scenario
// exchange and the tests can substitute a failing disk.
//
// Note what is absent, and note that it is absent from the INTERFACE and not
// merely from the implementation: there is no Clear, Remove, Reset, Overwrite
// or Resume. H-HALT-4 says "the harness never self-clears it. Clearing is the
// operator action of §10.4." A method that clears the latch is that sentence
// deleted, and it would be reachable from any code path that thought it had a
// good reason. The operator removes the file; this process cannot.
type LatchStore interface {
	// Load reports the latch state. `present` false with a nil error is the ONLY
	// clear answer. Any error means the state is unknown, which is not clear.
	Load() (record LatchRecord, present bool, err error)

	// Ensure makes a latch durable. `durable` true means the record -- this one
	// or an earlier one -- is on disk and survives a power cut.
	Ensure(record LatchRecord) (durable bool, err error)
}

// FileLatch is the production `harness.halt`.
type FileLatch struct {
	path string
	dir  string
	// syncDirFn is a seam, not a policy knob. The EEXIST retry path below has to
	// be able to fail its parent-directory sync in a test, and there is no
	// portable way to make a real fsync of a real directory fail on demand.
	// Private, defaulted in the constructor, and never settable from outside.
	syncDirFn func(string) error
}

// NewFileLatch pins the latch to an absolute path.
//
// The path is required to be absolute because the alternative is a latch whose
// location depends on the working directory of whatever restarted the process.
// launchd's `WorkingDirectory`, a shell, and a test runner do not agree, and a
// halt written into one directory and read from another is a halt that
// self-clears -- HR-009 with an extra step.
func NewFileLatch(path string) (*FileLatch, error) {
	if path == "" {
		return nil, errors.New("latch path is empty")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("latch path %q is not absolute: a relative latch "+
			"is read from whatever directory the supervisor happened to start "+
			"the process in, and a halt written to one and read from another "+
			"self-clears", path)
	}
	return &FileLatch{
		path: path, dir: filepath.Dir(path), syncDirFn: syncDir,
	}, nil
}

// Path is the absolute location the operator removes to clear the latch.
func (f *FileLatch) Path() string { return f.path }

// Load is H-HALT-4's read, and its whole design is in which errors mean "clear".
//
// Exactly one condition is clear: the file does not exist. Everything else --
// a permission error, an EIO, a directory where the file should be, a truncated
// write from a power cut, JSON that does not parse, a null, an empty file, the
// wrong version -- reports latched, an error, or both.
//
// The asymmetry is the point. Reading "clear" wrongly resumes quoting after a
// halt that a human has not seen; reading "latched" wrongly leaves a flat
// account not adding, which an operator clears in one command. There is no
// symmetric error handling for a question this asymmetric.
func (f *FileLatch) Load() (LatchRecord, bool, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return LatchRecord{}, false, nil
		}
		// Present-but-unreadable. Not clear. The caller blocks adding and keeps
		// retrying; it does not get to assume the file it could not open was
		// absent.
		return LatchRecord{}, true, fmt.Errorf("latch %s could not be read, so "+
			"its state is unknown and is treated as latched: %w", f.path, err)
	}
	rec, err := decodeLatch(b)
	if err != nil {
		return rec, true, err
	}
	return rec, true, nil
}

// decodeLatch validates the record's SHAPE. Every failure below returns a
// non-nil error, and every caller of this function treats the file as latched
// regardless -- the error selects the anomaly text, not the verdict.
func decodeLatch(b []byte) (LatchRecord, error) {
	if len(b) == 0 {
		return LatchRecord{}, errors.New("latch file is empty: a zero-length " +
			"file is what a crash between create and write leaves behind, and " +
			"the incarnation that created it was in the act of stopping")
	}

	// DisallowUnknownFields makes an unrecognised key a diagnostic rather than
	// something silently dropped. It does not make the file clear: a latch we do
	// not fully understand was still written by something that stopped.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rec LatchRecord
	if err := dec.Decode(&rec); err != nil {
		return LatchRecord{}, fmt.Errorf("latch file does not decode as a "+
			"latch record: %w", err)
	}
	// A bare `null` decodes into the zero value without error, and a bare
	// `123` does not decode into a struct at all -- the first is the one that
	// would otherwise pass.
	if rec == (LatchRecord{}) {
		return rec, errors.New("latch file decoded to an empty record (a JSON " +
			"null or an object with no fields)")
	}
	if rec.Version != latchVersion {
		return rec, fmt.Errorf("latch file is version %d, this build writes and "+
			"reads version %d; a record we cannot interpret is not a record "+
			"that says the harness may resume", rec.Version, latchVersion)
	}
	if rec.Trigger == "" {
		return rec, errors.New("latch file carries no trigger; H-HALT-4 " +
			"requires the trigger so the operator knows which stop fired")
	}
	if rec.TsMillis <= 0 {
		return rec, fmt.Errorf("latch file carries a non-positive timestamp %d",
			rec.TsMillis)
	}
	return rec, nil
}

// Ensure is H-HALT-4's write, and it is a FIRST-WRITER-WINS create.
//
// `O_EXCL` is doing two jobs. The obvious one is that an existing latch is never
// truncated or overwritten: the first durable cause is the one the operator
// investigates, and a later, more mundane trigger -- a SIGTERM sent while
// winding down from a taker fill -- must not overwrite the reason the harness
// stopped. The second is that create-exclusive is atomic against a second
// process, which the single-instance lock is supposed to prevent and which this
// does not depend on it preventing.
//
// The durability sequence is write, `Sync`, close, then `Sync` the PARENT
// DIRECTORY. Syncing the file alone persists the bytes but not necessarily the
// directory entry that names them, so a power cut between the two leaves a
// harness that stopped and a directory that never heard about it. That is the
// exact failure this file exists to prevent, one layer down.
//
// A crash mid-write leaves a short or empty file. That file is not clear (see
// decodeLatch), so the conservative outcome survives the interruption.
func (f *FileLatch) Ensure(rec LatchRecord) (bool, error) {
	if rec.Version == 0 {
		rec.Version = latchVersion
	}
	if err := validateCause(rec); err != nil {
		return false, err
	}

	b, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("latch record does not marshal: %w", err)
	}
	b = append(b, '\n')

	fh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Already latched by an earlier cause. The file is deliberately left
			// exactly as it was -- but "a file is there" is NOT the same claim as
			// "it is durable", and this path is reached by the RETRY after a
			// failed write as well as by a genuine second cause.
			//
			// The sequence that made this wrong: the first Ensure created the
			// file, wrote it, fsynced it, closed it, and then failed to fsync the
			// PARENT DIRECTORY. It correctly returned not-durable, so the caller
			// blocked adding and retried. The retry saw EEXIST and returned
			// durable immediately -- without ever completing the directory sync
			// the first attempt failed. A power cut then loses the directory
			// entry, the restart finds no latch, and the harness resumes adding.
			//
			// So the retry finishes the job it is standing in for.
			if serr := f.syncDirFn(f.dir); serr != nil {
				return false, fmt.Errorf("latch %s exists but its parent "+
					"directory %s could not be synced, so the entry naming it "+
					"may not survive a power cut: %w", f.path, f.dir, serr)
			}
			return true, nil
		}
		return false, fmt.Errorf("latch %s could not be created: %w", f.path, err)
	}

	if _, err := fh.Write(b); err != nil {
		fh.Close()
		return false, fmt.Errorf("latch %s could not be written: %w", f.path, err)
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return false, fmt.Errorf("latch %s could not be synced: %w", f.path, err)
	}
	if err := fh.Close(); err != nil {
		return false, fmt.Errorf("latch %s could not be closed: %w", f.path, err)
	}
	if err := f.syncDirFn(f.dir); err != nil {
		return false, fmt.Errorf("latch directory %s could not be synced, so "+
			"the file's bytes are durable but the directory entry naming them "+
			"may not be: %w", f.dir, err)
	}
	return true, nil
}

// validateCause rejects a record that would be useless to the operator who
// finds it. A latch with no trigger is a file that says "something stopped".
func validateCause(rec LatchRecord) error {
	if rec.Version != latchVersion {
		return fmt.Errorf("latch record version %d, this build writes %d",
			rec.Version, latchVersion)
	}
	if rec.Trigger == "" {
		return errors.New("latch record has no trigger: H-HALT-4 requires the " +
			"trigger, and a latch that cannot say why it fired cannot be " +
			"adjudicated by the operator of §10.4")
	}
	if rec.TsMillis <= 0 {
		return fmt.Errorf("latch record timestamp %d is not positive",
			rec.TsMillis)
	}
	return nil
}

// syncDir fsyncs a directory so a newly created entry survives a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// latchInvalid is the SEV1 a malformed or unreadable latch produces.
//
// It is SEV1 and not SEV2 because the harness is now in WINDING_DOWN on the
// strength of a file it could not read. That is the correct direction, and it is
// also a state no automatic process can leave -- so the operator has to be told
// now rather than in the next hourly heartbeat.
func latchInvalid(err error) risk.Anomaly {
	return risk.Anomaly{
		Class: "LATCH_INVALID", Sev: risk.SEV1,
		Text: fmt.Sprintf("the durable halt latch is present but not "+
			"interpretable, so it is treated as set and the harness enters "+
			"WINDING_DOWN without adding: %v", err),
	}
}

// confidence: high

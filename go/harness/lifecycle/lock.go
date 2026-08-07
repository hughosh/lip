package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// InstanceLock is H-DEP-5: "one instance only -- a PID lockfile, checked at
// startup. Two harnesses on one account is an unrecoverable position-model
// conflict."
//
// Unrecoverable is the operative word. Two processes on one account do not
// produce a merge conflict; they produce two `q_local` models that are each
// individually consistent, each wrong, and each unable to tell that the fills
// moving the position came from the other one. Every safety mechanism in this
// harness downstream of `q` -- the reducer size, the aggregate caps, H-POS-2's
// drift detector, `inv_kill` -- is then computing against a number that no
// amount of polling will correct, because the exchange's answer is the SUM of
// two intents and neither process authored it.
type InstanceLock struct {
	path string
	fh   *os.File
}

// AcquireInstanceLock takes an exclusive advisory lock and holds it for the
// life of the process.
//
// # The lock is the file descriptor, not the PID in the file
//
// A PID file alone is the classic broken version: the check is "read the PID,
// see if that process is alive", and it has two failure modes that both matter
// here. A crashed harness leaves a stale PID that blocks every restart --
// exactly when `launchd KeepAlive` is trying to bring one back after the SIGKILL
// of V4.17. And a recycled PID belongs to something else entirely, so the check
// either refuses to start for no reason or, worse, signals a stranger.
//
// `flock(LOCK_EX|LOCK_NB)` has neither. The lock is owned by the open file
// description and the kernel releases it when the process dies, however it dies
// -- so a stale file left by a SIGKILL is simply an unlocked file that the next
// incarnation reuses. The PID inside is written for the OPERATOR to read; this
// code never trusts it, never checks whether it is alive, and never signals it.
func AcquireInstanceLock(path string) (*InstanceLock, error) {
	if path == "" {
		return nil, errors.New("instance lock path is empty")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("instance lock path %q is not absolute: a "+
			"relative lock path is resolved against whatever directory the "+
			"supervisor started the process in, and two harnesses started from "+
			"two directories would each hold a different file and both believe "+
			"they were alone", path)
	}

	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("instance lock %s could not be opened: %w",
			path, err)
	}

	// Non-blocking on purpose. Blocking would leave a second harness waiting
	// silently for the first to exit -- and since H-HALT-3 makes the first one
	// outlive SIGTERM until it is drained, "silently" could mean hours, with the
	// operator believing the restart had happened.
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fh.Close()
		return nil, fmt.Errorf("instance lock %s is held by another process "+
			"(H-DEP-5): two harnesses on one account is an unrecoverable "+
			"position-model conflict, so this one does not start: %w", path, err)
	}

	// The PID is written only AFTER the lock is held, so the file never claims
	// an owner that does not own it. A process that lost the race wrote nothing.
	if err := writePID(fh); err != nil {
		syscall.Flock(int(fh.Fd()), syscall.LOCK_UN)
		fh.Close()
		return nil, fmt.Errorf("instance lock %s was acquired but its pid could "+
			"not be recorded: %w", path, err)
	}

	return &InstanceLock{path: path, fh: fh}, nil
}

func writePID(fh *os.File) error {
	if err := fh.Truncate(0); err != nil {
		return err
	}
	if _, err := fh.Seek(0, 0); err != nil {
		return err
	}
	if _, err := fh.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return err
	}
	return fh.Sync()
}

// Path is the lock file's absolute location.
func (l *InstanceLock) Path() string { return l.path }

// Close releases the lock and closes the descriptor. It deliberately does NOT
// remove the file.
//
// Unlinking a lock file is an inode race with a well-known shape: process A
// closes and unlinks while process B has already opened the same path, so B
// holds a lock on an inode with no name and process C creates a fresh file and
// locks that. Both then believe they are alone. Leaving the file in place makes
// the path and the inode the same thing for as long as the harness runs, and a
// stale unlocked file costs nothing -- the next incarnation reuses it.
func (l *InstanceLock) Close() error {
	if l == nil || l.fh == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.fh.Fd()), syscall.LOCK_UN)
	closeErr := l.fh.Close()
	l.fh = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// confidence: high

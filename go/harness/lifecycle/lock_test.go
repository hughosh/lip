package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	lockChildEnv = "LIP_LIFECYCLE_LOCK_CHILD"
	lockPathEnv  = "LIP_LIFECYCLE_LOCK_PATH"
)

// TestInstanceLockExcludesASecondProcessAndAllowsAStalePID is H-DEP-5.
//
// > one instance only | a PID lockfile, checked at startup. Two harnesses on one
// > account is an unrecoverable position-model conflict.
//
// Both halves are asserted, and they pull in opposite directions -- which is why
// the PID-file-only implementation is wrong in both:
//
//   - a SECOND live process must fail. A PID file with an aliveness check gets
//     this right only until a PID is recycled.
//   - a STALE file left by a SIGKILL must NOT block the restart. This is the
//     V4.17 case: `kill -9` mid-run, then `launchd KeepAlive` brings it back, and
//     a stale PID file would refuse every restart forever.
//
// `flock` on a retained descriptor gets both for free: the kernel drops the lock
// when the process dies however it dies, so a stale file is simply an unlocked
// file. The exclusion half needs a real second PROCESS, because flock is
// per-open-file-description and a second `flock` from within the same process
// succeeds.
//
// `M-L-LOCK` ignores the non-blocking lock's failure.
func TestInstanceLockExcludesASecondProcessAndAllowsAStalePID(t *testing.T) {
	if os.Getenv(lockChildEnv) == "1" {
		lockChild()
		return
	}

	path := filepath.Join(t.TempDir(), "harness.lock")

	// --- a stale file does not block ---------------------------------------
	//
	// A PID that is not ours and is not running, in a file nobody holds. The
	// implementation must never read, trust or signal it.
	if err := os.WriteFile(path, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}
	lk, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("a stale unlocked lock file blocked acquisition, which would "+
			"refuse every restart after the SIGKILL of V4.17: %v", err)
	}

	// The PID recorded is ours, and it was written only after the lock was held.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file holds pid %q, want %d", got, os.Getpid())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode %v, want 0600", fi.Mode().Perm())
	}

	// --- a second PROCESS is excluded --------------------------------------
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe,
		"-test.run=^TestInstanceLockExcludesASecondProcessAndAllowsAStalePID$")
	cmd.Env = append(os.Environ(), lockChildEnv+"=1", lockPathEnv+"="+path)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "LOCK_REFUSED") {
		t.Fatalf("a second process acquired the lock while the first held it. "+
			"Two harnesses on one account produce two q_local models that are "+
			"each individually consistent, each wrong, and neither able to see "+
			"that the other's fills moved the position.\n%s", out)
	}

	// --- releasing lets the next incarnation in ----------------------------
	if err := lk.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Close removed the lock file, which is an inode race: two "+
			"processes can end up locking different inodes at the same path "+
			"and both believe they are alone: %v", err)
	}
	again, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("the lock could not be retaken after Close: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// A relative path is refused: two harnesses started from two directories
	// would each hold a different file and both believe they were alone.
	if _, err := AcquireInstanceLock("harness.lock"); err == nil {
		t.Fatal("AcquireInstanceLock accepted a relative path")
	}
}

// lockChild is the second process. It must be refused.
func lockChild() {
	if _, err := AcquireInstanceLock(os.Getenv(lockPathEnv)); err != nil {
		fmt.Println("LOCK_REFUSED")
		return
	}
	fmt.Println("LOCK_ACQUIRED_BY_SECOND_PROCESS")
}

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"lip/harness/risk"
)

// The live floor is a local operational reserve for the SQLite WAL, anomaly
// journal and durable stop during recovery. It is deliberately separate from
// the measured 15 GiB floor for a full mutation round's Go build cache.
const operationsMinFreeBytes uint64 = 1 << 30

type operationsDiskSnapshot struct {
	MinFreeBytes  uint64
	ArtifactBytes uint64
}

// operationsDiskGuard belongs to the owner goroutine. Its first sample is
// immediate; subsequent samples are at most one minute apart while the owner
// is ticking. Unknown headroom has the same stop response as a low reading.
type operationsDiskGuard struct {
	enabled               bool
	db, journal, evidence string
	free                  func(string) (uint64, error)
	next                  time.Duration
	checked, stopped      bool
	last                  operationsDiskSnapshot
}

func (o *owner) checkRuntimeDisk(now time.Duration) {
	g := &o.operationsDisk
	if !g.enabled || g.stopped || (g.checked && now < g.next) {
		return
	}
	g.checked = true
	g.next = now + time.Minute
	probe := g.free
	if probe == nil {
		probe = freeBytesOnVolume
	}
	snap, err := checkOperationsDiskWithFree(g.db, g.journal, g.evidence, probe)
	g.last = snap
	if err == nil {
		return
	}
	g.stopped = true
	o.r.anom.raise(risk.Anomaly{Class: "DISK_HEADROOM", Sev: risk.SEV1,
		Text: fmt.Sprintf("operational storage is unsafe: %v; stop adding, keep observing and reducing; preserve the store and evidence for recovery", err)})
	o.requestStop("disk_headroom", "")
}

// checkOperationsDisk examines every filesystem holding active operational
// evidence. A caller must refuse a start on error, or request a durable stop
// while keeping observation and reduction alive when already running.
func checkOperationsDisk(db, journal, evidence string) (operationsDiskSnapshot, error) {
	return checkOperationsDiskWithFree(db, journal, evidence, freeBytesOnVolume)
}

func checkOperationsDiskWithFree(db, journal, evidence string,
	free func(string) (uint64, error)) (operationsDiskSnapshot, error) {
	paths := []string{db, db + "-wal", db + "-shm", journal}
	if evidence != "" {
		paths = append(paths, evidence)
	}
	var out operationsDiskSnapshot
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			return out, fmt.Errorf("operational artifact path %q is not absolute", path)
		}
		volumePath := filepath.Dir(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			// An artifact symlink can lead to another volume even when its
			// parent has ample space. Check the volume that receives writes.
			volumePath = filepath.Dir(resolved)
		} else if !errors.Is(err, os.ErrNotExist) {
			return out, fmt.Errorf("operational artifact %s cannot be resolved: %w", path, err)
		}
		available, err := free(volumePath)
		if err != nil {
			return out, fmt.Errorf("disk headroom for %s: %w", path, err)
		}
		if i == 0 || available < out.MinFreeBytes {
			out.MinFreeBytes = available
		}
		info, err := os.Stat(path)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return out, fmt.Errorf("operational artifact %s is not a regular file", path)
			}
			out.ArtifactBytes += uint64(info.Size())
		case errors.Is(err, os.ErrNotExist):
			// WAL/SHM and the evidence file can legitimately be absent before a run.
		default:
			return out, fmt.Errorf("operational artifact %s cannot be examined: %w", path, err)
		}
	}
	if out.MinFreeBytes < operationsMinFreeBytes {
		return out, fmt.Errorf("disk headroom %d bytes is below the %d-byte operational reserve; active store/evidence uses %d bytes",
			out.MinFreeBytes, operationsMinFreeBytes, out.ArtifactBytes)
	}
	return out, nil
}

func freeBytesOnVolume(dir string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	return fs.Bavail * uint64(fs.Bsize), nil
}

// confidence: high

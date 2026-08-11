package qual

import (
	"fmt"
	"os"
	"path/filepath"
)

type atomicFileOps struct {
	createTemp func(string, string) (*os.File, error)
	rename     func(string, string) error
	remove     func(string) error
	syncDir    func(string) error
}

func realAtomicFileOps() atomicFileOps {
	return atomicFileOps{
		createTemp: os.CreateTemp,
		rename:     os.Rename,
		remove:     os.Remove,
		syncDir: func(dir string) error {
			d, err := os.Open(dir)
			if err != nil {
				return err
			}
			if err := d.Sync(); err != nil {
				d.Close()
				return err
			}
			return d.Close()
		},
	}
}

// atomicWriteFile replaces path only after a same-directory temporary file is
// mode 0600, fully written, fsynced, and closed.  The directory fsync after
// rename makes the new name durable across power loss.
func atomicWriteFile(path string, data []byte, ops atomicFileOps) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := ops.createTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary checkpoint: %w", err)
	}
	tmpName := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = ops.remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary checkpoint: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary checkpoint: %w", err)
	}
	if err := ops.rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temporary checkpoint: %w", err)
	}
	keepTemp = false
	if err := ops.syncDir(dir); err != nil {
		return fmt.Errorf("sync checkpoint directory: %w", err)
	}
	return nil
}

// confidence: high

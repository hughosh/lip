package turnover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const stateMagic = "LIP-TURNOVER-STATE-1\n"
const maxStateBytes = 16 << 20

type stateEnvelope struct{ State *State }

// FileDurable is a single-owner atomic journal for the coordinator. Its lock is
// held for the handle lifetime. It does not replace the harness account lock or
// its ownership ledger. Initialization is explicit: a missing recovery journal
// must never be interpreted as an empty account.
type FileDurable struct {
	path string
	lock *os.File
}

func OpenFileDurable(path string, initialize bool) (*FileDurable, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("turnover state path must be absolute")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("turnover state already owned: %w", err)
	}
	f := &FileDurable{path: path, lock: lock}
	fail := func(err error) (*FileDurable, error) { f.Close(); return nil, err }
	if initialize {
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = fmt.Errorf("turnover state already exists")
			}
			return fail(err)
		}
		if err = f.write(nil); err != nil {
			return fail(err)
		}
	} else {
		if _, err = f.Load(context.Background()); err != nil {
			return fail(err)
		}
	}
	return f, nil
}

func (f *FileDurable) Close() error {
	if f.lock == nil {
		return nil
	}
	err := syscall.Flock(int(f.lock.Fd()), syscall.LOCK_UN)
	closeErr := f.lock.Close()
	f.lock = nil
	return errors.Join(err, closeErr)
}

func (f *FileDurable) Load(ctx context.Context) (*State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.lock == nil {
		return nil, fmt.Errorf("turnover state handle closed")
	}
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("turnover recovery state unavailable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("turnover state is not a readable regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, err
	}
	prefix := len(stateMagic) + sha256.Size
	if len(data) > maxStateBytes || len(data) <= prefix || string(data[:len(stateMagic)]) != stateMagic {
		return nil, fmt.Errorf("invalid turnover state envelope")
	}
	body := data[prefix:]
	digest := sha256.Sum256(body)
	if !bytes.Equal(digest[:], data[len(stateMagic):prefix]) {
		return nil, fmt.Errorf("turnover state checksum mismatch")
	}
	var envelope stateEnvelope
	decoder := gob.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode turnover state: %w", err)
	}
	var trailing stateEnvelope
	if err = decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing turnover state data")
	}
	if envelope.State != nil && envelope.State.Snapshot.Previous != nil {
		return nil, fmt.Errorf("recursive turnover state")
	}
	return envelope.State, nil
}

func (f *FileDurable) Replace(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Snapshot.Previous != nil {
		return fmt.Errorf("recursive turnover state")
	}
	return f.write(&state)
}

func (f *FileDurable) write(state *State) error {
	if f.lock == nil {
		return fmt.Errorf("turnover state handle closed")
	}
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(stateEnvelope{state}); err != nil {
		return err
	}
	digest := sha256.Sum256(payload.Bytes())
	data := append([]byte(stateMagic), digest[:]...)
	data = append(data, payload.Bytes()...)
	if len(data) > maxStateBytes {
		return fmt.Errorf("turnover state exceeds bound")
	}
	file, err := os.CreateTemp(filepath.Dir(f.path), ".turnover-state-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if err = os.Rename(name, f.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(f.path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	return errors.Join(err, dir.Close())
}

// confidence: high

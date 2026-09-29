package turnover

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"lip/harness/num"
)

func TestFileStateRestartRetainsObligationsAndFrozenCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turnover.state")
	if _, err := OpenFileDurable(path, false); err == nil {
		t.Fatal("missing recovery state became empty")
	}
	f, err := OpenFileDurable(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := OpenFileDurable(path, false); err == nil {
		t.Fatal("second owner acquired state")
	}
	state := State{Version: 1, StopAdding: true, Snapshot: Input{
		Positions: map[string]num.Qty{"OLD": 100}, FrozenCaps: map[Shard]num.Money{{ExchangeIndex: 2}: 50_000_000}, AggregateCap: 50_000_000,
		Orders: []Order{{ID: "pending", Ticker: "OLD", Quantity: 100, Price4: 4000, Ownership: Owned}},
	}}
	if err := f.Replace(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = OpenFileDurable(path, false)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := f.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.StopAdding || loaded.Snapshot.Positions["OLD"] != 100 || loaded.Snapshot.FrozenCaps[Shard{ExchangeIndex: 2}] != 50_000_000 || loaded.Snapshot.Orders[0].ID != "pending" {
		t.Fatalf("lost restart obligations: %+v", loaded)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
	f.Close()
	if _, err := OpenFileDurable(path, true); err == nil {
		t.Fatal("reinitialized existing recovery state")
	}
}

func TestFileStateCorruptionCannotBecomeEmptyRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	f, err := OpenFileDurable(path, true)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenFileDurable(path, false); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

func TestFileStateFailedReplacePreservesPriorState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	f, err := OpenFileDurable(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := State{Version: 1, StopAdding: true}
	if err = f.Replace(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = f.Replace(ctx, State{Version: 1}); err == nil {
		t.Fatal("cancelled replace accepted")
	}
	got, err := f.Load(context.Background())
	if err != nil || !got.StopAdding {
		t.Fatalf("lost prior stop: %+v %v", got, err)
	}
}

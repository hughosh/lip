package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/hstore"
	"lip/harness/rest"
)

type balanceWire struct {
	mu    sync.Mutex
	calls []time.Time
}

func (f *balanceWire) Do(_ context.Context, req rest.Request) (rest.Response, error) {
	if req.Method != "GET" || req.Path != "/portfolio/balance" {
		return rest.Response{}, fmt.Errorf("unexpected balance request %+v", req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, time.Now())
	switch len(f.calls) {
	case 1:
		return rest.Response{Status: 200, Body: []byte(`{"balance":10000}`)}, nil
	case 2:
		return rest.Response{Status: 200, Body: []byte(`{"balance":null}`)}, nil
	case 3:
		return rest.Response{Status: 200, Body: []byte(`{"balance":10100}`)}, nil
	default:
		return rest.Response{Status: 200, Body: []byte(`{"balance":null}`)}, nil
	}
}

func (f *balanceWire) times() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.calls...)
}

func TestBalanceTelemetryCadenceAndCompleteRows(t *testing.T) {
	dir := t.TempDir()
	store, err := hstore.Open(hstore.StoreConfig{
		DBPath:         filepath.Join(dir, "harness.db"),
		AnomalyLogPath: filepath.Join(dir, "anomaly.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	storeCtx, stopStore := context.WithCancel(context.Background())
	storeDone := make(chan struct{})
	go func() { store.Run(storeCtx); close(storeDone) }()
	defer func() {
		stopStore()
		<-storeDone
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	rcpt, err := store.BeginRun("runa", 1_700_000_000_000, cfg.Default())
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := awaitRunHandle(context.Background(), store, rcpt)
	if err != nil {
		t.Fatal(err)
	}
	wire := &balanceWire{}
	var stamped atomic.Int64
	start := time.Now()
	p := balanceTelemetry{
		source: rest.NewClient(wire), recorder: store, run: h,
		interval: 20 * time.Millisecond,
		nowMs:    func() int64 { return 1_700_000_000_000 + stamped.Add(60_000) },
		mono:     func() time.Duration { return time.Since(start) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var reported atomic.Int64
	go func() {
		p.runLoop(ctx, func(error) { reported.Add(1) })
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	var rows []hstore.BalancePollRow
	for len(rows) < 2 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for two complete balance rows; got %+v", rows)
		case <-time.After(5 * time.Millisecond):
			rows, err = store.Reader().BalancePolls()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	cancel()
	<-done
	if len(rows) != 2 || rows[0].BalanceCents != 10_000 || rows[1].BalanceCents != 10_100 {
		t.Fatalf("null balance wrote a good row or reward was lost: %+v", rows)
	}
	if reported.Load() == 0 {
		t.Fatal("malformed balance read was not reported")
	}
	times := wire.times()
	if len(times) < 3 || times[1].Sub(times[0]) < 10*time.Millisecond ||
		times[2].Sub(times[1]) < 10*time.Millisecond {
		t.Fatalf("balance reads did not follow configured 20ms cadence: %v", times)
	}
}

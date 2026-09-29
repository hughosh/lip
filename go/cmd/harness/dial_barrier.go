package main

import (
	"context"
	"errors"
	"net"
	"sync"
)

// net/http can detach a dial from its requesting goroutine. Registration at
// DialContext entry fences callbacks scheduled even after all request loops
// have joined, while already registered fallbacks can still report normally.
type dialBarrier struct {
	mu     sync.Mutex
	paused bool
	active int
	idle   chan struct{}
}

func (b *dialBarrier) enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.paused {
		return false
	}
	if b.active == 0 {
		b.idle = make(chan struct{})
	}
	b.active++
	return true
}
func (b *dialBarrier) leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active--
	if b.active == 0 {
		close(b.idle)
	}
}
func (b *dialBarrier) pause(ctx context.Context) error {
	b.mu.Lock()
	b.paused = true
	idle, active := b.idle, b.active
	b.mu.Unlock()
	if active == 0 {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *dialBarrier) resume() {
	b.mu.Lock()
	b.paused = false
	b.mu.Unlock()
}
func (nt *f6Net) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !nt.dials.enter() {
		return nil, errors.New("runtime transport is quiescing for final close")
	}
	defer nt.dials.leave()
	return nt.dialer.DialContext(ctx, network, address)
}

// confidence: high

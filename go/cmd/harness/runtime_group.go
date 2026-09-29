package main

import (
	"context"
	"fmt"
	"time"
)

// A refusal must give the owner and independent observers back promptly.
const finalCloseTimeout = 2 * time.Second

type stopRequest struct {
	ctx  context.Context
	done chan error
}

type runtimeTask struct {
	run    func(context.Context)
	cancel context.CancelFunc
	done   chan struct{}
}

// A group belongs to the serving owner. Each replacement waits for its own
// predecessor to join, even when cancellation returns before a callback does.
// Independent completed tasks can resume without waiting for a stuck peer.
// The store writer is deliberately outside these groups.
type runtimeGroup struct {
	tasks  []*runtimeTask
	paused bool
}

func (t *runtimeTask) start(parent context.Context) {
	previous := t.done
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	t.cancel, t.done = cancel, done
	go func() {
		defer close(done)
		if previous != nil {
			<-previous
		}
		if ctx.Err() == nil {
			t.run(ctx)
		}
	}()
}

func (g *runtimeGroup) start(parent context.Context, f func(context.Context)) {
	task := &runtimeTask{run: f}
	g.tasks = append(g.tasks, task)
	task.start(parent)
}

func (g *runtimeGroup) pause(ctx context.Context) error {
	g.paused = true
	for _, task := range g.tasks {
		task.cancel()
	}
	for _, task := range g.tasks {
		select {
		case <-task.done:
		case <-ctx.Done():
			return fmt.Errorf("joining runtime producers: %w", ctx.Err())
		}
	}
	return nil
}

func (g *runtimeGroup) resume(ctx context.Context) {
	if !g.paused {
		return
	}
	for _, task := range g.tasks {
		task.start(ctx)
	}
	g.paused = false
}

func (g *runtimeGroup) stop() { _ = g.pause(context.Background()) }

// confidence: high

package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

type clockWait struct {
	at   time.Time
	fire chan struct{}
}

type controlledClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan clockWait
}

func newTestRuntime(client *http.Client) (*pluginRuntime, *controlledClock) {
	clock := &controlledClock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local), waits: make(chan clockWait, 1)}
	p := newRuntime(client)
	p.now = clock.Now
	p.wait = clock.Wait
	return p, clock
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

func (c *controlledClock) Wait(ctx context.Context, delay time.Duration) bool {
	w := clockWait{at: c.Now().Add(delay), fire: make(chan struct{})}
	select {
	case c.waits <- w:
	case <-ctx.Done():
		return false
	}
	select {
	case <-w.fire:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (c *controlledClock) Await(t *testing.T) clockWait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not reach a controlled wait")
		return clockWait{}
	}
}

func (c *controlledClock) Fire(w clockWait) {
	c.Set(w.at)
	close(w.fire)
}

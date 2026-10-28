// Package gate is a first come, first served weighted gate. Flatpix uses it
// for render seats (weight 1), download slots (weight 1) and the scratch disk
// budget (weight in bytes).
package gate

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// ErrExceedsCapacity is returned when a request asks for more than the gate
// can ever grant.
var ErrExceedsCapacity = errors.New("gate: request exceeds capacity")

type waiter struct {
	n       int64
	ready   chan struct{}
	granted bool
}

// Gate grants up to capacity units at a time. Waiters are served strictly in
// arrival order: a small request never jumps ahead of a large one.
type Gate struct {
	mu      sync.Mutex
	cap     int64
	used    int64
	waiters list.List
}

// New makes a gate with the given capacity.
func New(capacity int64) *Gate { return &Gate{cap: capacity} }

// Capacity is the total number of units.
func (g *Gate) Capacity() int64 { return g.cap }

// InUse is the number of units currently granted.
func (g *Gate) InUse() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used
}

// Waiting is the number of callers queued.
func (g *Gate) Waiting() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiters.Len()
}

// Acquire waits for n units. It returns a release function, which is safe to
// call more than once. If ctx ends first it returns ctx.Err() and holds
// nothing. A grant that arrives at the instant the context ends is handed
// straight back.
func (g *Gate) Acquire(ctx context.Context, n int64) (release func(), err error) {
	if n > g.cap {
		return nil, ErrExceedsCapacity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.waiters.Len() == 0 && g.used+n <= g.cap {
		g.used += n
		g.mu.Unlock()
		return g.releaser(n), nil
	}
	w := &waiter{n: n, ready: make(chan struct{})}
	elem := g.waiters.PushBack(w)
	g.mu.Unlock()

	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			g.put(n) // granted at the instant the context ended: hand it back
			return nil, err
		}
		return g.releaser(n), nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.granted {
			g.mu.Unlock()
			g.put(n)
			return nil, ctx.Err()
		}
		wasFront := g.waiters.Front() == elem
		g.waiters.Remove(elem)
		if wasFront {
			g.grantLocked()
		}
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (g *Gate) releaser(n int64) func() {
	var once sync.Once
	return func() { once.Do(func() { g.put(n) }) }
}

func (g *Gate) put(n int64) {
	g.mu.Lock()
	g.used -= n
	g.grantLocked()
	g.mu.Unlock()
}

// grantLocked wakes waiters from the front while they fit.
func (g *Gate) grantLocked() {
	for {
		front := g.waiters.Front()
		if front == nil {
			return
		}
		w := front.Value.(*waiter)
		if g.used+w.n > g.cap {
			return
		}
		g.used += w.n
		w.granted = true
		g.waiters.Remove(front)
		close(w.ready)
	}
}

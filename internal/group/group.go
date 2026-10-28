// Package group collapses concurrent requests for the same output into one
// render, like singleflight, with reference counting.
//
//   - The render runs on its own context. It stays alive while at least one
//     requester is connected and is canceled when all of them have gone.
//   - The rendered resource is freed only when the last requester has
//     finished with it, and not before the optional finish step (the derived
//     cache upload) is done.
//   - While the resource exists, the key stays in the group, so a request
//     that arrives during the upload reuses the local file instead of
//     rendering the same thing again.
//   - A panic in the render or the finish step is contained, and the key is
//     removed so later requests do not hang on it.
package group

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/taktekhq/flatpix/internal/ferr"
)

// Resource is what a render produces. Free is called once, when nobody needs
// it any more.
type Resource interface{ Free() }

type call[R Resource] struct {
	refs       int
	workDone   bool
	finishDone bool
	val        R
	err        error
	done       chan struct{} // closed when work has returned
	cancel     context.CancelFunc
	freed      bool
}

// Group is the set of in-flight and finishing renders.
type Group[R Resource] struct {
	mu sync.Mutex
	m  map[string]*call[R]
}

// Lease is one requester's hold on the resource. Release it when the
// response has been sent.
type Lease[R Resource] struct {
	Value R
	g     *Group[R]
	key   string
	c     *call[R]
	once  sync.Once
}

// Release gives the resource back. It is safe to call more than once.
func (l *Lease[R]) Release() {
	l.once.Do(func() { l.g.unref(l.key, l.c) })
}

// Len is the number of keys held. Used by tests and metrics.
func (g *Group[R]) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

// Refs is the number of requesters holding key. Used by tests.
func (g *Group[R]) Refs(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.m[key]; c != nil {
		return c.refs
	}
	return 0
}

// Do returns a lease on the resource for key, running work if nobody else is.
// shared is true when this requester joined a render that was already running
// or finished. finish, if not nil, runs after a successful work (for example
// the cache upload); the key and the resource are kept until it returns.
// If ctx ends first, Do returns ctx.Err().
func (g *Group[R]) Do(ctx context.Context, key string,
	work func(ctx context.Context) (R, error),
	finish func(ctx context.Context, r R) error,
) (lease *Lease[R], shared bool, err error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call[R])
	}
	c := g.m[key]
	if c != nil {
		c.refs++
		shared = true
		g.mu.Unlock()
	} else {
		workCtx, cancel := context.WithCancel(context.Background())
		c = &call[R]{refs: 1, done: make(chan struct{}), cancel: cancel}
		g.m[key] = c
		g.mu.Unlock()
		go g.run(workCtx, key, c, work, finish)
	}

	select {
	case <-c.done:
	case <-ctx.Done():
		g.unref(key, c)
		return nil, shared, ctx.Err()
	}
	if c.err != nil {
		g.unref(key, c)
		return nil, shared, c.err
	}
	if err := ctx.Err(); err != nil {
		g.unref(key, c)
		return nil, shared, err
	}
	return &Lease[R]{Value: c.val, g: g, key: key, c: c}, shared, nil
}

func (g *Group[R]) run(ctx context.Context, key string, c *call[R],
	work func(ctx context.Context) (R, error),
	finish func(ctx context.Context, r R) error,
) {
	var val R
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("render_panic", "key", short(key), "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				err = ferr.New(ferr.ErrInternal, "render panicked")
			}
		}()
		val, err = work(ctx)
	}()

	g.mu.Lock()
	c.workDone = true
	if err != nil {
		c.err = err
		// Never keep a failure: the next request tries again.
		if g.m[key] == c {
			delete(g.m, key)
		}
	} else {
		c.val = val
	}
	c.cancel() // release the context's resources; the work is over
	close(c.done)
	g.mu.Unlock()
	if err != nil {
		return
	}

	if finish != nil {
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("finish_panic", "key", short(key), "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				}
			}()
			if ferr := finish(context.Background(), val); ferr != nil {
				slog.Warn("finish_failed", "key", short(key), "err", ferr)
			}
		}()
	}
	g.mu.Lock()
	c.finishDone = true
	g.maybeFreeLocked(key, c)
	g.mu.Unlock()
}

func (g *Group[R]) unref(key string, c *call[R]) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c.refs--
	if c.refs == 0 && !c.workDone {
		// Everybody left before the render finished: stop it, and make
		// sure a new request starts a fresh one.
		c.cancel()
		if g.m[key] == c {
			delete(g.m, key)
		}
		return
	}
	g.maybeFreeLocked(key, c)
}

// maybeFreeLocked frees the resource once the work and the finish step are
// done and nobody holds it. The key leaves the group at the same moment.
func (g *Group[R]) maybeFreeLocked(key string, c *call[R]) {
	if !(c.workDone && c.finishDone && c.refs == 0 && c.err == nil && !c.freed) {
		return
	}
	c.freed = true
	if g.m[key] == c {
		delete(g.m, key)
	}
	c.val.Free()
}

func short(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

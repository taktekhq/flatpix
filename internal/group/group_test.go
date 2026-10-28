package group

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taktekhq/flatpix/internal/ferr"
)

type res struct {
	name  string
	freed atomic.Int32
}

func (r *res) Free() { r.freed.Add(1) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOneWorkForConcurrentRequesters(t *testing.T) {
	var g Group[*res]
	var works atomic.Int32
	release := make(chan struct{})
	work := func(ctx context.Context) (*res, error) {
		works.Add(1)
		<-release
		return &res{name: "out"}, nil
	}
	const n = 20
	leases := make(chan *Lease[*res], n)
	var shared atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, sh, err := g.Do(context.Background(), "k", work, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if sh {
				shared.Add(1)
			}
			leases <- l
		}()
	}
	waitFor(t, func() bool { return g.Refs("k") == n })
	close(release)
	wg.Wait()
	close(leases)
	if works.Load() != 1 {
		t.Fatalf("work ran %d times, want 1", works.Load())
	}
	if shared.Load() != n-1 {
		t.Fatalf("shared = %d, want %d", shared.Load(), n-1)
	}
	var first *res
	var all []*Lease[*res]
	for l := range leases {
		if first == nil {
			first = l.Value
		}
		if l.Value != first {
			t.Fatal("requesters got different values")
		}
		all = append(all, l)
	}
	// The file stays until the last requester has finished sending it.
	for i, l := range all {
		if first.freed.Load() != 0 {
			t.Fatalf("freed after %d of %d releases", i, n)
		}
		l.Release()
		l.Release() // idempotent
	}
	if first.freed.Load() != 1 {
		t.Fatalf("freed %d times, want exactly 1", first.freed.Load())
	}
	if g.Len() != 0 {
		t.Fatalf("group still holds %d keys", g.Len())
	}
}

func TestEntryStaysWhileFinishRuns(t *testing.T) {
	var g Group[*res]
	var works atomic.Int32
	work := func(ctx context.Context) (*res, error) {
		works.Add(1)
		return &res{}, nil
	}
	finishGate := make(chan struct{})
	finish := func(ctx context.Context, r *res) error { <-finishGate; return nil }

	l1, _, err := g.Do(context.Background(), "k", work, finish)
	if err != nil {
		t.Fatal(err)
	}
	// The render is done but the cache upload is not: a new request must
	// reuse the local file instead of rendering again.
	l2, shared, err := g.Do(context.Background(), "k", work, finish)
	if err != nil {
		t.Fatal(err)
	}
	if !shared || works.Load() != 1 || l2.Value != l1.Value {
		t.Fatalf("shared=%v works=%d same=%v", shared, works.Load(), l2.Value == l1.Value)
	}
	l1.Release()
	l2.Release()
	if l1.Value.freed.Load() != 0 {
		t.Fatal("freed before the upload finished")
	}
	close(finishGate)
	waitFor(t, func() bool { return l1.Value.freed.Load() == 1 })
	waitFor(t, func() bool { return g.Len() == 0 })
}

func TestWorkContextCanceledWhenEveryoneLeaves(t *testing.T) {
	var g Group[*res]
	started := make(chan context.Context, 1)
	work := func(ctx context.Context) (*res, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c1, cancel1 := context.WithCancel(context.Background())
	c2, cancel2 := context.WithCancel(context.Background())
	errs := make(chan error, 2)
	go func() { _, _, err := g.Do(c1, "k", work, nil); errs <- err }()
	wctx := <-started
	go func() { _, _, err := g.Do(c2, "k", work, nil); errs <- err }()
	waitFor(t, func() bool { return g.Refs("k") == 2 })

	cancel1()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-wctx.Done():
		t.Fatal("work canceled while a requester is still connected")
	case <-time.After(20 * time.Millisecond):
	}
	cancel2()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-wctx.Done():
	case <-time.After(time.Second):
		t.Fatal("work not canceled after every requester left")
	}
	waitFor(t, func() bool { return g.Len() == 0 })
}

func TestOneRequesterLeavingDoesNotAffectOthers(t *testing.T) {
	var g Group[*res]
	release := make(chan struct{})
	work := func(ctx context.Context) (*res, error) {
		select {
		case <-release:
			return &res{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c1, cancel1 := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() { _, _, err := g.Do(c1, "k", work, nil); gone <- err }()
	waitFor(t, func() bool { return g.Refs("k") == 1 })
	stay := make(chan *Lease[*res], 1)
	go func() {
		l, _, err := g.Do(context.Background(), "k", work, nil)
		if err != nil {
			t.Error(err)
		}
		stay <- l
	}()
	waitFor(t, func() bool { return g.Refs("k") == 2 })
	cancel1()
	<-gone
	close(release)
	l := <-stay
	if l == nil || l.Value == nil {
		t.Fatal("the remaining requester got nothing")
	}
	l.Release()
}

func TestPanicRemovesTheKey(t *testing.T) {
	var g Group[*res]
	boom := func(ctx context.Context) (*res, error) { panic("render exploded") }
	_, _, err := g.Do(context.Background(), "k", boom, nil)
	if !errors.Is(err, ferr.ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
	// A later request must not hang on the dead entry.
	done := make(chan struct{})
	go func() {
		l, _, err := g.Do(context.Background(), "k", func(ctx context.Context) (*res, error) { return &res{}, nil }, nil)
		if err != nil {
			t.Error(err)
		} else {
			l.Release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a request after a panic hangs")
	}
}

func TestPanicInFinishIsContained(t *testing.T) {
	var g Group[*res]
	l, _, err := g.Do(context.Background(), "k",
		func(ctx context.Context) (*res, error) { return &res{}, nil },
		func(ctx context.Context, r *res) error { panic("upload exploded") })
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	waitFor(t, func() bool { return g.Len() == 0 })
	if l.Value.freed.Load() != 1 {
		t.Fatalf("freed = %d", l.Value.freed.Load())
	}
}

func TestErrorIsNotKept(t *testing.T) {
	var g Group[*res]
	var works atomic.Int32
	_, _, err := g.Do(context.Background(), "k", func(ctx context.Context) (*res, error) {
		works.Add(1)
		return nil, ferr.New(ferr.ErrBadInput, "nope")
	}, nil)
	if !errors.Is(err, ferr.ErrBadInput) {
		t.Fatalf("err = %v", err)
	}
	l, _, err := g.Do(context.Background(), "k", func(ctx context.Context) (*res, error) {
		works.Add(1)
		return &res{}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if works.Load() != 2 {
		t.Fatalf("works = %d, want 2 (an error must not be cached in the group)", works.Load())
	}
}

func TestDifferentKeysRunInParallel(t *testing.T) {
	var g Group[*res]
	var wg sync.WaitGroup
	var cur, max atomic.Int32
	for _, k := range []string{"a", "b", "c", "d"} {
		k := k
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, err := g.Do(context.Background(), k, func(ctx context.Context) (*res, error) {
				n := cur.Add(1)
				for {
					m := max.Load()
					if n <= m || max.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				cur.Add(-1)
				return &res{}, nil
			}, nil)
			if err == nil {
				l.Release()
			}
		}()
	}
	wg.Wait()
	if max.Load() < 2 {
		t.Fatalf("keys did not run in parallel (max %d)", max.Load())
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package gate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestNeverExceedsCapacity(t *testing.T) {
	g := New(3)
	var cur, max atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.Acquire(context.Background(), 1)
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			cur.Add(-1)
			rel()
		}()
	}
	wg.Wait()
	if max.Load() > 3 {
		t.Fatalf("max concurrent = %d, want <= 3", max.Load())
	}
	if g.InUse() != 0 || g.Waiting() != 0 {
		t.Fatalf("leaked: in use %d, waiting %d", g.InUse(), g.Waiting())
	}
}

func TestFIFO(t *testing.T) {
	g := New(1)
	hold, _ := g.Acquire(context.Background(), 1)
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.Acquire(context.Background(), 1)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			rel()
		}()
		waitFor(t, func() bool { return g.Waiting() == i+1 })
	}
	hold()
	wg.Wait()
	for i, v := range order {
		if v != i {
			t.Fatalf("order = %v, want first come first served", order)
		}
	}
}

func TestWaiterGivesUpWhenContextEnds(t *testing.T) {
	g := New(1)
	hold, _ := g.Acquire(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := g.Acquire(ctx, 1); errc <- err }()
	waitFor(t, func() bool { return g.Waiting() == 1 })
	cancel()
	if err := <-errc; err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if g.Waiting() != 0 {
		t.Fatalf("waiting = %d after cancel", g.Waiting())
	}
	hold()
	rel, err := g.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

func TestDeadlineGivesUp(t *testing.T) {
	g := New(1)
	hold, _ := g.Acquire(context.Background(), 1)
	defer hold()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := g.Acquire(ctx, 1)
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Fatal("returned before the deadline")
	}
}

func TestEndedContextTakesNothing(t *testing.T) {
	g := New(2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Acquire(ctx, 1); err == nil {
		t.Fatal("want an error for a context that already ended")
	}
	if g.InUse() != 0 {
		t.Fatalf("in use = %d, want 0", g.InUse())
	}
}

// A slot granted at the instant the context ends must go straight back.
func TestGrantAtTheInstantOfCancelIsHandedBack(t *testing.T) {
	for i := 0; i < 400; i++ {
		g := New(1)
		hold, _ := g.Acquire(context.Background(), 1)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			rel, err := g.Acquire(ctx, 1)
			if err == nil {
				// Granted before the cancel was seen: fine, give it back.
				rel()
			}
			errc <- err
		}()
		waitFor(t, func() bool { return g.Waiting() == 1 })
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); hold() }()
		wg.Wait()
		<-errc
		// Whatever happened, the gate must be clean and usable.
		waitFor(t, func() bool { return g.InUse() == 0 && g.Waiting() == 0 })
		rel, err := g.Acquire(context.Background(), 1)
		if err != nil {
			t.Fatalf("iteration %d: gate unusable: %v", i, err)
		}
		rel()
	}
}

func TestWeightedStrictFIFO(t *testing.T) {
	g := New(10)
	a, _ := g.Acquire(context.Background(), 6)
	bigGot := make(chan func(), 1)
	smallGot := make(chan func(), 1)
	go func() { rel, _ := g.Acquire(context.Background(), 10); bigGot <- rel }()
	waitFor(t, func() bool { return g.Waiting() == 1 })
	// 1 unit would fit next to the 6 held, but it arrived after the big one.
	go func() { rel, _ := g.Acquire(context.Background(), 1); smallGot <- rel }()
	waitFor(t, func() bool { return g.Waiting() == 2 })
	time.Sleep(10 * time.Millisecond)
	select {
	case <-smallGot:
		t.Fatal("the small request jumped the queue")
	default:
	}
	a()
	bigRel := <-bigGot
	select {
	case <-smallGot:
		t.Fatal("the small request was granted while the big one held everything")
	case <-time.After(10 * time.Millisecond):
	}
	bigRel()
	(<-smallGot)()
}

func TestMoreThanCapacityFailsAtOnce(t *testing.T) {
	g := New(2)
	if _, err := g.Acquire(context.Background(), 3); err != ErrExceedsCapacity {
		t.Fatalf("err = %v, want ErrExceedsCapacity", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	g := New(2)
	rel, _ := g.Acquire(context.Background(), 2)
	rel()
	rel()
	if g.InUse() != 0 {
		t.Fatalf("in use = %d", g.InUse())
	}
}

func TestForceCountsAgainstCapacity(t *testing.T) {
	g := New(10)
	rel := g.Force(15) // over capacity on purpose
	if g.InUse() != 15 {
		t.Fatalf("in use = %d", g.InUse())
	}
	got := make(chan func(), 1)
	go func() { r, _ := g.Acquire(context.Background(), 1); got <- r }()
	waitFor(t, func() bool { return g.Waiting() == 1 })
	rel()
	(<-got)()
}

func TestLeaseShrinkFreesTheDifference(t *testing.T) {
	g := New(100)
	l, err := g.AcquireLease(context.Background(), 80)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() { r, _ := g.Acquire(context.Background(), 50); got <- r }()
	waitFor(t, func() bool { return g.Waiting() == 1 })
	l.Shrink(10) // 90 units free now
	(<-got)()
	if g.InUse() != 10 {
		t.Fatalf("in use = %d, want 10", g.InUse())
	}
	l.Shrink(50) // never grows
	l.Release()
	l.Release()
	if g.InUse() != 0 {
		t.Fatalf("in use = %d, want 0", g.InUse())
	}
}

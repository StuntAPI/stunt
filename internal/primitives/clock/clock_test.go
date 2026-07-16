package clock

import (
	"sync"
	"testing"
	"time"
)

func TestNewVirtualClock(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}
}

func TestAdvance(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	c.Advance(2 * time.Second)
	want := start.Add(2 * time.Second)
	if !c.Now().Equal(want) {
		t.Fatalf("Now() = %v, want %v", c.Now(), want)
	}
}

func TestAfterNotFiredBeforeDue(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var fired bool
	c.After(2*time.Second, func() {
		fired = true
	})

	c.Advance(1 * time.Second)
	if fired {
		t.Fatal("callback fired after only 1s, should fire at 2s")
	}
}

func TestAfterFiredOnAdvance(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var fired bool
	c.After(2*time.Second, func() {
		fired = true
	})

	c.Advance(2 * time.Second)
	if !fired {
		t.Fatal("callback should have fired after advancing 2s")
	}
}

func TestAfterFiredExactlyOnTime(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var fireTime time.Time
	c.After(5*time.Second, func() {
		fireTime = c.Now()
	})

	c.Advance(5 * time.Second)
	if !fireTime.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("fireTime = %v, want %v", fireTime, start.Add(5*time.Second))
	}
}

func TestAfterFiresInOrder(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var order []int
	c.After(3*time.Second, func() { order = append(order, 3) })
	c.After(1*time.Second, func() { order = append(order, 1) })
	c.After(2*time.Second, func() { order = append(order, 2) })

	c.Advance(5 * time.Second)

	if len(order) != 3 {
		t.Fatalf("len(order) = %d, want 3", len(order))
	}
	if order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("order = %v, want [1 2 3]", order)
	}
}

func TestAfterMultipleAdvances(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var fired bool
	c.After(3*time.Second, func() { fired = true })

	c.Advance(1 * time.Second)
	if fired {
		t.Fatal("fired too early (1s)")
	}
	c.Advance(1 * time.Second)
	if fired {
		t.Fatal("fired too early (2s)")
	}
	c.Advance(1 * time.Second)
	if !fired {
		t.Fatal("should have fired at 3s")
	}
}

func TestMultipleCallbacksSameDeadline(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var mu sync.Mutex
	var fired []string
	mkCallback := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			fired = append(fired, name)
		}
	}

	c.After(2*time.Second, mkCallback("a"))
	c.After(2*time.Second, mkCallback("b"))
	c.After(2*time.Second, mkCallback("c"))

	c.Advance(2 * time.Second)

	if len(fired) != 3 {
		t.Fatalf("len(fired) = %d, want 3", len(fired))
	}
}

func TestAdvanceZero(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var fired bool
	c.After(1*time.Second, func() { fired = true })

	c.Advance(0) // should not fire anything
	if fired {
		t.Fatal("callback fired on zero advance")
	}
	if !c.Now().Equal(start) {
		t.Fatalf("Now() changed after zero advance")
	}
}

func TestTick(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var count int
	tk := c.Tick(2*time.Second, func() { count++ })

	c.Advance(2 * time.Second)
	if count != 1 {
		t.Fatalf("count = %d after 2s, want 1", count)
	}
	c.Advance(2 * time.Second)
	if count != 2 {
		t.Fatalf("count = %d after 4s, want 2", count)
	}
	c.Advance(2 * time.Second)
	if count != 3 {
		t.Fatalf("count = %d after 6s, want 3", count)
	}

	tk.Stop()
	c.Advance(2 * time.Second)
	if count != 3 {
		t.Fatalf("count = %d after Stop, want 3", count)
	}
}

func TestTickDoesNotFireBeforeFirstInterval(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var count int
	c.Tick(3*time.Second, func() { count++ })

	c.Advance(2 * time.Second)
	if count != 0 {
		t.Fatalf("count = %d, want 0 (should not fire before first interval)", count)
	}
	c.Advance(1 * time.Second)
	if count != 1 {
		t.Fatalf("count = %d after 3s, want 1", count)
	}
}

func TestRealClockNow(t *testing.T) {
	c := NewClock()

	before := time.Now()
	now := c.Now()
	after := time.Now()

	if now.Before(before) {
		t.Fatalf("Now() = %v is before time.Now() before call %v", now, before)
	}
	if now.After(after.Add(time.Millisecond)) {
		t.Fatalf("Now() = %v is after time.Now() after call %v", now, after)
	}
}

func TestRealClockAfter(t *testing.T) {
	c := NewClock()

	var fired bool
	c.After(50*time.Millisecond, func() { fired = true })

	// Give it time to fire.
	time.Sleep(150 * time.Millisecond)
	if !fired {
		t.Fatal("real After callback did not fire")
	}
}

func TestConcurrencySafe(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c := NewVirtualClock(start)

	var mu sync.Mutex
	var count int
	mkFn := func() func() {
		return func() {
			mu.Lock()
			count++
			mu.Unlock()
		}
	}

	// Register from multiple goroutines.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.After(1*time.Second, mkFn())
		}()
	}
	wg.Wait()

	c.Advance(2 * time.Second)
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
}

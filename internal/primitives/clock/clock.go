// Package clock provides a deterministic virtual clock for tests and a
// real-time clock for normal runs. Both modes support a simple scheduler
// for one-shot (After) and repeating (Tick) callbacks.
//
// # Threading model
//
// In virtual mode, all callbacks fire synchronously on the goroutine that
// calls Advance. Concurrent calls to After/Tick/Now are safe, but Advance
// should not be called concurrently with itself or from within a callback
// (doing so would deadlock or produce undefined ordering).
//
// In real mode, After callbacks fire on an internal timer goroutine managed
// by the standard library's time package; Tick callbacks fire in a dedicated
// goroutine per ticker. Now is safe for concurrent use in both modes.
package clock

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Clock is a time source with scheduling. It operates in either virtual
// (deterministic) or real-time mode.
type Clock struct {
	virtual bool
	now     time.Time // virtual mode: current simulated time

	mu     sync.Mutex
	timers []*scheduled // pending virtual timers
	nextID int          // monotonically increasing timer IDs
}

// scheduled is a pending callback in virtual mode.
type scheduled struct {
	id       int
	when     time.Time
	interval time.Duration // 0 = one-shot (After), >0 = repeating (Tick)
	fn       func()
	stopped  bool
}

// Ticker represents a repeating callback that can be stopped.
type Ticker struct {
	clock   *Clock
	id      int           // virtual mode: scheduled timer id
	stopCh  chan struct{} // real mode: closed to signal stop
	stopOnce sync.Once    // guards close(stopCh) against double-close
	stopMu   sync.Mutex   // guards the stopped flag in virtual mode
	stopped  bool         // virtual mode: whether Stop was called
}

// NewClock creates a real-time clock. Now returns time.Now and callbacks
// fire via the standard library timer mechanism.
func NewClock() *Clock {
	return &Clock{virtual: false}
}

// NewVirtualClock creates a deterministic clock starting at the given time.
// Now returns the simulated time, advanced only by explicit calls to Advance.
func NewVirtualClock(start time.Time) *Clock {
	return &Clock{virtual: true, now: start}
}

// Now returns the current time. In virtual mode this is the simulated time;
// in real mode it is time.Now().
func (c *Clock) Now() time.Time {
	if !c.virtual {
		return time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the virtual clock forward by d and fires all due timers.
// In virtual mode callbacks fire synchronously on the calling goroutine,
// ordered by their scheduled time (registration order breaks ties).
// Panics if called on a real-time clock.
//
// Advance must not be called concurrently with itself.
func (c *Clock) Advance(d time.Duration) {
	if !c.virtual {
		panic("clock: Advance called on real-time clock")
	}
	if d < 0 {
		panic(fmt.Sprintf("clock: cannot advance by negative duration %v", d))
	}

	c.mu.Lock()
	c.now = c.now.Add(d)
	newNow := c.now

	// Collect due timers, stable-sorted by scheduled time so earlier
	// deadlines fire first (registration order breaks ties).
	var due []*scheduled
	for _, t := range c.timers {
		if !t.stopped && !t.when.After(newNow) {
			due = append(due, t)
		}
	}
	c.mu.Unlock()

	sort.SliceStable(due, func(i, j int) bool {
		return due[i].when.Before(due[j].when)
	})

	// Mark fired one-shots so they can be pruned after.
	fired := make(map[int]bool, len(due))

	// Fire callbacks outside the lock to avoid deadlocks if a callback
	// calls Now (which would contend on the same mutex).
	for _, t := range due {
		t.fn()
		if t.interval == 0 {
			fired[t.id] = true
		}
	}

	// Handle repeating timers: reschedule and prune fired one-shots.
	c.mu.Lock()
	var alive []*scheduled
	for _, t := range c.timers {
		if t.stopped {
			continue
		}
		if t.interval > 0 {
			// Advance the when to the next interval boundary after now.
			for !t.when.After(newNow) {
				t.when = t.when.Add(t.interval)
			}
			alive = append(alive, t)
		} else if !fired[t.id] {
			// Unfired one-shot: keep it for a future Advance.
			alive = append(alive, t)
		}
	}
	c.timers = alive
	c.mu.Unlock()
}

// After registers a one-shot callback to run after d has elapsed.
// In virtual mode the callback fires during the next Advance that crosses d.
// In real mode the callback fires via time.AfterFunc on an internal goroutine.
func (c *Clock) After(d time.Duration, fn func()) {
	if d < 0 {
		d = 0
	}
	if !c.virtual {
		time.AfterFunc(d, fn)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, &scheduled{
		id:   c.nextID,
		when: c.now.Add(d),
		fn:   fn,
	})
	c.nextID++
}

// Tick registers a repeating callback that fires every d. The returned
// Ticker can be used to stop the callback. In virtual mode the callback
// fires during each Advance that crosses a d boundary. In real mode the
// callback fires in a dedicated goroutine via time.Ticker.
func (c *Clock) Tick(d time.Duration, fn func()) *Ticker {
	if d <= 0 {
		panic(fmt.Sprintf("clock: Tick duration must be positive, got %v", d))
	}

	if !c.virtual {
		stopCh := make(chan struct{})
		go func() {
			ticker := time.NewTicker(d)
			defer ticker.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
					fn()
				}
			}
		}()
		return &Ticker{clock: c, stopCh: stopCh}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.timers = append(c.timers, &scheduled{
		id:       id,
		when:     c.now.Add(d),
		interval: d,
		fn:       fn,
	})
	c.nextID++
	return &Ticker{clock: c, id: id}
}

// Stop stops the ticker. After Stop returns, the callback will not fire again.
// Calling Stop more than once is a no-op and is safe for concurrent use.
func (t *Ticker) Stop() {
	if t.stopCh != nil {
		// Real mode: sync.Once ensures we close exactly once even under
		// concurrent Stop calls (close of closed channel panics).
		t.stopOnce.Do(func() {
			close(t.stopCh)
		})
		return
	}
	// Virtual mode: guard against concurrent Stop with the ticker's own mutex.
	t.stopMu.Lock()
	if t.stopped {
		t.stopMu.Unlock()
		return
	}
	t.stopped = true
	t.stopMu.Unlock()

	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.timers {
		if s.id == t.id {
			s.stopped = true
			return
		}
	}
}

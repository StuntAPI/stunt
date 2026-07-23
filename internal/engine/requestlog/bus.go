package requestlog

import "sync"

// Bus is a fan-out publisher of captured entries. Subscribers receive only
// events published AFTER they subscribe (no history); slow subscribers have
// events dropped (non-blocking) so a parked client never blocks recorders.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan Entry]struct{}
}

// NewBus builds an empty bus.
func NewBus() *Bus { return &Bus{subs: map[chan Entry]struct{}{}} }

// Subscribe returns a channel of future entries and a cancel func to remove it.
// The channel is buffered (128); overflow is dropped on publish.
func (b *Bus) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 128)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Publish sends e to all subscribers, non-blocking (drops on full).
func (b *Bus) Publish(e Entry) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber is slow → drop
		}
	}
}

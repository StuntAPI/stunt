package requestlog_test

import (
	"testing"
	"time"

	"stuntapi.com/stunt/internal/engine/requestlog"
)

func TestBusFanOutAndDropOnSlow(t *testing.T) {
	b := requestlog.NewBus()
	ch, cancel := b.Subscribe()
	t.Cleanup(cancel)

	b.Publish(requestlog.Entry{Seq: 1})
	b.Publish(requestlog.Entry{Seq: 2})

	select {
	case e := <-ch:
		if e.Seq != 1 {
			t.Fatalf("first event seq = %d, want 1", e.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	// second subscriber gets subsequent events only (fan-out, no history)
	ch2, cancel2 := b.Subscribe()
	defer cancel2()
	b.Publish(requestlog.Entry{Seq: 3})
	e := <-ch2
	if e.Seq != 3 {
		t.Fatalf("late subscriber seq = %d, want 3", e.Seq)
	}
}

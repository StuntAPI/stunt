package rules

import "testing"

func TestRNGDeterministic(t *testing.T) {
	a := NewRNG(42)
	b := NewRNG(42)
	for i := 0; i < 100; i++ {
		if a.Percent() != b.Percent() {
			t.Fatalf("rng diverged at roll %d", i)
		}
	}
}

func TestRNGChanceGate(t *testing.T) {
	r := NewRNG(42)
	// With chance 0, should never fire (Percent returns 1..100).
	for i := 0; i < 50; i++ {
		if r.RollChance(0) {
			t.Fatalf("chance 0 fired")
		}
	}
	// With chance 100, should always fire.
	for i := 0; i < 50; i++ {
		if !r.RollChance(100) {
			t.Fatalf("chance 100 did not fire")
		}
	}
}

package rules

import (
	"strings"
	"testing"
)

func TestFakerDeterministic(t *testing.T) {
	// Two fresh instances with the same seed produce the same first value
	// (i.e. the same deterministic sequence start).
	if NewFaker(42).Email() != NewFaker(42).Email() {
		t.Fatal("faker not deterministic across same-seed instances")
	}
	// A different seed must (almost certainly) produce a different value.
	if NewFaker(42).Email() == NewFaker(7).Email() {
		t.Fatal("faker produced identical values for different seeds")
	}
}

func TestFakerProducesValues(t *testing.T) {
	f := NewFaker(7)
	if !strings.Contains(f.Email(), "@") {
		t.Fatalf("Email() = %q, want something with @", f.Email())
	}
	if f.Name() == "" {
		t.Fatal("Name() empty")
	}
	id := f.ID("ch")
	if !strings.HasPrefix(id, "ch_") {
		t.Fatalf("ID(\"ch\") = %q, want ch_ prefix", id)
	}
}

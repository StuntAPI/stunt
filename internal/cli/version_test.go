package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionPrints(t *testing.T) {
	var out bytes.Buffer
	if err := runVersion(&out); err != nil {
		t.Fatalf("runVersion: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "stunt ") {
		t.Fatalf("expected output to start with 'stunt ', got %q", got)
	}
}

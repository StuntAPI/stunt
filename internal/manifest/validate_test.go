package manifest

import "testing"

func TestValidateOK(t *testing.T) {
	m := &Manifest{Version: 1, Network: Network{Mode: "port", BasePort: 9000}, Services: map[string]Service{"x": {}}}
	if err := Validate(m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		m    *Manifest
	}{
		{"missing version", &Manifest{Network: Network{Mode: "port", BasePort: 9000}}},
		{"unsupported version", &Manifest{Version: 2, Network: Network{Mode: "port", BasePort: 9000}}},
		{"no services", &Manifest{Version: 1, Network: Network{Mode: "port", BasePort: 9000}}},
		{"empty mode", &Manifest{Version: 1, Network: Network{BasePort: 9000}, Services: map[string]Service{"x": {}}}},
		{"zero base_port", &Manifest{Version: 1, Network: Network{Mode: "port"}, Services: map[string]Service{"x": {}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate(c.m); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

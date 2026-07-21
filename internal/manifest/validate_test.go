package manifest

import (
	"testing"

	"stuntapi.com/stunt/internal/rules"
)

func TestValidateOK(t *testing.T) {
	m := &Manifest{Version: 1, Network: Network{Mode: "port", BasePort: 9000}, Services: map[string]Service{"x": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}
	if err := Validate(m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSubdomainOK(t *testing.T) {
	m := &Manifest{Version: 1, Network: Network{Mode: "subdomain", TLD: "localhost"}, Services: map[string]Service{"x": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}
	if err := Validate(m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSubdomainBasePortOptional(t *testing.T) {
	// base_port not required for subdomain mode
	m := &Manifest{Version: 1, Network: Network{Mode: "subdomain"}, Services: map[string]Service{"x": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}
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
		{"service without adapter or rules", &Manifest{Version: 1, Network: Network{Mode: "port", BasePort: 9000}, Services: map[string]Service{"x": {}}}},
		{"bad service name (newline)", &Manifest{Version: 1, Network: Network{Mode: "port", BasePort: 9000}, Services: map[string]Service{"ev\nil": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}},
		{"bad service name (space)", &Manifest{Version: 1, Network: Network{Mode: "port", BasePort: 9000}, Services: map[string]Service{"ev il": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}},
		{"bad TLD (newline)", &Manifest{Version: 1, Network: Network{Mode: "subdomain", TLD: "ev\nil"}, Services: map[string]Service{"x": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}},
		{"bad TLD (space)", &Manifest{Version: 1, Network: Network{Mode: "subdomain", TLD: "ev il"}, Services: map[string]Service{"x": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate(c.m); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

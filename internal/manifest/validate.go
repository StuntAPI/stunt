package manifest

import (
	"fmt"
	"regexp"
	"sort"

	"stuntapi.com/stunt/internal/rules"
)

// validNameRe matches safe DNS-label-like names: letters, digits, dots,
// underscores, and hyphens. This prevents injection of newlines or other
// special characters into hosts files and generated configs (C1).
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks structural invariants. Returns a multi-line error on failure.
func Validate(m *Manifest) error {
	if m.Version == 0 {
		return fmt.Errorf("manifest: 'version' is required")
	}
	if m.Version != 1 {
		return fmt.Errorf("manifest: unsupported version %d (only 1 is supported)", m.Version)
	}
	if len(m.Services) == 0 {
		return fmt.Errorf("manifest: at least one service is required")
	}
	if m.Network.Mode == "" {
		return fmt.Errorf("manifest: network.mode is required")
	}
	switch m.Network.Mode {
	case "port":
		if m.Network.BasePort <= 0 {
			return fmt.Errorf("manifest: network.base_port must be > 0 for port mode")
		}
	case "subdomain":
		// base_port is optional in subdomain mode; the engine auto-binds to
		// a free high port and the proxy listens on the configured --port.
	default:
		return fmt.Errorf("manifest: network.mode %q not supported (use 'port' or 'subdomain')", m.Network.Mode)
	}
	// Validate TLD for subdomain mode (C1: prevent injection).
	if m.Network.Mode == "subdomain" {
		tld := m.Network.TLD
		if tld == "" {
			tld = "localhost"
		}
		if !validNameRe.MatchString(tld) {
			return fmt.Errorf("manifest: network.tld %q contains invalid characters (allowed: letters, digits, dots, underscores, hyphens)", tld)
		}
	}

	// Deterministic order for stable errors.
	names := make([]string, 0, len(m.Services))
	for n := range m.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		// Validate service name (C1: prevent injection into hosts/configs).
		if !validNameRe.MatchString(n) {
			return fmt.Errorf("manifest: service name %q contains invalid characters (allowed: letters, digits, dots, underscores, hyphens)", n)
		}
		s := m.Services[n]
		// A service must declare at least one of an adapter or rules.
		if s.Adapter == "" && len(s.Rules) == 0 {
			return fmt.Errorf("manifest: service %q must have at least one of 'adapter' or 'rules'", n)
		}
		if s.MaxBodyBytes < 0 {
			return fmt.Errorf("manifest: service %q max_body_bytes must be >= 0 (0 = default 1 MiB)", n)
		}
		// Only validate rules that exist.
		if err := validateRules(s.Rules, fmt.Sprintf("service %q", n)); err != nil {
			return err
		}
		pnames := make([]string, 0, len(s.Profiles))
		for p := range s.Profiles {
			pnames = append(pnames, p)
		}
		sort.Strings(pnames)
		for _, p := range pnames {
			if !validNameRe.MatchString(p) {
				return fmt.Errorf("manifest: service %q profile %q contains invalid characters (allowed: letters, digits, dots, underscores, hyphens)", n, p)
			}
			if err := validateRules(s.Profiles[p].Rules, fmt.Sprintf("service %q profile %q", n, p)); err != nil {
				return err
			}
		}
	}
	// Global presets: names and service references are checked here; the
	// assigned profile NAMES are validated at activation time (adapters
	// may author profiles the manifest cannot see).
	gnames := make([]string, 0, len(m.Profiles))
	for g := range m.Profiles {
		gnames = append(gnames, g)
	}
	sort.Strings(gnames)
	for _, g := range gnames {
		if !validNameRe.MatchString(g) {
			return fmt.Errorf("manifest: profile %q contains invalid characters (allowed: letters, digits, dots, underscores, hyphens)", g)
		}
		svcs := make([]string, 0, len(m.Profiles[g].Set))
		for s := range m.Profiles[g].Set {
			svcs = append(svcs, s)
		}
		sort.Strings(svcs)
		for _, s := range svcs {
			if _, ok := m.Services[s]; !ok {
				return fmt.Errorf("manifest: profile %q references unknown service %q", g, s)
			}
		}
	}
	return nil
}

// validateRules shallowly checks one rules list: every rule needs something
// to respond with.
func validateRules(rs []rules.Rule, ctx string) error {
	for i, r := range rs {
		if r.Respond.Status == 0 && r.Respond.Behavior == "" && r.Respond.Body == nil {
			return fmt.Errorf("manifest: %s rule[%d] has no respond (status/body/behavior)", ctx, i)
		}
	}
	return nil
}

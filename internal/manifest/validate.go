package manifest

import (
	"fmt"
	"sort"
)

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
	// Deterministic order for stable errors.
	names := make([]string, 0, len(m.Services))
	for n := range m.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := m.Services[n]
		// A service must declare at least one of an adapter or rules.
		if s.Adapter == "" && len(s.Rules) == 0 {
			return fmt.Errorf("manifest: service %q must have at least one of 'adapter' or 'rules'", n)
		}
		// Only validate rules that exist.
		for i, r := range s.Rules {
			if r.Respond.Status == 0 && r.Respond.Behavior == "" && r.Respond.Body == nil {
				return fmt.Errorf("manifest: service %q rule[%d] has no respond (status/body/behavior)", n, i)
			}
		}
	}
	return nil
}

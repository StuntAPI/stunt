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
	if m.Network.Mode != "port" {
		return fmt.Errorf("manifest: network.mode %q not supported in this build (use 'port')", m.Network.Mode)
	}
	if m.Network.BasePort <= 0 {
		return fmt.Errorf("manifest: network.base_port must be > 0")
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

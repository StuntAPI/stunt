package engine

import (
	"fmt"
	"sort"
	"strings"
)

// profileDef is one activatable profile on a service, from either source.
type profileDef struct {
	Description string
	Source      string // "manifest" (stunt.yaml service profiles) or "adapter"
}

// ProfileInfo describes one activatable profile for catalog output.
type ProfileInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
}

// ProfileCatalog is the full activation surface: global presets plus every
// service's profiles. The shape the dashboard and `stunt profile list` render.
type ProfileCatalog struct {
	Presets  []ProfileInfo            `json:"presets"`
	Services map[string][]ProfileInfo `json:"services"`
}

// profileGetter returns the closure handed to a service's handler builtins
// so profile_active() observes live activation state.
func (e *Engine) profileGetter(name string) func() string {
	return func() string { return e.Profile(name) }
}

// Profile returns the service's active profile name ("" when none).
func (e *Engine) Profile(service string) string {
	e.profileMu.RLock()
	defer e.profileMu.RUnlock()
	return e.activeProfile[service]
}

// ActiveGlobal returns the name of the global preset that produced the
// current assignment ("" when profiles were set per-service or none are).
func (e *Engine) ActiveGlobal() string {
	e.profileMu.RLock()
	defer e.profileMu.RUnlock()
	return e.activeGlobal
}

// SetProfile activates by bare name. Resolution order:
//  1. a global preset of that name — applies its whole assignment;
//  2. otherwise the name must be a profile of exactly ONE service —
//     activating it there;
//  3. zero matches or an ambiguous match is an error that describes the
//     full available set, so humans and agents can self-correct.
//
// "" clears every service's profile (deactivate-all).
func (e *Engine) SetProfile(name string) error {
	if name == "" {
		e.profileMu.Lock()
		e.activeProfile = make(map[string]string)
		e.activeGlobal = ""
		e.profileMu.Unlock()
		return nil
	}
	if gp, ok := e.manifest.Profiles[name]; ok {
		// Validate the whole assignment before mutating anything.
		for svc, pname := range gp.Set {
			if _, err := e.checkServiceProfile(svc, pname); err != nil {
				return fmt.Errorf("profile %q: %w", name, err)
			}
		}
		e.profileMu.Lock()
		e.activeProfile = make(map[string]string, len(gp.Set))
		for svc, pname := range gp.Set {
			e.activeProfile[svc] = pname
		}
		e.activeGlobal = name
		e.profileMu.Unlock()
		return nil
	}
	// Not a preset: resolve against every service's profiles.
	var matches []string
	for _, svc := range e.serviceNames() {
		if _, ok := e.serviceProfileDefs(svc)[name]; ok {
			matches = append(matches, svc)
		}
	}
	switch len(matches) {
	case 1:
		return e.SetServiceProfile(matches[0], name)
	case 0:
		return fmt.Errorf("unknown profile %q — nothing defines it. %s", name, e.describeAvailable())
	default:
		return fmt.Errorf("profile %q is ambiguous — defined by services %s; activate it with an explicit service", name, strings.Join(matches, ", "))
	}
}

// SetServiceProfile activates (or, with "", clears) one service's profile.
func (e *Engine) SetServiceProfile(service, name string) error {
	if name == "" {
		e.profileMu.Lock()
		delete(e.activeProfile, service)
		// A per-service change no longer matches the global assignment, if any.
		e.activeGlobal = ""
		e.profileMu.Unlock()
		return nil
	}
	if _, err := e.checkServiceProfile(service, name); err != nil {
		return err
	}
	e.profileMu.Lock()
	e.activeProfile[service] = name
	e.activeGlobal = ""
	e.profileMu.Unlock()
	return nil
}

// checkServiceProfile validates that service exists and defines name.
func (e *Engine) checkServiceProfile(service, name string) (profileDef, error) {
	defs := e.serviceProfileDefs(service)
	if len(defs) == 0 {
		if _, _, err := e.serviceExists(service); err != nil {
			return profileDef{}, err
		}
		return profileDef{}, fmt.Errorf("service %s defines no profiles. %s", service, e.describeAvailable())
	}
	def, ok := defs[name]
	if !ok {
		var names []string
		for n := range defs {
			names = append(names, n)
		}
		sort.Strings(names)
		return profileDef{}, fmt.Errorf("service %s has no profile %q (has: %s)", service, name, strings.Join(names, ", "))
	}
	return def, nil
}

// serviceExists reports whether the manifest declares the service, with the
// nearest distinct services for the error message.
func (e *Engine) serviceExists(service string) (bool, []string, error) {
	if _, ok := e.manifest.Services[service]; ok {
		return true, nil, nil
	}
	names := e.serviceNames()
	return false, names, fmt.Errorf("unknown service %q (manifest declares: %s)", service, strings.Join(names, ", "))
}

// serviceProfileDefs merges the service's stunt.yaml-declared profiles with
// its adapter's authored ones (manifest wins on a name collision — explicit
// local config outranks what a bundled adapter ships).
func (e *Engine) serviceProfileDefs(service string) map[string]profileDef {
	defs := make(map[string]profileDef)
	if st := e.states[service]; st != nil && st.adapter != nil {
		for n, desc := range st.adapter.Profiles {
			defs[n] = profileDef{Description: desc, Source: "adapter"}
		}
	}
	if svc, ok := e.manifest.Services[service]; ok {
		for n, p := range svc.Profiles {
			defs[n] = profileDef{Description: p.Description, Source: "manifest"}
		}
	}
	return defs
}

// AvailableProfiles builds the full catalog: global presets plus each
// service's profiles, sorted for stable output.
func (e *Engine) AvailableProfiles() ProfileCatalog {
	cat := ProfileCatalog{Services: make(map[string][]ProfileInfo)}
	for n, gp := range e.manifest.Profiles {
		cat.Presets = append(cat.Presets, ProfileInfo{Name: n, Description: gp.Description, Source: "manifest"})
	}
	for _, svc := range e.serviceNames() {
		defs := e.serviceProfileDefs(svc)
		var infos []ProfileInfo
		for n, d := range defs {
			infos = append(infos, ProfileInfo{Name: n, Description: d.Description, Source: d.Source})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		if len(infos) > 0 {
			cat.Services[svc] = infos
		}
	}
	sort.Slice(cat.Presets, func(i, j int) bool { return cat.Presets[i].Name < cat.Presets[j].Name })
	return cat
}

// describeAvailable renders the activation surface as one compact line for
// error messages — humans and agents should be able to pick the right name
// from the error alone, without a second command.
func (e *Engine) describeAvailable() string {
	cat := e.AvailableProfiles()
	var parts []string
	if len(cat.Presets) > 0 {
		var ps []string
		for _, p := range cat.Presets {
			ps = append(ps, p.Name)
		}
		parts = append(parts, "presets: "+strings.Join(ps, ", "))
	}
	for _, svc := range e.serviceNames() {
		if infos, ok := cat.Services[svc]; ok {
			var ns []string
			for _, i := range infos {
				ns = append(ns, i.Name)
			}
			parts = append(parts, fmt.Sprintf("%s: %s", svc, strings.Join(ns, ", ")))
		}
	}
	if len(parts) == 0 {
		return "no profiles are defined (declare them under a service's profiles: key, as adapter.yaml profiles:, or as a top-level preset)"
	}
	return "available — " + strings.Join(parts, "; ")
}

// ProfileView is the complete profile state for one glance: what is active
// and everything that could be activated.
type ProfileView struct {
	ActiveGlobal string            `json:"active_global"`
	Active       map[string]string `json:"active"`
	Catalog      ProfileCatalog    `json:"catalog"`
}

// ProfileSnapshot returns the current activation state plus the catalog.
func (e *Engine) ProfileSnapshot() ProfileView {
	e.profileMu.RLock()
	active := make(map[string]string, len(e.activeProfile))
	for k, v := range e.activeProfile {
		active[k] = v
	}
	global := e.activeGlobal
	e.profileMu.RUnlock()
	if active == nil {
		active = make(map[string]string)
	}
	return ProfileView{ActiveGlobal: global, Active: active, Catalog: e.AvailableProfiles()}
}

// serviceNames returns the manifest's service names, sorted.
func (e *Engine) serviceNames() []string {
	names := make([]string, 0, len(e.manifest.Services))
	for n := range e.manifest.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

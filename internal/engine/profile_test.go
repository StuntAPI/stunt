package engine

import (
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

func profileManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	okBody := func(msg string) *rules.Body { return &rules.Body{Inline: map[string]any{"msg": msg}} }
	flake := rules.Rule{
		Match:   rules.Match{Method: "GET", Path: "/**"},
		When:    &rules.When{Chance: 100},
		Respond: rules.Respond{Status: 503, Body: &rules.Body{Inline: map[string]any{"error": "profile-flake"}}},
	}
	slow := rules.Rule{
		Match:   rules.Match{Method: "GET", Path: "/ok"},
		Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"slow": true}}, LatencyMS: 1},
	}
	okRule := func(msg string) rules.Rule {
		return rules.Rule{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200, Body: okBody(msg)}}
	}
	hello := manifest.Service{
		Rules: []rules.Rule{okRule("hi")},
		Profiles: map[string]manifest.ServiceProfile{
			"chaos":   {Description: "everything flakes", Rules: []rules.Rule{flake}},
			"slow-ok": {Rules: []rules.Rule{slow}},
		},
	}
	other := manifest.Service{
		Rules:    []rules.Rule{okRule("other")},
		Profiles: map[string]manifest.ServiceProfile{"chaos": {Rules: []rules.Rule{flake}}},
	}
	return &manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{"hello": hello, "other": other},
		Profiles: map[string]manifest.GlobalProfile{
			"launch-day": {Description: "both flake", Set: map[string]string{"hello": "chaos", "other": "chaos"}},
		},
	}
}

// The core property base rules cannot offer: an active profile's rules
// intercept a route that is otherwise handled (here by a base rule).
func TestProfileOverridesDispatch(t *testing.T) {
	// HTTPServerForTest serves the first map-ordered service; pin one.
	m := profileManifest(t)
	delete(m.Services, "other")
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	addr, shutdown := startOnFreePort(t, e)
	defer shutdown()

	body, status := get(t, addr+"/ok")
	if status != 200 || !strings.Contains(body, "hi") {
		t.Fatalf("baseline -> %d %q", status, body)
	}
	if err := e.SetServiceProfile("hello", "chaos"); err != nil {
		t.Fatal(err)
	}
	body, status = get(t, addr+"/ok")
	if status != 503 || !strings.Contains(body, "profile-flake") {
		t.Fatalf("under profile -> %d %q", status, body)
	}
	// Deactivate restores the exact baseline behavior.
	if err := e.SetServiceProfile("hello", ""); err != nil {
		t.Fatal(err)
	}
	body, status = get(t, addr+"/ok")
	if status != 200 || !strings.Contains(body, "hi") {
		t.Fatalf("after deactivate -> %d %q", status, body)
	}
}

// An active profile with no matching rule falls through to normal dispatch.
func TestProfileFallsThroughWhenNoRuleMatches(t *testing.T) {
	// HTTPServerForTest serves the first map-ordered service; pin one.
	m := profileManifest(t)
	delete(m.Services, "other")
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	addr, shutdown := startOnFreePort(t, e)
	defer shutdown()

	if err := e.SetServiceProfile("hello", "slow-ok"); err != nil {
		t.Fatal(err)
	}
	// slow-ok only matches GET /ok; any other path falls through.
	if _, status := get(t, addr+"/no-match"); status != 404 {
		t.Fatalf("unmatched path under profile -> %d, want fall-through 404", status)
	}
	body, status := get(t, addr+"/ok")
	if status != 200 || !strings.Contains(body, "slow") {
		t.Fatalf("GET /ok under slow-ok -> %d %q", status, body)
	}
}

// Global presets assign profiles across services and report themselves.
func TestProfileGlobalPreset(t *testing.T) {
	// Both services stay: the preset references both, and its 503/200
	// assertions are identical on either, so map order does not matter.
	e, err := New(profileManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	addr, shutdown := startOnFreePort(t, e)
	defer shutdown()

	if err := e.SetProfile("launch-day"); err != nil {
		t.Fatal(err)
	}
	if e.ActiveGlobal() != "launch-day" {
		t.Fatalf("active global = %q", e.ActiveGlobal())
	}
	if got := e.Profile("hello"); got != "chaos" || e.Profile("other") != "chaos" {
		t.Fatalf("preset assignment = hello:%q other:%q", got, e.Profile("other"))
	}
	if _, status := get(t, addr+"/ok"); status != 503 {
		t.Fatalf("hello under preset -> %d", status)
	}
	// A per-service change invalidates the global marker.
	if err := e.SetServiceProfile("other", ""); err != nil {
		t.Fatal(err)
	}
	if e.ActiveGlobal() != "" {
		t.Fatalf("global marker after per-service change = %q, want cleared", e.ActiveGlobal())
	}
	// Deactivate-all clears everything.
	if err := e.SetProfile(""); err != nil {
		t.Fatal(err)
	}
	if e.Profile("hello") != "" || e.ActiveGlobal() != "" {
		t.Fatal("deactivate-all left state behind")
	}
	if _, status := get(t, addr+"/ok"); status != 200 {
		t.Fatalf("after deactivate-all -> %d", status)
	}
}

// Bare-name resolution: unique match activates; ambiguity and unknown names
// produce descriptive errors that list what IS available.
func TestProfileResolutionErrors(t *testing.T) {
	e, err := New(profileManifest(t))
	if err != nil {
		t.Fatal(err)
	}

	// "chaos" exists on both services → ambiguous without --service.
	err = e.SetProfile("chaos")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "hello, other") {
		t.Fatalf("ambiguous error = %v", err)
	}
	// Unique per-service name resolves by bare name.
	if err := e.SetProfile("slow-ok"); err != nil {
		t.Fatal(err)
	}
	if e.Profile("hello") != "slow-ok" {
		t.Fatalf("bare-name activation landed on %q", e.Profile("hello"))
	}
	// Unknown name describes the full available set.
	err = e.SetProfile("nope")
	if err == nil || !strings.Contains(err.Error(), "presets: launch-day") || !strings.Contains(err.Error(), "hello: chaos, slow-ok") {
		t.Fatalf("unknown-profile error should list availability, got: %v", err)
	}
	// Per-service activation with a foreign name names the real options.
	err = e.SetServiceProfile("other", "slow-ok")
	if err == nil || !strings.Contains(err.Error(), `has no profile "slow-ok"`) || !strings.Contains(err.Error(), "has: chaos") {
		t.Fatalf("wrong-profile error = %v", err)
	}
	// Unknown service lists the manifest's services.
	err = e.SetServiceProfile("ghost", "chaos")
	if err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("unknown-service error = %v", err)
	}
	// Deactivating a typo'd service errors too, not a silent success.
	err = e.SetServiceProfile("ghost", "")
	if err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("deactivate unknown service = %v, want error", err)
	}
}

// The catalog exposes presets and per-service profiles with their source.
func TestProfileCatalog(t *testing.T) {
	e, err := New(profileManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	cat := e.AvailableProfiles()
	if len(cat.Presets) != 1 || cat.Presets[0].Name != "launch-day" {
		t.Fatalf("presets = %+v", cat.Presets)
	}
	infos := cat.Services["hello"]
	if len(infos) != 2 || infos[0].Name != "chaos" || infos[1].Name != "slow-ok" || infos[0].Source != "manifest" {
		t.Fatalf("hello profiles = %+v", infos)
	}
	snap := e.ProfileSnapshot()
	if snap.ActiveGlobal != "" || snap.Active == nil || len(snap.Active) != 0 {
		t.Fatalf("fresh snapshot = %+v", snap)
	}
}

// A profile name defined on a service that declares no rules for it (the
// adapter-authored shape) must pass through untouched.
func TestProfileActiveWithoutRulesPassesThrough(t *testing.T) {
	// HTTPServerForTest serves the first map-ordered service; pin one.
	// Declare a mode with no rules: rules-less, like adapter-authored ones.
	m := profileManifest(t)
	delete(m.Services, "other")
	m.Services["hello"].Profiles["breach"] = manifest.ServiceProfile{}
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	addr, shutdown := startOnFreePort(t, e)
	defer shutdown()

	if err := e.SetServiceProfile("hello", "breach"); err != nil {
		t.Fatal(err)
	}
	body, status := get(t, addr+"/ok")
	if status != 200 || !strings.Contains(body, "hi") {
		t.Fatalf("rules-less profile changed dispatch: %d %q", status, body)
	}
}

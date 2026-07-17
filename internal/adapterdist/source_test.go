package adapterdist

import (
	"testing"
)

func TestParseSourceEmpty(t *testing.T) {
	for _, spec := range []string{"", "  ", "\t\n"} {
		_, err := ParseSource(spec)
		if err == nil {
			t.Errorf("ParseSource(%q) should error on empty spec", spec)
		}
	}
}

func TestParseGitShorthandHTTPS(t *testing.T) {
	cases := []struct {
		spec     string
		url      string
		ref      string
		host     string
		path     string
		wantStr  string
		identity string
	}{
		{
			spec:     "git:github.com/user/repo",
			url:      "github.com/user/repo",
			ref:      "",
			host:     "github.com",
			path:     "user/repo",
			wantStr:  "git:github.com/user/repo",
			identity: "github.com/user/repo",
		},
		{
			spec:     "git:github.com/user/repo@v3.1.0",
			url:      "github.com/user/repo",
			ref:      "v3.1.0",
			host:     "github.com",
			path:     "user/repo",
			wantStr:  "git:github.com/user/repo@v3.1.0",
			identity: "github.com/user/repo",
		},
		{
			spec:     "git:github.com/org/sub/deep-repo@v2.0-rc.1",
			url:      "github.com/org/sub/deep-repo",
			ref:      "v2.0-rc.1",
			host:     "github.com",
			path:     "org/sub/deep-repo",
			wantStr:  "git:github.com/org/sub/deep-repo@v2.0-rc.1",
			identity: "github.com/org/sub/deep-repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s, err := ParseSource(tc.spec)
			if err != nil {
				t.Fatalf("ParseSource: %v", err)
			}
			if s.Kind != "git" {
				t.Errorf("Kind = %q, want git", s.Kind)
			}
			if s.URL != tc.url {
				t.Errorf("URL = %q, want %q", s.URL, tc.url)
			}
			if s.Ref != tc.ref {
				t.Errorf("Ref = %q, want %q", s.Ref, tc.ref)
			}
			if s.Host != tc.host {
				t.Errorf("Host = %q, want %q", s.Host, tc.host)
			}
			if s.Path != tc.path {
				t.Errorf("Path = %q, want %q", s.Path, tc.path)
			}
			if got := s.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
			if got := s.Identity(); got != tc.identity {
				t.Errorf("Identity() = %q, want %q", got, tc.identity)
			}
		})
	}
}

func TestParseGitShorthandSSH(t *testing.T) {
	cases := []struct {
		spec    string
		url     string
		ref     string
		host    string
		path    string
		wantStr string
	}{
		{
			spec:    "git:git@github.com:user/repo",
			url:     "git@github.com:user/repo",
			ref:     "",
			host:    "github.com",
			path:    "user/repo",
			wantStr: "git:git@github.com:user/repo",
		},
		{
			spec:    "git:git@github.com:user/repo@v1.0",
			url:     "git@github.com:user/repo",
			ref:     "v1.0",
			host:    "github.com",
			path:    "user/repo",
			wantStr: "git:git@github.com:user/repo@v1.0",
		},
		{
			spec:    "git:deploy@gitlab.io:team/sub/repo@main",
			url:     "deploy@gitlab.io:team/sub/repo",
			ref:     "main",
			host:    "gitlab.io",
			path:    "team/sub/repo",
			wantStr: "git:deploy@gitlab.io:team/sub/repo@main",
		},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s, err := ParseSource(tc.spec)
			if err != nil {
				t.Fatalf("ParseSource: %v", err)
			}
			if s.Kind != "git" {
				t.Errorf("Kind = %q, want git", s.Kind)
			}
			if s.URL != tc.url {
				t.Errorf("URL = %q, want %q", s.URL, tc.url)
			}
			if s.Ref != tc.ref {
				t.Errorf("Ref = %q, want %q", s.Ref, tc.ref)
			}
			if s.Host != tc.host {
				t.Errorf("Host = %q, want %q", s.Host, tc.host)
			}
			if s.Path != tc.path {
				t.Errorf("Path = %q, want %q", s.Path, tc.path)
			}
			if got := s.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
		})
	}
}

func TestParseProtocolURLs(t *testing.T) {
	cases := []struct {
		spec string
		url  string
		ref  string
		host string
		path string
	}{
		{
			spec: "https://github.com/user/repo",
			url:  "https://github.com/user/repo",
			ref:  "",
			host: "github.com",
			path: "user/repo",
		},
		{
			spec: "https://github.com/user/repo@v1.0",
			url:  "https://github.com/user/repo",
			ref:  "v1.0",
			host: "github.com",
			path: "user/repo",
		},
		{
			spec: "ssh://git@github.com/user/repo@v1.0",
			url:  "ssh://git@github.com/user/repo",
			ref:  "v1.0",
			host: "github.com",
			path: "user/repo",
		},
		{
			spec: "ssh://github.com/user/repo@main",
			url:  "ssh://github.com/user/repo",
			ref:  "main",
			host: "github.com",
			path: "user/repo",
		},
		{
			spec: "git://github.com/user/repo@v2.0",
			url:  "git://github.com/user/repo",
			ref:  "v2.0",
			host: "github.com",
			path: "user/repo",
		},
		{
			spec: "https://codeberg.org/org/repo@release/2.0",
			url:  "https://codeberg.org/org/repo",
			ref:  "release/2.0",
			host: "codeberg.org",
			path: "org/repo",
		},
		{
			spec: "file:///tmp/repo@v1.0",
			url:  "file:///tmp/repo",
			ref:  "v1.0",
			host: "localhost",
			path: "tmp/repo",
		},
		{
			spec: "file://localhost/tmp/repo",
			url:  "file://localhost/tmp/repo",
			ref:  "",
			host: "localhost",
			path: "tmp/repo",
		},
		{
			spec: "git:file:///tmp/repo@v1.0",
			url:  "file:///tmp/repo",
			ref:  "v1.0",
			host: "localhost",
			path: "tmp/repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s, err := ParseSource(tc.spec)
			if err != nil {
				t.Fatalf("ParseSource: %v", err)
			}
			if s.Kind != "git" {
				t.Errorf("Kind = %q, want git", s.Kind)
			}
			if s.URL != tc.url {
				t.Errorf("URL = %q, want %q", s.URL, tc.url)
			}
			if s.Ref != tc.ref {
				t.Errorf("Ref = %q, want %q", s.Ref, tc.ref)
			}
			if s.Host != tc.host {
				t.Errorf("Host = %q, want %q", s.Host, tc.host)
			}
			if s.Path != tc.path {
				t.Errorf("Path = %q, want %q", s.Path, tc.path)
			}
		})
	}
}

func TestParseLocalPaths(t *testing.T) {
	cases := []string{
		"/abs/path/to/adapter",
		"./rel/path",
		"../sibling/adapter",
		".",
		"..",
		"/",
		"./my-adapter",
		"some/dir/without/prefix",
	}
	for _, spec := range cases {
		t.Run(spec, func(t *testing.T) {
			s, err := ParseSource(spec)
			if err != nil {
				t.Fatalf("ParseSource(%q): %v", spec, err)
			}
			if s.Kind != "local" {
				t.Errorf("Kind = %q, want local", s.Kind)
			}
			if s.URL != spec {
				t.Errorf("URL = %q, want %q", s.URL, spec)
			}
			if s.Ref != "" {
				t.Errorf("Ref = %q, want empty", s.Ref)
			}
			if got := s.String(); got != spec {
				t.Errorf("String() = %q, want %q", got, spec)
			}
			if got := s.Identity(); got != spec {
				t.Errorf("Identity() = %q, want %q", got, spec)
			}
		})
	}
}

// --- dedup ---

func TestIdentityDedup(t *testing.T) {
	// Same repo, different refs → same identity.
	s1, err := ParseSource("git:github.com/user/repo@v1.0")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := ParseSource("git:github.com/user/repo@v2.0")
	if err != nil {
		t.Fatal(err)
	}
	s3, err := ParseSource("git:github.com/user/repo")
	if err != nil {
		t.Fatal(err)
	}
	if s1.Identity() != s2.Identity() {
		t.Errorf("identity mismatch: %q vs %q", s1.Identity(), s2.Identity())
	}
	if s1.Identity() != s3.Identity() {
		t.Errorf("identity mismatch: %q vs %q", s1.Identity(), s3.Identity())
	}
	// Different repos → different identity.
	other, _ := ParseSource("git:github.com/other/repo@v1.0")
	if s1.Identity() == other.Identity() {
		t.Errorf("different repos should have different identities")
	}
}

// --- validation: reject unsafe hosts and paths ---

func TestParseRejectsUnsafeHostPath(t *testing.T) {
	bad := []string{
		"git:../etc/passwd",            // path traversal
		"git:github.com/../etc/passwd", // path traversal in path
		"git:github.com/../../etc",     // path traversal
		"git:host/repo/../../escape",   // path traversal
		"git:..%2f..%2f/etc",           // encoded traversal (not in charset)
		"git:/abs/path",                // leading slash
		"git:-option/repo",             // leading dash in path
		"git:host;a=b/repo",            // semicolon in host
		"git:host/repo;rm",             // semicolon in path
		"git:host/repo|pipe",           // pipe in path
		"git:host/repo$var",            // dollar in path
		"git:host/repo`cmd`",           // backtick in path
		"git:(host)/repo",              // parens
		"git: host/repo",               // space in host
		"git:git@host:user/repo;bad",   // SSH with bad char
	}
	for _, spec := range bad {
		t.Run(spec, func(t *testing.T) {
			_, err := ParseSource(spec)
			if err == nil {
				t.Errorf("ParseSource(%q) should be rejected", spec)
			}
		})
	}
}

// --- validation: reject injection refs ---

func TestParseRejectsInjectionRefs(t *testing.T) {
	bad := []string{
		`git:host/repo@; rm -rf /`,
		`git:host/repo@--upload-pack=evil`,
		`git:host/repo@-branch`,
		`git:host/repo@../etc/passwd`,
		`git:host/repo@$(whoami)`,
		`git:host/repo@` + "`whoami`",
		`git:host/repo@| cat /etc/passwd`,
		`git:host/repo@; echo hacked`,
		`git:host/repo@ & echo owned`,
		`git:host/repo@\n`, // newline (not in charset)
	}
	for _, spec := range bad {
		t.Run(spec, func(t *testing.T) {
			_, err := ParseSource(spec)
			if err == nil {
				t.Errorf("ParseSource(%q) should reject injection ref", spec)
			}
		})
	}
}

func TestValidRefsAccepted(t *testing.T) {
	good := []string{
		"git:host/user/repo@v1.0",
		"git:host/user/repo@v1.0-rc.1",
		"git:host/user/repo@main",
		"git:host/user/repo@release/2.0",
		"git:host/user/repo@abc123def",
		"git:host/user/repo@HEAD",
		"git:host/user/repo@feature/branch-1",
		"git:host/user/repo@1.2.3",
	}
	for _, spec := range good {
		t.Run(spec, func(t *testing.T) {
			s, err := ParseSource(spec)
			if err != nil {
				t.Fatalf("ParseSource(%q): %v", spec, err)
			}
			if s.Ref == "" {
				t.Errorf("ref should not be empty for %q", spec)
			}
		})
	}
}

// --- git shorthand error cases ---

func TestParseGitShorthandErrors(t *testing.T) {
	bad := []string{
		"git:",
		"git:host", // no path
	}
	for _, spec := range bad {
		t.Run(spec, func(t *testing.T) {
			_, err := ParseSource(spec)
			if err == nil {
				t.Errorf("ParseSource(%q) should error", spec)
			}
		})
	}
}

// --- String round-trip ---

func TestStringRoundTrip(t *testing.T) {
	specs := []string{
		"git:github.com/user/repo@v1.0",
		"git:github.com/user/repo",
		"git:git@github.com:user/repo@v1.0",
		"git:git@github.com:user/repo",
		"https://github.com/user/repo@v1.0",
		"https://github.com/user/repo",
		"ssh://git@github.com/user/repo@v1.0",
		"file:///tmp/repo@v1.0",
		"git:file:///tmp/repo@v1.0",
		"/local/abs/path",
		"./relative/path",
	}
	for _, spec := range specs {
		t.Run(spec, func(t *testing.T) {
			s, err := ParseSource(spec)
			if err != nil {
				t.Fatalf("ParseSource(%q): %v", spec, err)
			}
			// Re-parse the canonical form — should succeed.
			s2, err := ParseSource(s.String())
			if err != nil {
				t.Fatalf("re-parse of String() %q: %v", s.String(), err)
			}
			// Identities must match.
			if s.Identity() != s2.Identity() {
				t.Errorf("identity mismatch after round-trip: %q vs %q",
					s.Identity(), s2.Identity())
			}
			// Refs must match.
			if s.Ref != s2.Ref {
				t.Errorf("ref mismatch after round-trip: %q vs %q", s.Ref, s2.Ref)
			}
		})
	}
}

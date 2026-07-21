// Package adapterdist implements resolution and caching of adapter source
// specifications for stunt. A source spec describes where to fetch an
// adapter from: either a git repository (by URL and optional pinned ref) or
// a local filesystem path.
//
// Source specs mirror the pi package model (§6.9 of the design spec):
//
//   - git:github.com/user/repo             shorthand, head-at-install
//   - git:github.com/user/repo@v3.1.0      shorthand, pinned ref
//   - git:git@github.com:user/repo@v1      SSH shorthand
//   - https://github.com/user/repo@v1      protocol URL
//   - ssh://git@github.com/user/repo@v1    SSH protocol URL
//   - git://github.com/user/repo@v1        git protocol URL
//   - /abs/path  ./rel/path  ../sibling    local filesystem path
//
// Git sources are cloned into a cache directory rooted at
// <root>/git/<host>/<path> and pinned to the specified ref (or the default
// branch HEAD at install time). See [Cache] for details.
//
// # Security model
//
// All git operations use [os/exec.Command] with explicit argv — never a
// shell — so shell injection is impossible by construction. Refs, hosts,
// and paths are additionally validated against conservative character sets
// (defense-in-depth) to prevent:
//
//   - Option injection via a leading "-" in a ref.
//   - Directory traversal via ".." in a host/path/ref.
//   - Unexpected characters that could confuse git's ref resolution.
package adapterdist

import (
	"fmt"
	"regexp"
	"strings"
)

// Source describes where to fetch an adapter from. It is the parsed form of
// a source spec string (see [ParseSource]).
type Source struct {
	// Kind is "git" for git sources or "local" for filesystem paths.
	Kind string

	// URL is the clone URL for git sources (without ref) or the filesystem
	// path for local sources. For git sources this doubles as the dedup
	// identity (see [Source.Identity]).
	URL string

	// Ref is the pinned git ref (tag, branch, or SHA) for git sources.
	// An empty Ref means "head-at-install": the default branch HEAD at the
	// time of the first [Cache.Ensure] call; the resolved commit SHA is
	// returned as resolvedRef.
	Ref string

	// Host is the hostname portion of a git URL, used for the cache layout
	// (<root>/git/<host>/<path>). Empty for local sources.
	Host string

	// Path is the repository path portion of a git URL (e.g. "user/repo"),
	// used for the cache layout. Empty for local sources.
	Path string
}

// gitProtocols are the URL scheme prefixes recognized as git sources.
// file:// is included so that local git repositories (e.g. for offline
// testing or LAN-hosted repos) can be used as adapter sources.
var gitProtocols = []string{"https://", "ssh://", "git://", "file://"}

// Character-class validation regexes (security: prevent cache-dir escape
// and git-argument injection — see the package security model comment).
var (
	safeHost = regexp.MustCompile(`^[A-Za-z0-9.\-]+$`)
	safePath = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)
	safeRef  = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)
)

// ParseSource parses a source spec into a [Source]. Recognized forms:
//
//   - git:host/user/repo[@ref]      HTTPS shorthand
//   - git:user@host:path[@ref]      SSH shorthand (scp-like)
//   - https://host/path[@ref]       protocol URL
//   - ssh://[user@]host/path[@ref]  protocol URL
//   - git://host/path[@ref]         protocol URL
//   - /abs/path, ./rel, ../sibling  local filesystem path
//
// For git sources, Host and Path are extracted for the cache layout and
// validated against a safe character set to prevent cache-directory escape.
// Refs are validated against a safe charset to prevent git-argument
// injection (see the package security model).
func ParseSource(spec string) (*Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("adapterdist: empty source spec")
	}

	// Protocol URLs (https://, ssh://, git://) — checked before the bare
	// "git:" shorthand so that "git://" is not misclassified.
	for _, proto := range gitProtocols {
		if strings.HasPrefix(spec, proto) {
			return parseProtocolURL(spec, proto)
		}
	}

	// git: shorthand forms (HTTPS or SSH scp-like).
	if strings.HasPrefix(spec, "git:") {
		return parseGitShorthand(spec[len("git:"):])
	}

	// embedded:<name> — an adapter baked into the binary (no fetch needed).
	if strings.HasPrefix(spec, "embedded:") {
		name := spec[len("embedded:"):]
		if name == "" {
			return nil, fmt.Errorf("adapterdist: empty embedded adapter name")
		}
		// Validate the name against the safe path charset to prevent
		// cache-directory escape. An embedded name must be a single segment
		// (no slashes, no "..").
		if strings.Contains(name, "/") || strings.Contains(name, "..") || !safePath.MatchString(name) {
			return nil, fmt.Errorf("adapterdist: unsafe embedded adapter name %q", name)
		}
		return &Source{Kind: "embedded", URL: name}, nil
	}

	// Anything else is a local filesystem path.
	return &Source{Kind: "local", URL: spec}, nil
}

// parseGitShorthand handles the git: prefix forms:
//
//	host/user/repo[@ref]   HTTPS shorthand
//	user@host:path[@ref]   SSH shorthand (scp-like)
func parseGitShorthand(rest string) (*Source, error) {
	if rest == "" {
		return nil, fmt.Errorf("adapterdist: empty git source after 'git:' prefix")
	}

	// If the rest starts with a known protocol (e.g. "git:file:///tmp/repo"),
	// delegate to the protocol URL parser so the URL is preserved correctly.
	for _, proto := range gitProtocols {
		if strings.HasPrefix(rest, proto) {
			return parseProtocolURL(rest, proto)
		}
	}

	// SSH shorthand: user@host:path[@ref].
	// Detect by a ':' that has an '@' before it (scp-like syntax).
	if colonIdx := strings.Index(rest, ":"); colonIdx > 0 {
		beforeColon := rest[:colonIdx]
		if atIdx := strings.Index(beforeColon, "@"); atIdx >= 0 {
			userAtHost := beforeColon    // e.g. "git@github.com"
			host := userAtHost[atIdx+1:] // e.g. "github.com"
			pathRef := rest[colonIdx+1:] // e.g. "user/repo" or "user/repo@v1"
			path, ref := splitRef(pathRef)
			url := userAtHost + ":" + path
			return newGitSource(url, host, path, ref)
		}
	}

	// HTTPS shorthand: host/path[@ref].
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return nil, fmt.Errorf("adapterdist: invalid git shorthand %q: expected host/path", rest)
	}
	host := rest[:slashIdx]
	pathRef := rest[slashIdx+1:]
	path, ref := splitRef(pathRef)
	url := host + "/" + path
	return newGitSource(url, host, path, ref)
}

// parseProtocolURL handles https://, ssh://, git:// URLs.
// The ref (if any) is the portion after the last '@' in the path component.
func parseProtocolURL(spec, scheme string) (*Source, error) {
	afterScheme := spec[len(scheme):] // everything after "https://" etc.

	// The first '/' separates the authority (host[:port][userinfo]) from the path.
	slashIdx := strings.Index(afterScheme, "/")
	if slashIdx < 0 {
		// No path component — just a host.
		return newGitSource(spec, stripUserinfo(afterScheme), "", "")
	}

	authority := afterScheme[:slashIdx]
	pathRef := afterScheme[slashIdx+1:]
	path, ref := splitRef(pathRef)
	host := stripUserinfo(authority)
	url := scheme + authority + "/" + path

	// For file:// URLs with an empty authority (e.g. "file:///tmp/repo"),
	// default host to "localhost" — this is the standard semantics for
	// file URLs and keeps the cache layout valid (<root>/git/localhost/...).
	if host == "" && scheme == "file://" {
		host = "localhost"
	}

	return newGitSource(url, host, path, ref)
}

// splitRef splits "path@ref" into ("path", "ref") at the last '@'.
// If there is no '@', returns (s, "").
func splitRef(s string) (path, ref string) {
	idx := strings.LastIndex(s, "@")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// stripUserinfo removes the "user@" prefix from an authority component.
// e.g. "git@github.com" -> "github.com".
func stripUserinfo(authority string) string {
	if atIdx := strings.LastIndex(authority, "@"); atIdx >= 0 {
		return authority[atIdx+1:]
	}
	return authority
}

// newGitSource constructs a git [Source] and validates host/path/ref.
// It rejects URLs starting with "-" to prevent git option injection (C1):
// the SSH shorthand "git:user@host:path" can yield a URL like
// "--upload-pack=evil@host:path" which git would parse as a flag, not a
// URL. This is a defense-in-depth measure — cloneFresh also inserts "--"
// before the URL in the clone argv.
func newGitSource(url, host, path, ref string) (*Source, error) {
	if strings.HasPrefix(url, "-") {
		return nil, fmt.Errorf("adapterdist: unsafe URL %q (must not start with -)", url)
	}
	if err := validateHostPath(host, path); err != nil {
		return nil, err
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	return &Source{
		Kind: "git",
		URL:  url,
		Ref:  ref,
		Host: host,
		Path: path,
	}, nil
}

// validateHostPath checks that host and path are safe for use in the cache
// directory layout (<root>/git/<host>/<path>). This prevents directory
// traversal attacks (e.g. ".." or absolute paths) that could escape the
// cache root.
func validateHostPath(host, path string) error {
	if host == "" {
		return fmt.Errorf("adapterdist: git source missing host")
	}
	if !safeHost.MatchString(host) || strings.Contains(host, "..") || strings.HasPrefix(host, "-") {
		return fmt.Errorf("adapterdist: unsafe host %q", host)
	}
	if path == "" {
		return fmt.Errorf("adapterdist: git source missing path")
	}
	if !safePath.MatchString(path) || strings.Contains(path, "..") {
		return fmt.Errorf("adapterdist: unsafe path %q", path)
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "-") {
		return fmt.Errorf("adapterdist: unsafe path %q", path)
	}
	return nil
}

// validateRef checks that a git ref is safe to pass as a command-line
// argument to git. The ref must match a conservative charset and must not
// contain ".." or start with "-" (which could be interpreted as a git
// option). An empty ref is valid (head-at-install).
//
// This is a defense-in-depth measure — refs are always passed via explicit
// argv, never through a shell — that prevents option injection and path
// traversal.
func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !safeRef.MatchString(ref) {
		return fmt.Errorf("adapterdist: unsafe ref %q (allowed: [A-Za-z0-9._/-])", ref)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("adapterdist: unsafe ref %q (must not contain ..)", ref)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("adapterdist: unsafe ref %q (must not start with -)", ref)
	}
	return nil
}

// String reproduces the canonical source spec string.
//
// For git shorthand sources: "git:<url>@<ref>" (or "git:<url>" if ref
// is empty).
// For git protocol URLs (https://, ssh://, git://): the URL with ref
// appended (the scheme already identifies it as git).
// For local sources: the filesystem path.
//
// The output is always re-parseable by [ParseSource].
func (s *Source) String() string {
	if s.Kind == "embedded" {
		return "embedded:" + s.URL
	}
	if s.Kind == "local" {
		return s.URL
	}
	// Protocol URLs already have a scheme, so no "git:" prefix is needed.
	for _, proto := range gitProtocols {
		if strings.HasPrefix(s.URL, proto) {
			if s.Ref != "" {
				return s.URL + "@" + s.Ref
			}
			return s.URL
		}
	}
	// Bare shorthand URL — prepend "git:".
	if s.Ref != "" {
		return "git:" + s.URL + "@" + s.Ref
	}
	return "git:" + s.URL
}

// Identity returns the dedup identity of this source: the URL without the
// ref. Two sources pointing to the same repository (same URL) but at
// different refs share the same identity.
func (s *Source) Identity() string {
	return s.URL
}

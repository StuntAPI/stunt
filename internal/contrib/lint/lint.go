// Package lint checks adapter fixtures and templates for content that looks
// like REAL recorded data rather than synthetic data.
//
// The safety rule: adapters must ship SYNTHETIC data only — never recorded
// real responses or PII. Lint scans fixture (.jsonl) and template (.json,
// .yaml) files for patterns indicative of leaked real data: real-looking
// emails, UUIDs, provider-specific IDs (cus_, ch_, …), credit-card numbers,
// long base64 blobs, and PII field names with literal values.
//
// Template placeholders ({{ faker.Email }}, {{ uuid }}) are recognized and
// NOT flagged — they are the correct way to produce synthetic values.
package lint

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Severity levels for findings.
const (
	SeverityError = "error"
	SeverityWarn  = "warn"
)

// maxLiteralLen is the threshold (in runes) above which a fixture string
// value is considered suspiciously long and warrants a warning.
const maxLiteralLen = 200

// Finding represents a single lint issue discovered in an adapter.
type Finding struct {
	File     string // path to the file (relative to the adapter dir)
	Line     int    // 1-based line number
	Severity string // "error" or "warn"
	Message  string // human-readable description
}

// ExitCode returns 1 if any finding has "error" severity, 0 otherwise.
// The CLI uses this to fail the build when real data is detected.
func ExitCode(findings []Finding) int {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return 1
		}
	}
	return 0
}

// Lint scans the adapter directory's fixtures/, templates/, and endpoints/
// directories (recursively) plus the root adapter.yaml for content that looks
// like real recorded data. It also validates the `ws:` section structurally
// (each endpoint must have a route + valid handler spec) and scans the ws
// handler scripts for real-looking data.
//
// It returns all findings (which may be empty). A nil error with findings
// is a successful scan that found issues.
func Lint(dir string) ([]Finding, error) {
	var findings []Finding

	// Scan convention directories recursively.
	for _, sub := range scanDirs {
		subDir := filepath.Join(dir, sub)
		ff, err := scanDirRecursive(dir, subDir)
		if err != nil {
			return nil, err
		}
		findings = append(findings, ff...)
	}

	// Scan the root adapter.yaml.
	manifestPath := filepath.Join(dir, "adapter.yaml")
	if entries, err := os.Stat(manifestPath); err == nil && !entries.IsDir() {
		ff, err := scanFile("adapter.yaml", manifestPath)
		if err != nil {
			return nil, err
		}
		findings = append(findings, ff...)
	}

	// Validate and scan the ws section (handler scripts).
	wsFindings, err := lintWS(dir)
	if err != nil {
		return nil, err
	}
	findings = append(findings, wsFindings...)

	return findings, nil
}

// wsManifest is a minimal struct for parsing just the `ws:` section of
// adapter.yaml.
type wsManifest struct {
	WS []struct {
		Route   string `yaml:"route"`
		Handler string `yaml:"handler"`
	} `yaml:"ws"`
}

// lintWS validates the ws section of adapter.yaml: each ws endpoint must have
// a non-empty route and a valid handler spec ("scripts/x.star#fn" form). It
// also scans each ws handler script for content that looks like real data,
// so a handler hardcoding real-looking values is flagged.
func lintWS(dir string) ([]Finding, error) {
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil // no adapter.yaml → nothing to validate
	}

	var wm wsManifest
	if err := yaml.Unmarshal(data, &wm); err != nil {
		// Malformed YAML is caught by the adapter loader; skip ws lint.
		return nil, nil
	}

	var findings []Finding

	seenRoutes := make(map[string]bool)
	for i, ep := range wm.WS {
		// Validate route.
		if ep.Route == "" {
			findings = append(findings, Finding{
				File:     "adapter.yaml",
				Severity: SeverityError,
				Message:  fmt.Sprintf("ws[%d].route is required", i),
			})
			continue
		}
		if seenRoutes[ep.Route] {
			findings = append(findings, Finding{
				File:     "adapter.yaml",
				Severity: SeverityError,
				Message:  fmt.Sprintf("ws[%d].route %q is duplicated", i, ep.Route),
			})
		}
		seenRoutes[ep.Route] = true

		// Validate handler spec.
		if ep.Handler == "" {
			findings = append(findings, Finding{
				File:     "adapter.yaml",
				Severity: SeverityError,
				Message:  fmt.Sprintf("ws[%d].handler is required", i),
			})
			continue
		}
		if !strings.Contains(ep.Handler, "#") {
			findings = append(findings, Finding{
				File:     "adapter.yaml",
				Severity: SeverityError,
				Message:  fmt.Sprintf("ws[%d].handler %q must be in \"scripts/x.star#fn\" form", i, ep.Handler),
			})
			continue
		}

		// Scan the handler script for real-looking data.
		scriptPath := ep.Handler[:strings.Index(ep.Handler, "#")]
		absScript := filepath.Join(dir, scriptPath)
		ff, err := scanFile(scriptPath, absScript)
		if err != nil {
			// Missing script is not a lint error (the loader catches it);
			// skip scanning.
			continue
		}
		findings = append(findings, ff...)
	}

	return findings, nil
}

// scanDirRecursive walks dir recursively, scanning all files whose
// extensions match shouldScan. Findings use paths relative to root.
func scanDirRecursive(root, dir string) ([]Finding, error) {
	var findings []Finding
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil {
			if os.IsNotExist(wErr) {
				return nil
			}
			return wErr
		}
		if d.IsDir() {
			return nil
		}
		if !shouldScan(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		ff, err := scanFile(rel, path)
		if err != nil {
			return err
		}
		findings = append(findings, ff...)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("lint: walk %s: %w", dir, err)
	}
	return findings, nil
}

// scanDirs lists the convention directories Lint inspects.
var scanDirs = []string{"fixtures", "templates", "endpoints"}

// shouldScan reports whether a file should be scanned based on its extension.
func shouldScan(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jsonl", ".json", ".yaml", ".yml":
		return true
	}
	return false
}

// scanFile reads a file line-by-line and applies all heuristics.
func scanFile(rel, abs string) ([]Finding, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("lint: open %s: %w", rel, err)
	}
	defer f.Close()

	var findings []Finding
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		findings = append(findings, scanLine(rel, lineNum, scanner.Text())...)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("lint: scan %s: %w", rel, err)
	}
	return findings, nil
}

// scanLine applies all heuristics to a single line of text and returns any
// findings.
func scanLine(file string, lineNum int, line string) []Finding {
	// Strip {{ ... }} placeholders before applying content heuristics.
	// This is how we distinguish our own faker expressions (safe) from
	// literal values (potentially real data).
	literal := rePlaceholder.ReplaceAllString(line, "")

	var findings []Finding

	// Regex-based content heuristics on the literal (placeholder-stripped) text.
	findings = append(findings, matchPattern(file, lineNum, literal, reEmail, "email address looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reUUID, "literal UUID — use {{ uuid }} instead", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reProviderID, "provider-style ID — use a faker placeholder instead", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reCreditCard, "credit-card pattern", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reGitHubToken, "GitHub token — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reStripeKey, "Stripe secret key — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reAWSKey, "AWS access key ID — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reSlackToken, "Slack token — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reGoogleKey, "Google API key — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reJWT, "JWT token — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, rePhone, "phone number — looks like real data", SeverityError)...)
	findings = append(findings, matchPattern(file, lineNum, literal, reBase64, "long base64 blob — looks like real encoded data", SeverityError)...)

	// JSON-aware checks (PII field names, long string values).
	findings = append(findings, checkJSONValues(file, lineNum, line)...)

	return findings
}

// matchPattern applies a regex to text and returns one finding per match.
func matchPattern(file string, lineNum int, text string, re *regexp.Regexp, msg, severity string) []Finding {
	matches := re.FindAllString(text, -1)
	if matches == nil {
		return nil
	}
	var findings []Finding
	for _, m := range matches {
		findings = append(findings, Finding{
			File:     file,
			Line:     lineNum,
			Severity: severity,
			Message:  fmt.Sprintf("%s: %s", msg, truncate(m, 80)),
		})
	}
	return findings
}

// checkJSONValues parses a line as JSON. If successful, it checks each
// string field value for PII field names and suspicious length.
func checkJSONValues(file string, lineNum int, line string) []Finding {
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return nil // not a JSON object on this line — skip
	}

	var findings []Finding
	for k, v := range obj {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		if isPlaceholder(s) {
			continue
		}

		// PII field name with a literal value.
		if piiFieldName(k) {
			findings = append(findings, Finding{
				File:     file,
				Line:     lineNum,
				Severity: SeverityError,
				Message:  fmt.Sprintf("field %q is a sensitive field with a literal value — use a faker placeholder", k),
			})
		}

		// Suspiciously long literal string.
		if len([]rune(s)) > maxLiteralLen {
			findings = append(findings, Finding{
				File:     file,
				Line:     lineNum,
				Severity: SeverityWarn,
				Message:  fmt.Sprintf("field %q has a very long literal value (%d chars) — verify this is synthetic", k, len([]rune(s))),
			})
		}
	}
	return findings
}

// isPlaceholder reports whether a string contains a template expression.
func isPlaceholder(s string) bool {
	return strings.Contains(s, "{{") && strings.Contains(s, "}}")
}

// piiFieldName reports whether a field name (case-insensitive) is a known
// sensitive/PII field that should use a faker placeholder, not a literal.
func piiFieldName(key string) bool {
	lk := strings.ToLower(key)
	for _, name := range piiFields {
		if lk == name {
			return true
		}
	}
	return false
}

// piiFields is the set of sensitive field names. A literal value in any of
// these fields is suspicious.
var piiFields = []string{
	"ssn", "social_security",
	"password", "passwd", "pwd",
	"api_key", "apikey",
	"secret",
	"token", "access_token", "refresh_token",
	"private_key",
	"credit_card", "card_number", "card_cvc", "cvv",
	"email", "phone", "phone_number",
}

// truncate shortens s to at most n runes, appending "…" if truncated.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------------------
// heuristics: regex patterns
// ---------------------------------------------------------------------------

// rePlaceholder matches Go text/template expressions {{ ... }}.
var rePlaceholder = regexp.MustCompile(`\{\{.*?\}\}`)

// reEmail matches common email addresses.
var reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// reUUID matches canonical UUIDs (8-4-4-4-12 hex).
var reUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// reProviderID matches provider-specific prefixed IDs like cus_, ch_, pi_,
// sub_, txn_, acct_, tok_, etc. followed by 4+ alphanumeric characters.
// These are characteristic of recorded data from payment/SaaS APIs.
var reProviderID = regexp.MustCompile(
	`\b(?:cus|ch|pi|sub|txn|acct|card|tok|evt|fee|file|ref|req|conn|src|dp|payout|setupi|plan|prod|price|coupon|promo)_[A-Za-z0-9]{4,}`)

// reCreditCard matches 13–19 digit sequences with optional separators
// (spaces or hyphens), characteristic of card numbers.
var reCreditCard = regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`)

// reGitHubToken matches GitHub personal access tokens and other GitHub tokens.
// Prefixes: ghp_ (classic PAT), gho_ (OAuth), ghu_ (user-to-server),
// ghs_ (server-to-server).
var reGitHubToken = regexp.MustCompile(`\bgh[posu]_[A-Za-z0-9]{16,}\b`)

// reStripeKey matches Stripe secret keys and restricted keys.
var reStripeKey = regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{16,}\b|\brk_(?:live|test)_[A-Za-z0-9]{16,}\b`)

// reAWSKey matches AWS access key IDs (20 uppercase alphanumeric chars
// starting with AKIA).
var reAWSKey = regexp.MustCompile(`\bAKIA[A-Z0-9]{12,}\b`)

// reSlackToken matches Slack tokens (xox[bpoa]-...).
var reSlackToken = regexp.MustCompile(`\bxox[bpoa]-[A-Za-z0-9-]{10,}\b`)

// reGoogleKey matches Google API keys (AIza...).
var reGoogleKey = regexp.MustCompile(`\bAIza[A-Za-z0-9_\-]{20,}\b`)

// reJWT matches JWT tokens (header.payload.signature base64 segments).
var reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)

// rePhone matches phone numbers: sequences of digits with optional + prefix
// and separators (spaces, hyphens, dots, parentheses), 7–15 digits total.
var rePhone = regexp.MustCompile(`\+?\(?\d{1,4}\)?[ .\-]?\(?\d{1,4}\)?[ .\-]?\d{3,4}[ .\-]?\d{0,4}`)

// reBase64 matches long base64-encoded blobs (40+ chars), characteristic of
// real encoded payloads (JWT bodies, certificates, binary blobs).
var reBase64 = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)

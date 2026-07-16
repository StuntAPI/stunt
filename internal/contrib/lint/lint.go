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

// Lint scans the adapter directory's fixtures/ and templates/ for content
// that looks like real recorded data. It returns all findings (which may be
// empty). A nil error with findings is a successful scan that found issues.
func Lint(dir string) ([]Finding, error) {
	var findings []Finding
	for _, sub := range scanDirs {
		subDir := filepath.Join(dir, sub)
		entries, err := os.ReadDir(subDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("lint: read %s: %w", subDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if !shouldScan(entry.Name()) {
				continue
			}
			rel := filepath.Join(sub, entry.Name())
			ff, err := scanFile(rel, filepath.Join(subDir, entry.Name()))
			if err != nil {
				return nil, err
			}
			findings = append(findings, ff...)
		}
	}
	return findings, nil
}

// scanDirs lists the convention directories Lint inspects.
var scanDirs = []string{"fixtures", "templates"}

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

// reBase64 matches long base64-encoded blobs (60+ chars), characteristic of
// real encoded payloads (JWT bodies, certificates, binary blobs).
var reBase64 = regexp.MustCompile(`[A-Za-z0-9+/]{60,}={0,2}`)

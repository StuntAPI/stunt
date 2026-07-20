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
	Value    string // the matched value that triggered the finding (for dedup)
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
// like real recorded data. It also validates the `ws:` and `graphql:` sections
// structurally (ws: each endpoint must have a route + valid handler spec;
// graphql: schema + resolvers must be present with a valid handler spec) and
// scans the ws + graphql handler/resolver scripts for real-looking data.
//
// It also checks the scripts/ directory for helper-drift: the same function
// name defined in multiple handler scripts (excluding lib.star) is flagged as
// a warning, since it indicates copy-pasted code that should be in lib.star.
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

	// Check that adapter.yaml declares the real upstream API + version it
	// simulates (the `api:` block). Adapters should be versioned to match
	// the real API version they reproduce.
	apiFindings := lintAPIBlock(dir)
	findings = append(findings, apiFindings...)

	// Validate and scan the ws section (handler scripts).
	wsFindings, err := lintWS(dir)
	if err != nil {
		return nil, err
	}
	findings = append(findings, wsFindings...)

	// Validate and scan the graphql section (schema + resolver script).
	gqlFindings, err := lintGraphql(dir)
	if err != nil {
		return nil, err
	}
	findings = append(findings, gqlFindings...)

	// Check scripts/ for helper-drift (same function name in multiple files).
	driftFindings, err := lintScriptDrift(dir)
	if err != nil {
		return nil, err
	}
	findings = append(findings, driftFindings...)

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

// graphqlManifest is a minimal struct for parsing just the `graphql:`
// section of adapter.yaml.
type graphqlManifest struct {
	Graphql *struct {
		Schema    string `yaml:"schema"`
		Resolvers string `yaml:"resolvers"`
		Path      string `yaml:"path"`
	} `yaml:"graphql"`
}

// lintGraphql validates the graphql section of adapter.yaml: the schema and
// resolvers fields must be present, and the resolvers spec must be a valid
// handler path (either "scripts/x.star" or "scripts/x.star#fn" form). It
// also scans the resolver script for content that looks like real data,
// so a resolver hardcoding real-looking values is flagged.
func lintGraphql(dir string) ([]Finding, error) {
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil // no adapter.yaml → nothing to validate
	}

	var gm graphqlManifest
	if err := yaml.Unmarshal(data, &gm); err != nil {
		// Malformed YAML is caught by the adapter loader; skip graphql lint.
		return nil, nil
	}
	if gm.Graphql == nil {
		return nil, nil // no graphql section → nothing to validate
	}

	var findings []Finding

	// Validate schema.
	if gm.Graphql.Schema == "" {
		findings = append(findings, Finding{
			File:     "adapter.yaml",
			Severity: SeverityError,
			Message:  "graphql.schema is required when graphql is declared",
		})
	}

	// Validate resolvers presence.
	if gm.Graphql.Resolvers == "" {
		findings = append(findings, Finding{
			File:     "adapter.yaml",
			Severity: SeverityError,
			Message:  "graphql.resolvers is required when graphql is declared",
		})
		return findings, nil // can't scan a missing script
	}

	// Validate resolvers is a valid script path (optionally with #fn).
	// Unlike ws handlers, graphql resolvers may use convention-named
	// functions without a # fragment.
	scriptPath, _ := splitGraphqlHandler(gm.Graphql.Resolvers)
	if scriptPath == "" {
		findings = append(findings, Finding{
			File:     "adapter.yaml",
			Severity: SeverityError,
			Message:  fmt.Sprintf("graphql.resolvers %q must be a script path", gm.Graphql.Resolvers),
		})
		return findings, nil
	}

	// Scan the resolver script for real-looking data.
	absScript := filepath.Join(dir, scriptPath)
	ff, err := scanFile(scriptPath, absScript)
	if err != nil {
		// Missing script is not a lint error (the loader catches it);
		// skip scanning.
		return findings, nil
	}
	findings = append(findings, ff...)

	return findings, nil
}

// splitGraphqlHandler splits "scripts/x.star#fn" into ("scripts/x.star", "fn").
// Unlike ws handlers, the # fragment is optional for graphql resolvers.
func splitGraphqlHandler(h string) (path, fn string) {
	idx := strings.Index(h, "#")
	if idx < 0 {
		return h, ""
	}
	return h[:idx], h[idx+1:]
}

// lintAPIBlock checks that adapter.yaml declares the real upstream API +
// version the adapter simulates via the `api:` block, e.g.:
//
//	api:
//	  name: "Twilio API"
//	  version: "2010-06-01"
//
// Missing or incomplete blocks are warnings (existing adapters without the
// block are not broken), but every adapter SHOULD declare which real API
// version it reproduces for fidelity.
func lintAPIBlock(dir string) []Finding {
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil
	}
	doc := &root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	var apiNode *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "api" {
			apiNode = doc.Content[i+1]
			break
		}
	}
	if apiNode == nil || apiNode.Kind != yaml.MappingNode {
		return []Finding{{
			File: "adapter.yaml", Severity: SeverityWarn,
			Message: "missing `api:` block — declare the real upstream API + version this adapter simulates (e.g. api: { name: \"Twilio API\", version: \"2010-06-01\" })",
		}}
	}
	name, ver := "", ""
	for i := 0; i+1 < len(apiNode.Content); i += 2 {
		switch apiNode.Content[i].Value {
		case "name":
			name = strings.TrimSpace(apiNode.Content[i+1].Value)
		case "version":
			ver = strings.TrimSpace(apiNode.Content[i+1].Value)
		}
	}
	var ff []Finding
	if name == "" || ver == "" {
		ff = append(ff, Finding{
			File: "adapter.yaml", Severity: SeverityWarn,
			Message: "`api:` block incomplete — both api.name and api.version should be set to the real upstream API + version simulated",
		})
	}
	return ff
}

// reDef matches a top-level Starlark function definition, capturing the
// function name. It requires `def ` at the start of a line (no leading
// whitespace) to avoid matching nested defs.
var reDef = regexp.MustCompile(`^def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// lintScriptDrift scans the scripts/ directory (recursively) for .star files
// and flags any function name defined in more than one handler script
// (excluding lib.star). This catches copy-pasted helpers that should be
// moved to scripts/lib.star. The finding is a warning (not an error) because
// the code may be intentionally duplicated.
func lintScriptDrift(dir string) ([]Finding, error) {
	scriptsDir := filepath.Join(dir, "scripts")
	// name -> list of files that define it
	defs := make(map[string][]string)

	err := filepath.WalkDir(scriptsDir, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil {
			if os.IsNotExist(wErr) {
				return nil
			}
			return wErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Ext(path)) != ".star" {
			return nil
		}
		// lib.star is the shared library — its defs are expected to be used
		// across files, so it is excluded from the drift check.
		if filepath.Base(path) == "lib.star" {
			return nil
		}
		names, err := extractDefNames(path)
		if err != nil {
			return nil // skip unreadable files
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		for _, name := range names {
			defs[name] = append(defs[name], rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("lint: drift scan %s: %w", scriptsDir, err)
	}

	var findings []Finding
	for name, files := range defs {
		if len(files) > 1 {
			findings = append(findings, Finding{
				File:     files[0],
				Severity: SeverityWarn,
				Message:  fmt.Sprintf("function %q is defined in %d scripts (%s) — move to lib.star to avoid drift", name, len(files), strings.Join(files, ", ")),
			})
		}
	}
	return findings, nil
}

// extractDefNames reads a .star file and returns the names of all top-level
// function definitions (lines matching `^def <name>(`).
func extractDefNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if m := reDef.FindStringSubmatch(scanner.Text()); m != nil {
			names = append(names, m[1])
		}
	}
	return names, scanner.Err()
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
	// Patterns are ordered most-specific first so that dedup (by file+line+value)
	// keeps the most specific finding (e.g. credit-card wins over phone).
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

	// Deduplicate: when the same value matches multiple heuristics on the
	// same line, keep only the first (most specific) finding. This prevents
	// e.g. a credit-card number from also being reported as a phone number.
	findings = dedupFindings(findings)

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
			Value:    m,
			Message:  fmt.Sprintf("%s: %s", msg, truncate(m, 80)),
		})
	}
	return findings
}

// dedupFindings removes findings that share the same (File, Line, Value)
// triple, keeping the first occurrence. Patterns are applied most-specific
// first, so this ensures a credit-card number is not also reported as a
// phone number.
func dedupFindings(findings []Finding) []Finding {
	seen := make(map[string]bool, len(findings))
	out := findings[:0] // reuse backing array
	for _, f := range findings {
		if f.Value == "" {
			// Findings without a matched value (e.g. PII field-name checks)
			// are always kept.
			out = append(out, f)
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
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

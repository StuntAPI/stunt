package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/contrib"
)

// scaffold creates a clean synthetic adapter in a temp dir and returns its
// path. This is the baseline that should lint clean.
func scaffold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := contrib.Scaffold(dir, "test-api", contrib.ScaffoldOptions{}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	return filepath.Join(dir, "test-api")
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func hasFinding(findings []Finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(strings.ToLower(f.Message), strings.ToLower(substr)) {
			return true
		}
	}
	return false
}

func hasError(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// --- clean scaffold lints clean ---

func TestScaffoldLintsClean(t *testing.T) {
	dir := scaffold(t)
	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		t.Errorf("unexpected finding: %s:%d [%s] %s", f.File, f.Line, f.Severity, f.Message)
	}
}

// --- real email in fixture produces error ---

func TestRealEmailInFixture(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"item-1","email":"john.doe@acme-corp.com"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected an email finding, got: %+v", findings)
	}
	if !hasError(findings) {
		t.Errorf("expected at least one error-severity finding: %+v", findings)
	}
}

// --- provider-style literal id in fixture produces error ---

func TestProviderIDInFixture(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"cus_xyz123ABCdef","amount":5000}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "provider") {
		t.Errorf("expected a provider-id finding, got: %+v", findings)
	}
}

// --- api_key with real-looking value produces error ---

func TestPIIFieldInFixture(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"item-1","api_key":"sk_live_abc123def456ghi789jkl012mno345pqr678"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "api_key") {
		t.Errorf("expected an api_key PII finding, got: %+v", findings)
	}
}

func TestPasswordFieldInFixture(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"item-1","password":"correct-horse-battery-staple"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "password") {
		t.Errorf("expected a password PII finding, got: %+v", findings)
	}
}

// --- placeholders in templates are NOT flagged ---

func TestPlaceholdersNotFlagged(t *testing.T) {
	dir := scaffold(t)
	// Overwrite the template with explicit placeholder usage.
	writeFile(t, dir, "templates/hello.json",
		`{
  "id": "{{ uuid }}",
  "email": "{{ faker.Email }}",
  "token": "{{ faker.ID "tok" }}"
}`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	// The placeholders should not be flagged.
	for _, f := range findings {
		if strings.Contains(strings.ToLower(f.Message), "email") ||
			strings.Contains(strings.ToLower(f.Message), "uuid") ||
			strings.Contains(strings.ToLower(f.Message), "provider") ||
			strings.Contains(strings.ToLower(f.Message), "token") {
			t.Errorf("placeholder was incorrectly flagged: %s:%d %s", f.File, f.Line, f.Message)
		}
	}
}

// --- literal UUID in fixture is flagged ---

func TestLiteralUUIDInFixture(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"550e8400-e29b-41d4-a716-446655440000","name":"test"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "uuid") {
		t.Errorf("expected a UUID finding, got: %+v", findings)
	}
}

// --- credit-card pattern in fixture is flagged ---

func TestCreditCardInFixture(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"item-1","card":"4111-1111-1111-1111"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "credit") {
		t.Errorf("expected a credit-card finding, got: %+v", findings)
	}
}

// --- ExitCode ---

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
	if got := ExitCode([]Finding{{Severity: SeverityWarn}}); got != 0 {
		t.Errorf("ExitCode(warn-only) = %d, want 0", got)
	}
	if got := ExitCode([]Finding{{Severity: SeverityError}}); got != 1 {
		t.Errorf("ExitCode(error) = %d, want 1", got)
	}
	if got := ExitCode([]Finding{
		{Severity: SeverityWarn},
		{Severity: SeverityError},
	}); got != 1 {
		t.Errorf("ExitCode(mixed) = %d, want 1", got)
	}
}

// --- missing dir is not an error, just no findings ---

func TestEmptyDir(t *testing.T) {
	dir := t.TempDir()
	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint on empty dir: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %d: %+v", len(findings), findings)
	}
}

// --- C2: lint scans endpoints/ and adapter.yaml ---

func TestRealDataInEndpointFile(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "endpoints/leaky.yaml",
		`route: /users
class: GET
rules:
  - name: ok
    respond:
      body:
        inline:
          email: admin@real-company.com
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected an email finding from endpoints/ file, got: %+v", findings)
	}
	if !hasError(findings) {
		t.Errorf("expected error-severity finding: %+v", findings)
	}
}

func TestRealDataInAdapterYAML(t *testing.T) {
	dir := scaffold(t)
	// Overwrite adapter.yaml with one containing a real email.
	writeFile(t, dir, "adapter.yaml",
		`id: leaky
name: Leaky
version: "0.1.0"
endpoints:
  - route: /whoami
    method: GET
    rules:
      - name: whoami-ok
        match: { method: GET, path: /whoami }
        respond:
          status: 200
          body:
            inline:
              email: root@realdomain.com
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected an email finding from adapter.yaml, got: %+v", findings)
	}
}

// --- I2: lint recurses into subdirectories ---

func TestLintRecursesIntoSubdirs(t *testing.T) {
	dir := scaffold(t)
	// A fixture nested in a subdirectory.
	writeFile(t, dir, "fixtures/v1/users.jsonl",
		`{"id":"item-1","email":"deep@nested.com"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected an email finding from nested fixture, got: %+v", findings)
	}
}

// --- I3: common token patterns detected ---

func TestGitHubTokenDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"token":"ghp_1234567890abcdefABCD1234567890abcd"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for GitHub token, got: %+v", findings)
	}
}

func TestStripeKeyDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"secret":"sk_live_1234567890abcdefABCDEF1234567890"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for Stripe key, got: %+v", findings)
	}
}

func TestAWSKeyDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"key":"AKIA1234567890ABCD"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for AWS key, got: %+v", findings)
	}
}

func TestSlackTokenDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"token":"xoxb-1234567890-abcdefghij"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for Slack token, got: %+v", findings)
	}
}

func TestGoogleKeyDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"key":"AIzaSyABCDEFGHIJKLMN0123456789abcd"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for Google key, got: %+v", findings)
	}
}

func TestJWTDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for JWT, got: %+v", findings)
	}
}

func TestPhoneNumberDetected(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"phone":"+1-555-123-4567"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasError(findings) {
		t.Errorf("expected an error finding for phone number, got: %+v", findings)
	}
}

func TestEmailPIIFieldDetected(t *testing.T) {
	dir := scaffold(t)
	// A JSON field named "email" with a literal value (not caught by email
	// regex because the value is not an email address, but caught by PII
	// field check).
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"email":"some-literal-value"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected a PII email field finding, got: %+v", findings)
	}
}

func TestPlaceholdersStillNotFlagged(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "fixtures/safe.jsonl",
		`{"token":"{{ faker.ID "tok" }}","email":"{{ faker.Email }}","id":"{{ uuid }}"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityError {
			t.Errorf("placeholder should not be flagged as error: %s:%d %s", f.File, f.Line, f.Message)
		}
	}
}

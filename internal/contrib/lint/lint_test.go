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

// --- api: block versioning check ---

func TestAPIBlockMissing(t *testing.T) {
	dir := scaffold(t)
	// Strip the api: block the scaffold now emits so we can test the missing case.
	writeFile(t, dir, "adapter.yaml", `id: test-api
name: "Test API"
version: "0.1.0"
endpoints:
  - route: /hello
    method: GET
`)
	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "missing `api:` block") {
		t.Errorf("expected a warning about missing api: block; got %v", findings)
	}
	if hasError(findings) {
		t.Errorf("missing api: block should be a warning, not an error")
	}
}

func TestAPIBlockIncomplete(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml", `id: test-api
name: "Test API"
version: "0.1.0"
api:
  name: "Some API"
endpoints:
  - route: /hello
    method: GET
`)
	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "api:") || !hasFinding(findings, "incomplete") {
		t.Errorf("expected an incomplete api: block warning; got %v", findings)
	}
}

func TestAPIBlockPresentNoWarning(t *testing.T) {
	dir := scaffold(t)
	// The scaffolded adapter.yaml now includes a complete api: block, so it
	// should not produce any api-block warning.
	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(strings.ToLower(f.Message), "api:") {
			t.Errorf("did not expect an api: block warning for a complete block: %s", f.Message)
		}
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

func TestCreditCardNotDoubleFlaggedAsPhone(t *testing.T) {
	dir := scaffold(t)
	// A plain credit-card number (no separators) that also matches the
	// phone-number heuristic. After dedup it should be reported only once
	// as a credit-card pattern, not also as a phone number.
	writeFile(t, dir, "fixtures/real.jsonl",
		`{"id":"item-1","card":"4242424242424242"}`+"\n")

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "credit") {
		t.Errorf("expected a credit-card finding, got: %+v", findings)
	}
	for _, f := range findings {
		if strings.Contains(strings.ToLower(f.Message), "phone") {
			t.Errorf("credit-card value should not also be flagged as phone: %s", f.Message)
		}
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

// --- ws section validation ---

// TestWSSectionCleanAdapter verifies that a valid ws section with a clean
// handler script lints without findings.
func TestWSSectionCleanAdapter(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: wsclean
name: WSClean
version: "0.1.0"
ws:
  - route: /ws/echo
    handler: scripts/ws.star#on_connect
`)
	writeFile(t, dir, "scripts/ws.star",
		`def on_connect(ws):
    while True:
        m = ws.recv()
        if m == None:
            break
        ws.send(m)
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityError {
			t.Errorf("unexpected ws error finding: %s:%d %s", f.File, f.Line, f.Message)
		}
	}
}

// TestWSSectionMissingRoute verifies that a ws endpoint without a route is
// flagged.
func TestWSSectionMissingRoute(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: wsbad
name: WSBad
version: "0.1.0"
ws:
  - handler: scripts/ws.star#on_connect
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "ws[0].route") {
		t.Errorf("expected ws route finding, got: %+v", findings)
	}
}

// TestWSSectionMissingHandler verifies that a ws endpoint without a handler
// is flagged.
func TestWSSectionMissingHandler(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: wsbad
name: WSBad
version: "0.1.0"
ws:
  - route: /ws/echo
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "ws[0].handler") {
		t.Errorf("expected ws handler finding, got: %+v", findings)
	}
}

// TestWSSectionInvalidHandlerSpec verifies that a handler spec without the
// "#" separator is flagged.
func TestWSSectionInvalidHandlerSpec(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: wsbad
name: WSBad
version: "0.1.0"
ws:
  - route: /ws/echo
    handler: scripts/ws.star
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "scripts/x.star#fn") {
		t.Errorf("expected handler format finding, got: %+v", findings)
	}
}

// TestWSSectionDuplicateRoute verifies that duplicate ws routes are flagged.
func TestWSSectionDuplicateRoute(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: wsbad
name: WSBad
version: "0.1.0"
ws:
  - route: /ws/echo
    handler: scripts/ws.star#on_connect
  - route: /ws/echo
    handler: scripts/ws.star#on_connect2
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "duplicated") {
		t.Errorf("expected duplicate route finding, got: %+v", findings)
	}
}

// TestWSSectionRealDataInHandlerScript verifies that real-looking data in a
// ws handler script is flagged.
func TestWSSectionRealDataInHandlerScript(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: wsbad
name: WSBad
version: "0.1.0"
ws:
  - route: /ws/echo
    handler: scripts/ws.star#on_connect
`)
	writeFile(t, dir, "scripts/ws.star",
		`def on_connect(ws):
    # Real-looking email hardcoded in handler
    ws.send({"email": "admin@real-company.com"})
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected email finding in ws handler script, got: %+v", findings)
	}
	if !hasError(findings) {
		t.Errorf("expected error-severity finding: %+v", findings)
	}
}

// --- graphql section validation ---

// TestGraphqlSectionCleanAdapter verifies that a valid graphql section with
// a clean resolver script lints without findings.
func TestGraphqlSectionCleanAdapter(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: gqlclean
name: GQLClean
version: "0.1.0"
graphql:
  schema: schemas/blog.graphql
  resolvers: scripts/resolvers.star
`)
	writeFile(t, dir, "schemas/blog.graphql",
		`type Query { user(id: ID!): User }
type User { id: ID! name: String! }
`)
	writeFile(t, dir, "scripts/resolvers.star",
		`def on_user(args):
    return respond(200, {"id": "1", "name": "synthetic"})
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityError {
			t.Errorf("unexpected graphql error finding: %s:%d %s", f.File, f.Line, f.Message)
		}
	}
}

// TestGraphqlSectionMissingSchema verifies that a graphql section without
// a schema is flagged.
func TestGraphqlSectionMissingSchema(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: gqlbad
name: GQLBad
version: "0.1.0"
graphql:
  resolvers: scripts/resolvers.star
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "graphql.schema") {
		t.Errorf("expected graphql.schema finding, got: %+v", findings)
	}
}

// TestGraphqlSectionMissingResolvers verifies that a graphql section without
// resolvers is flagged.
func TestGraphqlSectionMissingResolvers(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: gqlbad
name: GQLBad
version: "0.1.0"
graphql:
  schema: schemas/blog.graphql
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "graphql.resolvers") {
		t.Errorf("expected graphql.resolvers finding, got: %+v", findings)
	}
}

// TestGraphqlSectionRealDataInResolverScript verifies that real-looking data
// in a graphql resolver script is flagged.
func TestGraphqlSectionRealDataInResolverScript(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "adapter.yaml",
		`id: gqlbad
name: GQLBad
version: "0.1.0"
graphql:
  schema: schemas/blog.graphql
  resolvers: scripts/resolvers.star
`)
	writeFile(t, dir, "scripts/resolvers.star",
		`def on_user(args):
    return respond(200, {"email": "admin@real-company.com"})
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "email") {
		t.Errorf("expected email finding in graphql resolver script, got: %+v", findings)
	}
	if !hasError(findings) {
		t.Errorf("expected error-severity finding: %+v", findings)
	}
}

// TestScriptDriftDetectsDuplicateDefs verifies that the same function name
// defined in multiple handler scripts (excluding lib.star) is flagged as a
// drift warning.
func TestScriptDriftDetectsDuplicateDefs(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "scripts/a.star",
		`def _bearer(req):
    return "tok"

def on_get(req):
    return respond(200, {})
`)
	writeFile(t, dir, "scripts/b.star",
		`def _bearer(req):
    return "tok"

def on_post(req):
    return respond(200, {})
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if !hasFinding(findings, "_bearer") {
		t.Errorf("expected drift warning for _bearer, got: %+v", findings)
	}
	// Drift is a warning, not an error — it should not cause a non-zero exit.
	if hasError(findings) {
		t.Errorf("drift should be a warning, not an error: %+v", findings)
	}
}

// TestScriptDriftIgnoresLibStar verifies that functions defined in lib.star
// are NOT flagged as drift (lib.star is the shared library).
func TestScriptDriftIgnoresLibStar(t *testing.T) {
	dir := scaffold(t)
	writeFile(t, dir, "scripts/lib.star",
		`def _bearer(req):
    return "tok"
`)
	writeFile(t, dir, "scripts/a.star",
		`def on_custom_a(req):
    return respond(200, {})
`)
	writeFile(t, dir, "scripts/b.star",
		`def on_custom_b(req):
    return respond(200, {})
`)

	findings, err := Lint(dir)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	// _bearer is only in lib.star — no drift.
	for _, f := range findings {
		if strings.Contains(f.Message, "drift") {
			t.Errorf("unexpected drift finding when helpers are in lib.star: %+v", f)
		}
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGoRecords(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a_test.go", `package conformance

func TestX(t *testing.T) {
	Record(t, "stripe-go/v86", "stripe-style", "Customer round-trip")
	Record(t,  "aws-sdk-go-v2" , "sqs-style" , "whitespace tolerated")
}
`)
	write("b_test.go", `package conformance

func TestY(t *testing.T) {
	Record(t, "twilio-go", "twilio-style", "CreateMessage")
}
`)
	got, err := parseGoRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].SDK != "stripe-go/v86" || got[2].Name != "CreateMessage" {
		t.Fatalf("got %+v", got)
	}

	write("bad_test.go", `Record(t, "sdk", "adapter", fmt.Sprintf("dyn %d", 1))
`)
	if _, err := parseGoRecords(dir); err == nil || !strings.Contains(err.Error(), "literals only") {
		t.Fatalf("non-literal Record not rejected: %v", err)
	}

	dir2 := t.TempDir()
	// gofmt preserves a line-broken argument list; the call-count guard
	// must still see it.
	write2 := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir2, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write2("wrapped_test.go", `Record(
	t, "sdk", "adapter", "wrapped call",
)
`)
	if _, err := parseGoRecords(dir2); err == nil || !strings.Contains(err.Error(), "literals only") {
		t.Fatalf("line-broken Record not rejected: %v", err)
	}
}

func TestParseNodeTests(t *testing.T) {
	dir := t.TempDir()
	body := `import { bootAdapter } from "../helpers";
describe("x", () => {
  test("full lifecycle", async () => {
    const h = await bootAdapter("stripe-style");
    // ===== Typed create/get (form encoded) =====
    // ===== Webhook registration, delivery verified by the SDK's own
    // constructEvent =====
  });
});
`
	if err := os.WriteFile(filepath.Join(dir, "stripe.test.ts"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The other mapped suites must exist too (parseNodeTests reads all of
	// nodeSuiteSDK); give them minimal valid bodies.
	stub := `bootAdapter("twilio-style");
test("lifecycle", async () => {});`
	if err := os.WriteFile(filepath.Join(dir, "twilio.test.ts"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	stub = `bootAdapter("github-style");
test("lifecycle", async () => {});`
	if err := os.WriteFile(filepath.Join(dir, "github.test.ts"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseNodeTests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 { // 2 stripe sections + 2 stub test-name fallbacks
		t.Fatalf("got %+v", got)
	}
	// Files sort github < stripe < twilio: the stripe sections are got[1:3].
	if got[1].SDK != "stripe-node" || got[1].Adapter != "stripe-style" {
		t.Fatalf("got %+v", got[1])
	}
	if got[2].Name != "Webhook registration, delivery verified by the SDK's own constructEvent" {
		t.Fatalf("multiline marker not collapsed: %q", got[2].Name)
	}
}

func TestParseNodeTestsRejectsUnmappedSuiteFile(t *testing.T) {
	dir := t.TempDir()
	for _, base := range []string{"github.test.ts", "stripe.test.ts", "twilio.test.ts"} {
		if err := os.WriteFile(filepath.Join(dir, base),
			[]byte(`bootAdapter("x"); test("t", async () => {});`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "shopify.test.ts"),
		[]byte(`bootAdapter("shopify-style"); test("t", async () => {});`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseNodeTests(dir); err == nil || !strings.Contains(err.Error(), "no SDK label mapping") {
		t.Fatalf("unmapped suite file not rejected: %v", err)
	}
}

func TestParseNodeTestsFallbackRecordsEveryTest(t *testing.T) {
	dir := t.TempDir()
	body := `bootAdapter("twilio-style");
test("first", async () => {});
test("second", async () => {});
`
	for base, boot := range map[string]string{
		"github.test.ts": "github-style",
		"stripe.test.ts": "stripe-style",
		"twilio.test.ts": "twilio-style",
	} {
		if err := os.WriteFile(filepath.Join(dir, base),
			[]byte(strings.Replace(body, "twilio-style", boot, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := parseNodeTests(dir)
	if err != nil {
		t.Fatal(err)
	}
	perSDK := map[string]int{}
	for _, c := range got {
		perSDK[c.SDK]++
	}
	for _, sdk := range []string{"octokit", "stripe-node", "twilio-node"} {
		if perSDK[sdk] != 2 {
			t.Fatalf("sdk %s recorded %d checks, want 2 (second test() must not vanish)", sdk, perSDK[sdk])
		}
	}
}

func TestParseNodeTestsRequiresExactlyOneBoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "github.test.ts"),
		[]byte(`bootAdapter("a"); bootAdapter("b");`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseNodeTests(dir); err == nil || !strings.Contains(err.Error(), "want exactly 1") {
		t.Fatalf("multi-boot not rejected: %v", err)
	}
}

func TestLoadVersions(t *testing.T) {
	conf := t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "go.mod"), []byte(`module m

go 1.25

require (
	github.com/aws/aws-sdk-go-v2 v1.43.7
	github.com/aws/aws-sdk-go-v2/config v1.32.38 // indirect
	github.com/google/go-github/v89 v89.0.0
)

replace x => y
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(conf, "node"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "node", "package.json"),
		[]byte(`{"dependencies": {"stripe": "^22.5.0", "octokit": "^5.0.5"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadVersions(conf)
	if err != nil {
		t.Fatal(err)
	}
	if got["github.com/aws/aws-sdk-go-v2"] != "v1.43.7" {
		t.Fatalf("go.mod pin: %+v", got)
	}
	if _, ok := got["github.com/aws/aws-sdk-go-v2/config"]; ok {
		t.Fatal("indirect require leaked into versions")
	}
	if got["github.com/google/go-github/v89"] != "v89.0.0" {
		t.Fatalf("major-suffixed module: %+v", got)
	}
	if got["stripe"] != "22.5.0" {
		t.Fatalf("package.json floor: %+v", got)
	}
}

func TestResolveVersions(t *testing.T) {
	versions := map[string]string{
		"github.com/google/go-github/v89": "v89.0.0",
		"github.com/stripe/stripe-go/v86": "v86.3.0",
		"stripe":                          "22.5.0",
	}
	checks := []check{
		{SDK: "go-github/v89", Adapter: "github-style", Name: "x"},
		{SDK: "stripe-go/v86", Adapter: "stripe-style", Name: "x"},
		{SDK: "stripe-node", Adapter: "stripe-style", Name: "x"},
	}
	got, err := resolveVersions(checks, versions)
	if err != nil {
		t.Fatal(err)
	}
	if got["go-github/v89"] != "v89.0.0" || got["stripe-go/v86"] != "v86.3.0" {
		t.Fatalf("go versions: %+v", got)
	}
	if got["stripe-node"] != "22.5.0 (floor)" {
		t.Fatalf("node version: %+v", got)
	}

	if _, err := resolveVersions([]check{{SDK: "mystery-sdk"}}, versions); err == nil ||
		!strings.Contains(err.Error(), "no module mapping") {
		t.Fatalf("unmapped sdk not rejected: %v", err)
	}
}

func TestLoadSidecarRejectsGapsAndStaleEntries(t *testing.T) {
	dir := t.TempDir()
	sc := "adapters:\n  stripe-style:\n    deviations:\n      - \"no X\"\n  ghost-style:\n    deviations: []\n"
	if err := os.WriteFile(filepath.Join(dir, "matrix.yaml"), []byte(sc), 0o644); err != nil {
		t.Fatal(err)
	}
	// ghost-style is stale (matches no adapter); missing entry for a-style.
	ids := []string{"a-style", "stripe-style"}
	if _, err := loadSidecar(filepath.Join(dir, "matrix.yaml"), ids); err == nil ||
		!strings.Contains(err.Error(), "ghost-style") {
		t.Fatalf("stale entry not rejected: %v", err)
	}
	sc = "adapters:\n  stripe-style:\n    deviations: []\n"
	if err := os.WriteFile(filepath.Join(dir, "matrix.yaml"), []byte(sc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSidecar(filepath.Join(dir, "matrix.yaml"), ids); err == nil ||
		!strings.Contains(err.Error(), "a-style") {
		t.Fatalf("missing adapter entry not rejected: %v", err)
	}
}

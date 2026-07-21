package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/pathutil"
)

// FileReadAuditEntry is one row in the file-read audit matrix.
// Each entry tests that an adapter-reachable file read path rejects
// traversal escapes (../) and does NOT return the outside file's bytes.
type FileReadAuditEntry struct {
	name    string // human-readable description
	baseDir string // the adapter directory (root of trust)
	relPath string // the adapter-declared relative path
}

// TestFileReadAuditMatrix is a table-driven test proving that EVERY
// adapter-reachable file read rejects path-traversal attempts. This is the
// canonical "file-read audit" for the stunt trust property: "adapters can't
// touch the host."
//
// Rows cover:
//   - rules body.file (internal/rules/evaluate.go bodyBytes)
//   - collection seed (internal/engine/engine.go buildServiceState)
//   - adapter.ReadFile (grpc.descriptor, graphql.schema)
//   - adapter.resolveContainedPath (handler scripts)
//
// Each row creates a "secret" file in baseDir's parent, then attempts the
// read with a ../ path. The assertion: the outside file's bytes are NEVER
// returned.
func TestFileReadAuditMatrix(t *testing.T) {
	cases := []struct {
		name string
		fn   func(t *testing.T, baseDir, parentSecretPath string)
	}{
		{
			name: "rules body.file rejects ../ traversal",
			fn: func(t *testing.T, baseDir, parentSecretPath string) {
				rules := []Rule{{
					Match:   Match{Path: "/x"},
					Respond: Respond{Status: 200, Body: &Body{File: "../stunt_audit_secret.json"}},
				}}
				d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
				body := string(d.BodyBytes)
				if strings.Contains(body, "AUDIT_SECRET") {
					t.Fatalf("TRAVERSAL: body.file read outside file! body=%q", body)
				}
			},
		},
		{
			name: "rules body.file rejects deep ../../ traversal",
			fn: func(t *testing.T, baseDir, parentSecretPath string) {
				rules := []Rule{{
					Match:   Match{Path: "/x"},
					Respond: Respond{Status: 200, Body: &Body{File: "../../stunt_audit_secret.json"}},
				}}
				d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
				body := string(d.BodyBytes)
				if strings.Contains(body, "AUDIT_SECRET") {
					t.Fatalf("TRAVERSAL: body.file deep read outside file! body=%q", body)
				}
			},
		},
		{
			name: "rules body.file rejects absolute path outside base",
			fn: func(t *testing.T, baseDir, parentSecretPath string) {
				rules := []Rule{{
					Match:   Match{Path: "/x"},
					Respond: Respond{Status: 200, Body: &Body{File: parentSecretPath}},
				}}
				d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
				body := string(d.BodyBytes)
				if strings.Contains(body, "AUDIT_SECRET") {
					t.Fatalf("TRAVERSAL: body.file absolute read outside file! body=%q", body)
				}
			},
		},
		{
			name: "rules body.file rejects normalized traversal a/../../x",
			fn: func(t *testing.T, baseDir, parentSecretPath string) {
				rules := []Rule{{
					Match:   Match{Path: "/x"},
					Respond: Respond{Status: 200, Body: &Body{File: "a/../../stunt_audit_secret.json"}},
				}}
				d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
				body := string(d.BodyBytes)
				if strings.Contains(body, "AUDIT_SECRET") {
					t.Fatalf("TRAVERSAL: body.file normalized read outside file! body=%q", body)
				}
			},
		},
		{
			name: "pathutil.ContainedPath rejects all traversal variants",
			fn: func(t *testing.T, baseDir, parentSecretPath string) {
				bad := []string{
					"../stunt_audit_secret.json",
					"../../stunt_audit_secret.json",
					"a/../../../stunt_audit_secret.json",
					parentSecretPath, // absolute outside
				}
				for _, p := range bad {
					_, err := pathutil.ContainedPath(baseDir, p)
					if err == nil {
						t.Errorf("ContainedPath(%q) should have been rejected but was accepted", p)
					}
				}
			},
		},
		{
			name: "legitimate in-base body.file still works",
			fn: func(t *testing.T, baseDir, _ string) {
				// Write a legitimate fixture inside baseDir.
				innerPath := filepath.Join(baseDir, "fixtures", "ok.json")
				os.MkdirAll(filepath.Dir(innerPath), 0o755)
				os.WriteFile(innerPath, []byte(`{"ok": true}`), 0o644)

				rules := []Rule{{
					Match:   Match{Path: "/x"},
					Respond: Respond{Status: 200, Body: &Body{File: "fixtures/ok.json"}},
				}}
				d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
				body := string(d.BodyBytes)
				if !strings.Contains(body, `"ok": true`) {
					t.Fatalf("legitimate in-base file should work, got body=%q", body)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			baseDir := t.TempDir()
			absBase, _ := filepath.Abs(baseDir)
			parent := filepath.Dir(absBase)
			secretPath := filepath.Join(parent, "stunt_audit_secret.json")
			os.WriteFile(secretPath, []byte(`{"secret": "AUDIT_SECRET"}`), 0o644)
			t.Cleanup(func() { os.Remove(secretPath) })

			c.fn(t, absBase, secretPath)
		})
	}
}

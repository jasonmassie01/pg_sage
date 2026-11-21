package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/gitops"
)

// cli-02a: `config validate` and `config diff` take a policy.directory.
// No concurrent test: single-shot commands that write nothing.

func writeGitopsDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const cliPrincipalFile = "kind: Principal\nschema_version: 1\nname: ci-bot\n" +
	"profile: readonly-analyst\nenv_ceiling: dev\n"

func TestConfigCmd_ValidateDirectory(t *testing.T) {
	root := writeGitopsDir(t, map[string]string{"principals/ci-bot.yaml": cliPrincipalFile})
	code, out, errOut := runConfigCLI(t, "", "validate", root)
	if code != configExitOK || !strings.Contains(out, "valid; 0 policy set(s), 1 principal(s)") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	bad := writeGitopsDir(t, map[string]string{
		"principals/a.yaml": "kind: Robot\nschema_version: 1\n",
		"grants/g.yaml":     "kind: DatabaseGrants\nschema_version: 2\ngrants: []\n"})
	code, out, _ = runConfigCLI(t, "", "validate", bad)
	if code != configExitProblem || !strings.Contains(out, "invalid, 2 problems") ||
		!strings.Contains(out, "principals/a.yaml:1: unknown kind") ||
		!strings.Contains(out, "grants/g.yaml:2: schema_version") {
		t.Fatalf("invalid directory: exit %d, out %q", code, out)
	}
}

// serveGitopsFake answers GET /api/v1/gitops with body.
func serveGitopsFake(t *testing.T, body any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/gitops" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestConfigCmd_DiffDirectory(t *testing.T) {
	root := writeGitopsDir(t, map[string]string{"principals/ci-bot.yaml": cliPrincipalFile})
	dir, err := gitops.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	report := func(hash string, changes []gitops.Change, errText string) map[string]any {
		return map[string]any{"configured": true, "report": gitops.Report{
			Directory: "/etc/sage/policy", Mode: gitops.ModeDryRun, Hash: hash,
			Changes: changes, Error: errText}}
	}
	cases := []struct {
		name string
		body any
		code int
		want string
	}{
		{"same files, nothing to do", report(dir.Hash, nil, ""), configExitOK,
			"no differences"},
		{"same files, a pending create", report(dir.Hash, []gitops.Change{{
			Kind: gitops.KindPrincipal, Name: "ci-bot", Op: gitops.OpCreate,
			File: "principals/ci-bot.yaml"}}, ""), configExitProblem,
			"create principal ci-bot"},
		{"other files", report("ffff", nil, ""), configExitProblem, "has not reconciled"},
		{"last reconcile failed", report(dir.Hash, nil, "connection refused"),
			configExitProblem, "failed: connection refused"},
		{"not configured", map[string]any{"configured": false}, configExitProblem,
			"no policy.directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runConfigCLI(t, serveGitopsFake(t, tc.body), "diff", root)
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, out %q, err %q; want %d with %q", code, out, errOut,
					tc.code, tc.want)
			}
		})
	}
	bad := writeGitopsDir(t, map[string]string{"principals/a.yaml": "kind: Robot\n"})
	if code, _, errOut := runConfigCLI(t, serveGitopsFake(t, nil), "diff", bad); code !=
		configExitError || !strings.Contains(errOut, "fix the files first") {
		t.Fatalf("diff of an invalid directory: exit %d, err %q", code, errOut)
	}
}

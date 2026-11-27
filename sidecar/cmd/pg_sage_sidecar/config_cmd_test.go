package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// cli-01b: `config validate | diff | export` in process through runCLI.
// diff and export run against a fake sidecar here and against the real
// router in config_cmd_db_test.go. No concurrent access tests: each is a
// single-shot command, and the routes they read write nothing.

const configFixtureSecret = "fixture-secret-Kp4vW8n2"

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runConfigCLI(t *testing.T, url string, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{"SAGE_API_TOKEN": cliTestToken}
	if url != "" {
		env["SAGE_API_URL"] = url
	}
	return runCLIEnv(t, env, "", append([]string{configSubcommand}, args...)...)
}

// Happy path: the shipped example validates.
func TestConfigCmd_ValidateExample(t *testing.T) {
	code, out, errOut := runConfigCLI(t, "", "validate", "../../config.example.yaml")
	if code != configExitOK || !strings.Contains(out, "config.example.yaml: valid\n") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
}

// Invalid input: an unknown key, an out-of-range value and a wrong type
// are each printed with their key path; exit 2.
func TestConfigCmd_ValidatePrintsEveryKeyPath(t *testing.T) {
	path := writeTestConfig(t, "collector:\n  batch_size: 0\n  interval_seconds: often\n"+
		"anaylzer:\n  interval_seconds: 60\n")
	code, out, errOut := runConfigCLI(t, "", "validate", path)
	if code != configExitProblem {
		t.Fatalf("exit %d; want 2 (out %q, err %q)", code, out, errOut)
	}
	for _, want := range []string{"invalid, 3 problems",
		"  collector.batch_size (line 2): must be 1-",
		"  collector.interval_seconds (line 3): cannot unmarshal",
		"  anaylzer (line 4): unknown key"} {
		if !strings.Contains(out, want) {
			t.Fatalf("out %q; want %q", out, want)
		}
	}
}

// Nil/empty: an empty file is valid and says every setting is a default.
func TestConfigCmd_ValidateEmptyFileNotesDefaults(t *testing.T) {
	code, out, _ := runConfigCLI(t, "", "validate", writeTestConfig(t, ""))
	if code != configExitOK || !strings.Contains(out, "every setting is its default") {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

// Error propagation: a missing file prints the OS error and the path and
// exits 1, not 2: nothing was validated.
func TestConfigCmd_ValidateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	code, out, errOut := runConfigCLI(t, "", "validate", path)
	if code != configExitError || !strings.Contains(errOut, path) || out != "" {
		t.Fatalf("exit %d, out %q, err %q; want 1 naming %s", code, out, errOut, path)
	}
}

// Invalid input: usage errors are 64, never 2.
func TestConfigCmd_UsageErrors(t *testing.T) {
	file := writeTestConfig(t, "")
	cases := [][]string{
		{}, {"frob"}, {"validate"}, {"validate", file, "extra"}, {"diff"},
		{"export", "positional"}, {"validate", "--nope", file},
	}
	for _, args := range cases {
		if code, _, errOut := runConfigCLI(t, "", args...); code != configExitUsage ||
			!strings.Contains(errOut, "usage:") {
			t.Fatalf("%v: exit %d, err %q; want 64 with usage", args, code, errOut)
		}
	}
	if code, out, _ := runConfigCLI(t, "", "--help"); code != 0 ||
		!strings.Contains(out, configExportUsage) {
		t.Fatalf("--help: exit %d, out %q", code, out)
	}
}

// configFake is a sidecar answering the two config reads.
type configFake struct {
	mu       sync.Mutex
	requests []*http.Request
	global   map[string]any
	yaml     string
	status   int
}

func (f *configFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":"nope","code":"forbidden"}`))
		return
	}
	switch r.URL.Path {
	case "/api/v1/config/global":
		_ = json.NewEncoder(w).Encode(map[string]any{"config": f.global})
	case "/api/v1/config/yaml":
		_ = json.NewEncoder(w).Encode(map[string]any{"yaml": f.yaml})
	default:
		http.NotFound(w, r)
	}
}

func serveConfigFake(t *testing.T, f *configFake) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

// readModelOf is the sidecar's answer for a file nobody overrode.
func readModelOf(t *testing.T, path string) map[string]any {
	t.Helper()
	check, err := config.CheckFile(path)
	if err != nil || len(check.Problems) > 0 {
		t.Fatalf("fixture invalid: %v %v", err, check.Problems)
	}
	raw, err := json.Marshal(store.ConfigReadModel(check.Config))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Happy path and state transition: with no override diff is clean (exit
// 0); one stored override prints exactly that key (exit 2).
func TestConfigCmd_DiffShowsTheOneOverride(t *testing.T) {
	path := writeTestConfig(t, "retention:\n  snapshots_days: 30\n")
	f := &configFake{global: readModelOf(t, path)}
	url := serveConfigFake(t, f)
	code, out, errOut := runConfigCLI(t, url, "diff", path)
	if code != configExitOK || !strings.Contains(out, "no differences") {
		t.Fatalf("clean: exit %d, out %q, err %q", code, out, errOut)
	}
	f.global["retention.snapshots_days"] = map[string]any{"value": 21.0, "source": "override"}
	code, out, errOut = runConfigCLI(t, url, "diff", path, "--url", url)
	if code != configExitProblem {
		t.Fatalf("override: exit %d, out %q, err %q", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "KEY") ||
		strings.Join(strings.Fields(lines[1]), " ") !=
			"retention.snapshots_days 30 21 stored override" ||
		lines[2] != "1 key(s) differ from "+path {
		t.Fatalf("override: lines %q", lines)
	}
	if got := f.requests[0].Header.Get("Authorization"); got != "Bearer "+cliTestToken {
		t.Fatalf("Authorization %q", got)
	}
}

// Boundary: an override equal to the file still prints (it masks later
// file edits), and a value from elsewhere is labelled env.
func TestConfigCmd_DiffEqualOverrideAndEnvSource(t *testing.T) {
	path := writeTestConfig(t, "collector:\n  batch_size: 500\n")
	f := &configFake{global: readModelOf(t, path)}
	f.global["collector.batch_size"] = map[string]any{"value": 500.0, "source": "override"}
	f.global["collector.max_queries"] = map[string]any{"value": 9.0, "source": "file/default"}
	code, out, _ := runConfigCLI(t, serveConfigFake(t, f), "diff", path)
	if code != configExitProblem || !strings.Contains(out, "2 key(s) differ") {
		t.Fatalf("exit %d, out %q", code, out)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			fields[parts[0]] = strings.Join(parts[1:], " ")
		}
	}
	if fields["collector.batch_size"] != "500 500 stored override" ||
		!strings.HasSuffix(fields["collector.max_queries"], " 9 env") {
		t.Fatalf("rows %v", fields)
	}
}

// Boundary: a secret never prints in diff, even when it differs.
func TestConfigCmd_DiffNeverPrintsASecret(t *testing.T) {
	t.Setenv("SAGE_TEST_LLM_KEY", configFixtureSecret)
	path := writeTestConfig(t, "llm:\n  api_key: ${SAGE_TEST_LLM_KEY}\n")
	f := &configFake{global: readModelOf(t, path)}
	f.global["llm.api_key"] = map[string]any{"value": "********zzzz", "source": "override"}
	code, out, errOut := runConfigCLI(t, serveConfigFake(t, f), "diff", path)
	if code != configExitProblem || !strings.Contains(out, "<secret> (differs)") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	for _, leak := range []string{configFixtureSecret, "W8n2", "zzzz"} {
		if strings.Contains(out+errOut, leak) {
			t.Fatalf("diff printed %q: %q", leak, out)
		}
	}
}

// Error propagation: a sidecar that is down is named by its host (exit
// 1); a refused token exits 3; an invalid file is not compared.
func TestConfigCmd_DiffAndExportErrors(t *testing.T) {
	path := writeTestConfig(t, "")
	srv := httptest.NewServer(http.NotFoundHandler())
	down := srv.URL
	srv.Close()
	host := strings.TrimPrefix(down, "http://")
	for _, args := range [][]string{{"diff", path}, {"export"}} {
		code, _, errOut := runConfigCLI(t, down, args...)
		if code != configExitError || !strings.Contains(errOut, "cannot reach the sidecar at "+
			host) {
			t.Fatalf("%v down: exit %d, err %q; want 1 naming %s", args, code, errOut, host)
		}
		forbidden := serveConfigFake(t, &configFake{status: http.StatusForbidden})
		if code, _, errOut := runConfigCLI(t, forbidden, args...); code != configExitAuth {
			t.Fatalf("%v 403: exit %d, err %q; want 3", args, code, errOut)
		}
		code, _, errOut = runCLIEnv(t, map[string]string{"SAGE_API_URL": down}, "",
			append([]string{configSubcommand}, args...)...)
		if code != configExitAuth || !strings.Contains(errOut, "SAGE_API_TOKEN is not set") {
			t.Fatalf("%v no token: exit %d, err %q", args, code, errOut)
		}
	}
	bad := writeTestConfig(t, "collector:\n  batch_size: -3\n")
	f := &configFake{global: map[string]any{}}
	code, out, _ := runConfigCLI(t, serveConfigFake(t, f), "diff", bad)
	if code != configExitError || !strings.Contains(out, "collector.batch_size") ||
		len(f.requests) != 0 {
		t.Fatalf("invalid file: exit %d, out %q, %d requests", code, out, len(f.requests))
	}
}

// Happy path and nil/empty: export prints the sidecar's YAML, asks for
// the overrides only with --only-overrides, and prints {} for none.
func TestConfigCmd_ExportPrintsYAML(t *testing.T) {
	f := &configFake{yaml: "collector:\n    batch_size: 500\n"}
	url := serveConfigFake(t, f)
	code, out, errOut := runConfigCLI(t, url, "export")
	if code != configExitOK || out != f.yaml || f.requests[0].URL.RawQuery != "" {
		t.Fatalf("export: exit %d, out %q, err %q, query %q", code, out, errOut,
			f.requests[0].URL.RawQuery)
	}
	f.yaml = "{}"
	code, out, _ = runConfigCLI(t, url, "export", "--only-overrides")
	if code != configExitOK || out != "{}\n" ||
		f.requests[1].URL.Query().Get("overrides") != "1" {
		t.Fatalf("only overrides: exit %d, out %q, query %q", code, out,
			f.requests[1].URL.RawQuery)
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(old.Close)
	if code, out, errOut := runConfigCLI(t, old.URL, "export"); code != configExitError ||
		out != "" || !strings.Contains(errOut, "has no yaml") {
		t.Fatalf("no yaml: exit %d, out %q, err %q", code, out, errOut)
	}
}

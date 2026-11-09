package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ci-10: the drift scan covers every runtime reader under internal/, cmd/
// and the bench harnesses, and each entry says whether operators
// (runtime) or contributors (contributor) set it. Runtime entries render
// into docs/configuration.md, contributor entries into the "Test
// environment" table of CONTRIBUTING.md.
//
// No concurrent-access tests: the registry is an immutable package value
// and the renderers are pure functions of it.

// contributorPrefixes name the variables only tests and benches read.
var contributorPrefixes = []string{"SAGE_TEST_", "SAGE_BENCH_", "SAGE_SAFETY_BENCH_"}

func isContributorName(name string) bool {
	for _, prefix := range contributorPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func TestEveryEnvVarIsRegistered(t *testing.T) {
	literals := sourceEnvLiterals(t)
	// One literal per tree outside internal/config and cmd/pg_sage_sidecar:
	// the scan must reach each of them.
	for _, want := range []string{
		"SAGE_AUTH_MODE",                        // internal/appmode, internal/auth
		"SAGE_DATABRICKS_ADMIN_EMAIL",           // internal/auth
		"SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS", // internal/auth
		"SAGE_LAKEBASE_ENDPOINTS",               // internal/databricksauth
		"SAGE_TEST_DATABASE_URL",                // internal/testdb
		"SAGE_TEST_SECOND_CLUSTER_URL",          // internal/testdb
		"SAGE_BENCH_RUN",                        // sre-bench
		"SAGE_BENCH_FAMILIES",                   // sre-bench
		"SAGE_SAFETY_BENCH_RUN",                 // safety-bench
	} {
		if !containsString(literals, want) {
			t.Errorf("scan missed %s", want)
		}
	}
	// Only test files read these; the scan must not count them.
	for _, testOnly := range []string{"SAGE_TEST_PSS_SMALL_URL", "SAGE_E2E_COVERAGE_DIR",
		"SAGE_STX_X"} {
		if containsString(literals, testOnly) {
			t.Errorf("scan counted %s, which only _test.go files read", testOnly)
		}
	}
	if gaps := envRegistryGaps(EnvVars(), literals); len(gaps) != 0 {
		t.Fatalf("SAGE_* variables read but not in env_registry.go: %v; add them "+
			"there (Kind runtime or contributor), then run `go run ./cmd/gen_config_meta "+
			"-env-only -env-docs ../docs/configuration.md -contrib-docs ../CONTRIBUTING.md`",
			gaps)
	}
}

func TestEnvRegistryKindsMatchTheReaders(t *testing.T) {
	contributors := 0
	for _, entry := range EnvVars() {
		switch entry.Kind {
		case EnvRuntime, EnvContributor:
		default:
			t.Errorf("%s: kind %q, want %q or %q", entry.Name, entry.Kind,
				EnvRuntime, EnvContributor)
			continue
		}
		want := EnvRuntime
		if isContributorName(entry.Name) {
			want = EnvContributor
			contributors++
		}
		if entry.Kind != want {
			t.Errorf("%s: kind %q, want %q", entry.Name, entry.Kind, want)
		}
	}
	if contributors < 10 {
		t.Fatalf("only %d contributor entries; the test and bench variables are missing",
			contributors)
	}
}

func TestEnvVarsMarkdownSplitsByKind(t *testing.T) {
	runtime, contributor := EnvVarsMarkdown(), ContributorEnvVarsMarkdown()
	const header = "| Variable | Default | Description |\n|---|---|---|\n"
	if !strings.HasPrefix(contributor, header) {
		t.Fatalf("contributor table header = %q", strings.SplitN(contributor, "\n", 3)[:2])
	}
	for _, entry := range EnvVars() {
		cell := "| `" + entry.Name + "`"
		inRuntime := strings.Contains(runtime, cell+" ")
		inContributor := strings.Contains(contributor, cell+" ")
		if entry.Kind == EnvContributor && (inRuntime || !inContributor) {
			t.Errorf("%s: contributor entry in runtime=%v contributor=%v",
				entry.Name, inRuntime, inContributor)
		}
		if entry.Kind == EnvRuntime && (!inRuntime || inContributor) {
			t.Errorf("%s: runtime entry in runtime=%v contributor=%v",
				entry.Name, inRuntime, inContributor)
		}
	}
	if !strings.Contains(contributor, "| `SAGE_TEST_DATABASE_URL` | (none) | ") {
		t.Error("contributor table lacks the SAGE_TEST_DATABASE_URL row")
	}
}

// The generated section of CONTRIBUTING.md matches the registry.
func TestContributingTestEnvTableMatchesRegistry(t *testing.T) {
	path := filepath.Join("..", "..", "..", "CONTRIBUTING.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got, err := ContributorEnvDocsSection(strings.ReplaceAll(string(body), "\r\n", "\n"))
	if err != nil {
		t.Fatalf("CONTRIBUTING.md: %v", err)
	}
	if got != ContributorEnvVarsMarkdown() {
		t.Fatal("CONTRIBUTING.md test-environment table drifted from env_registry.go; " +
			"regenerate with `go run ./cmd/gen_config_meta -env-only " +
			"-contrib-docs ../CONTRIBUTING.md`")
	}
}

func TestReplaceContributorEnvDocsSection(t *testing.T) {
	doc := "intro\n" + ContributorEnvDocsStart + "\nold\n" + ContributorEnvDocsEnd + "\ntail\n"
	got, err := ReplaceContributorEnvDocsSection(doc)
	if err != nil {
		t.Fatalf("ReplaceContributorEnvDocsSection: %v", err)
	}
	want := "intro\n" + ContributorEnvDocsStart + "\n" + ContributorEnvVarsMarkdown() +
		ContributorEnvDocsEnd + "\ntail\n"
	if got != want {
		t.Fatalf("replaced doc = %q, want %q", got, want)
	}
	// The runtime markers are not the contributor markers, and vice versa.
	runtimeDoc := EnvDocsStart + "\nold\n" + EnvDocsEnd + "\n"
	if _, err := ReplaceContributorEnvDocsSection(runtimeDoc); err == nil ||
		!strings.Contains(err.Error(), "test-env-vars:start") {
		t.Errorf("contributor replace on runtime markers = %v, want a marker error", err)
	}
	if _, err := ReplaceEnvDocsSection(doc); err == nil {
		t.Error("runtime replace accepted the contributor markers")
	}
	reversed := ContributorEnvDocsEnd + "\n" + ContributorEnvDocsStart + "\n"
	for _, bad := range []string{"", reversed} {
		if _, err := ContributorEnvDocsSection(bad); err == nil {
			t.Errorf("ContributorEnvDocsSection(%q) succeeded, want a marker error", bad)
		}
	}
}

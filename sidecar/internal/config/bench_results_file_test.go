package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benches-09: "pg_sage bench install" reads sre.autonomy.bench_results_path
// from the sidecar's config file without loading (or validating) the rest.

func writeBenchConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBenchResultsPathFromFile(t *testing.T) {
	t.Setenv("SAGE_TEST_BENCH_DIR", "/var/lib/pg_sage/bench")
	cases := map[string]string{
		"sre:\n  autonomy:\n    bench_results_path: /etc/pg_sage/bench\n": "/etc/pg_sage/bench",
		"sre:\n  autonomy:\n    bench_results_path: '  /x/y  '\n":         "/x/y",
		"sre:\n  autonomy:\n    bench_results_path: ${SAGE_TEST_BENCH_DIR}\n": "/var/lib/" +
			"pg_sage/bench",
		"sre:\n  autonomy:\n    enabled: true\n": "",
		"mode: fleet\n":                          "",
		"":                                       "",
	}
	for body, want := range cases {
		got, err := BenchResultsPathFromFile(writeBenchConfig(t, body))
		if err != nil || got != want {
			t.Errorf("%q: bench_results_path = %q (%v), want %q", body, got, err, want)
		}
	}
}

func TestBenchResultsPathFromFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := BenchResultsPathFromFile(missing); err == nil ||
		!strings.Contains(err.Error(), "nope.yaml") {
		t.Fatalf("missing file = %v, want an error naming it", err)
	}
	bad := writeBenchConfig(t, "sre: [unclosed\n")
	if _, err := BenchResultsPathFromFile(bad); err == nil {
		t.Fatal("malformed YAML was accepted")
	}
	wrongType := writeBenchConfig(t, "sre:\n  autonomy:\n    bench_results_path: [a, b]\n")
	if _, err := BenchResultsPathFromFile(wrongType); err == nil {
		t.Fatal("a list bench_results_path was accepted")
	}
}

// No concurrent-access test: a pure read of one file with no shared state.

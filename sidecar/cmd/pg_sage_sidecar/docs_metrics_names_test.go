package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-09: every pg_sage_* name the operator docs, the Grafana
// README and the alert rules mention is a registered family, so no guide
// tells an operator to alert on a family that does not exist (the alert
// table once named pg_sage_executor_actions_total). Retired names may
// appear only inside the rename table's markers.

// No concurrent access tests: a static check over files.

const (
	docsRoot     = "../../.."
	renamesStart = "<!-- metrics-renames:start -->"
	renamesEnd   = "<!-- metrics-renames:end -->"
)

var docMetricToken = regexp.MustCompile(`\bpg_sage_[a-z0-9_]+`)

// docNonMetricTokens are pg_sage_ names in the docs that are not metrics:
// the binary and its release archives, the Grafana dashboard file, secret
// and file names in the deployment guides, and the auto_explain log
// rejection reason pg_sage_self (rca-04), and the migration ledger column
// pg_sage_version (config-04b) and the bench report stamps pg_sage_version and
// pg_sage_commit (benches-09), and the apply_migration contract script's
// dollar-quote tag pg_sage_guard (migration-06).
var docNonMetricTokens = map[string]bool{
	"pg_sage_sidecar": true, "pg_sage_linux_amd64": true, "pg_sage_darwin_arm64": true,
	"pg_sage_dashboard": true, "pg_sage_metrics_token": true,
	"pg_sage_break_glass_hash": true, "pg_sage_app_sp_secret": true, "pg_sage_app": true,
	"pg_sage_self": true, "pg_sage_version": true, "pg_sage_commit": true,
	"pg_sage_guard": true,
}

// docTokenProblem says why token is not a registered metric name, or "".
func docTokenProblem(token string, retired map[string]bool, inRenames bool) string {
	if _, ok := metricsreg.Lookup(token); ok || docNonMetricTokens[token] {
		return ""
	}
	if retired[token] {
		if inRenames {
			return ""
		}
		return "is retired; only the rename table may name it"
	}
	for _, suffix := range []string{"_sum", "_count"} {
		if f, ok := metricsreg.Lookup(strings.TrimSuffix(token, suffix)); ok &&
			f.Type == metricsreg.Summary && strings.HasSuffix(token, suffix) {
			return ""
		}
	}
	if strings.HasSuffix(token, "_") { // a prefix, as in pg_sage_self_*
		for _, f := range metricsreg.Families() {
			if strings.HasPrefix(f.Name, token) {
				return ""
			}
		}
	}
	return "is not a registered family"
}

// docProblems lists the bad metric names of one document.
func docProblems(name, body string, retired map[string]bool) []string {
	var out []string
	inRenames := false
	for n, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case renamesStart:
			inRenames = true
		case renamesEnd:
			inRenames = false
		}
		for _, tok := range docMetricToken.FindAllString(line, -1) {
			if p := docTokenProblem(tok, retired, inRenames); p != "" {
				out = append(out, name+":"+strconv.Itoa(n+1)+": "+tok+" "+p)
			}
		}
	}
	return out
}

func retiredNames() map[string]bool {
	out := map[string]bool{}
	for _, r := range metricsreg.RetiredFamilies() {
		out[r.Name] = true
	}
	return out
}

func TestDocsMetricsNamesAreRegistered(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(docsRoot, "docs", "*.md"))
	if err != nil || len(files) < 10 {
		t.Fatalf("list docs: %v (%d files)", err, len(files))
	}
	files = append(files, filepath.Join(docsRoot, "grafana", "README.md"),
		filepath.Join(docsRoot, "deploy", "prometheus", "pg_sage.rules.yml"))
	var problems []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		rel, _ := filepath.Rel(docsRoot, f)
		problems = append(problems, docProblems(filepath.ToSlash(rel), string(body),
			retiredNames())...)
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("docs name metrics that /metrics does not export:\n%s",
			strings.Join(problems, "\n"))
	}
}

// Invalid input and boundaries of the token rules.
func TestDocsMetricsTokenRules(t *testing.T) {
	retired := map[string]bool{"pg_sage_cache_hit_ratio": true}
	cases := map[string]struct {
		token     string
		inRenames bool
		bad       bool
	}{
		"registered":            {"pg_sage_database_up", false, false},
		"never existed":         {"pg_sage_executor_actions_total", false, true},
		"retired outside table": {"pg_sage_cache_hit_ratio", false, true},
		"retired in table":      {"pg_sage_cache_hit_ratio", true, false},
		"summary sum":           {"pg_sage_sre_probe_queue_wait_seconds_sum", false, false},
		"gauge with _count":     {"pg_sage_database_up_count", false, true},
		"family prefix":         {"pg_sage_self_", false, false},
		"prefix of nothing":     {"pg_sage_nothing_", false, true},
		"non-metric name":       {"pg_sage_sidecar", false, false},
	}
	for name, c := range cases {
		if got := docTokenProblem(c.token, retired, c.inRenames); (got != "") != c.bad {
			t.Errorf("%s: docTokenProblem(%q) = %q, want bad=%v", name, c.token, got, c.bad)
		}
	}
	body := "intro\n" + renamesStart + "\n| `pg_sage_cache_hit_ratio` |\n" + renamesEnd +
		"\nalert on `pg_sage_cache_hit_ratio` and `pg_sage_nope`\n"
	got := docProblems("x.md", body, retired)
	want := []string{"x.md:5: pg_sage_cache_hit_ratio is retired; only the rename " +
		"table may name it", "x.md:5: pg_sage_nope is not a registered family"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems = %q, want %q", got, want)
	}
	if got := docProblems("empty.md", "", retired); len(got) != 0 {
		t.Fatalf("empty doc problems = %v", got)
	}
}

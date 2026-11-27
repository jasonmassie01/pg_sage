package llm

import (
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

// llm-10: the costing guide's consumer table must name every package
// that calls a model, so an operator sizing spend sees every feature.
// The call-site guard (llm-09) is the source of truth for which exist.
//
// No concurrent-access tests: this reads two files and compares sets.

const costingGuidePath = "../../../docs/llm-costing.md"

var consumerPathRE = regexp.MustCompile("`((?:internal|cmd)/[a-z0-9_/]+)`")

// consumerTable returns the code paths named in the table under
// "## Model consumers".
func consumerTable(doc string) map[string]bool {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	start := strings.Index(doc, "\n## Model consumers\n")
	if start < 0 {
		return nil
	}
	paths := map[string]bool{}
	for _, line := range strings.Split(doc[start+1:], "\n")[1:] {
		if strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		for _, m := range consumerPathRE.FindAllStringSubmatch(line, -1) {
			paths[m[1]] = true
		}
	}
	return paths
}

// siteDir is the package directory of a guard site
// ("internal/rca/narrate.go:runNarration" is "internal/rca").
func siteDir(site string) string {
	file, _, _ := strings.Cut(site, ":")
	return path.Dir(file)
}

func readCostingGuide(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(costingGuidePath)
	if err != nil {
		t.Fatalf("read %s: %v", costingGuidePath, err)
	}
	return string(raw)
}

func TestCostingGuideNamesEveryModelConsumer(t *testing.T) {
	documented := consumerTable(readCostingGuide(t))
	if len(documented) == 0 {
		t.Fatal("llm-costing.md has no \"## Model consumers\" table naming code paths")
	}
	live := map[string]bool{}
	for _, r := range guardSites(t, sidecarRoot) {
		if dir := siteDir(r.site); !strings.HasPrefix(dir, "internal/llm") {
			live[dir] = true
		}
	}
	for _, site := range knownCallSites {
		live[siteDir(site)] = true
	}
	for dir := range live {
		if !documented[dir] {
			t.Errorf("model consumer %s is missing from the costing guide's table", dir)
		}
	}
	for dir := range documented {
		if !live[dir] {
			t.Errorf("costing guide lists %s, which makes no model call", dir)
		}
	}
}

func TestConsumerTableParsing(t *testing.T) {
	doc := "# G\n\n## Model consumers\n\nThe `internal/notatable` prose.\n\n" +
		"| Feature | Code |\n|---|---|\n| Briefing | `internal/briefing` |\n" +
		"| RCA | `internal/rca`, `internal/sre/runbook` |\n\n## Next\n\n" +
		"| Other | `internal/after` |\n"
	got := consumerTable(doc)
	want := []string{"internal/briefing", "internal/rca", "internal/sre/runbook"}
	if len(got) != len(want) {
		t.Fatalf("consumerTable = %v, want %v", got, want)
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("consumerTable missed %s (got %v)", p, got)
		}
	}
	if consumerTable("") != nil || consumerTable("## Defaults\n") != nil {
		t.Error("a doc without the section must yield nil")
	}
}

func TestSiteDir(t *testing.T) {
	cases := map[string]string{
		"internal/rca/narrate.go:runNarration":     "internal/rca",
		"internal/sre/runbook/compile.go:attempt":  "internal/sre/runbook",
		"cmd/pg_sage_sidecar/x.go:f":               "cmd/pg_sage_sidecar",
		"internal/advisor/llmcall.go:chatAdvisor":  "internal/advisor",
		"internal/schema/lint/llm_jsonb.go:a.b":    "internal/schema/lint",
		"internal/ask/run.go:budgetedModel.ChatWT": "internal/ask",
	}
	for in, want := range cases {
		if got := siteDir(in); got != want {
			t.Errorf("siteDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// The guide's optimizer section must match ForPurpose: only the tuning
// agent's purposes reach the optimizer client.
func TestCostingGuideOptimizerRouting(t *testing.T) {
	general, optimizer := &Client{}, &Client{}
	m := NewManager(general, optimizer, true)
	for _, purpose := range []string{"index_optimization", "query_tuning"} {
		if m.ForPurpose(purpose) != optimizer {
			t.Errorf("ForPurpose(%q) is not the optimizer client", purpose)
		}
	}
	for _, purpose := range []string{"advisor", "briefing", ""} {
		if m.ForPurpose(purpose) != general {
			t.Errorf("ForPurpose(%q) is not the general client", purpose)
		}
	}
	doc := readCostingGuide(t)
	if strings.Contains(doc, "diagnose") {
		t.Error("llm-costing.md names a \"diagnose\" feature that does not exist")
	}
	if !strings.Contains(doc, "`internal/tuning`") {
		t.Error("llm-costing.md does not tie the optimizer client to internal/tuning")
	}
}

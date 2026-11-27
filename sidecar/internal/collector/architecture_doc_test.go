package collector

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access, database or invalid-input tests: this checks a
// document against the code's category list.

// docCategories reads the snapshot categories docs/architecture.md lists in
// its "Each snapshot holds one row per category:" sentence.
func docCategories(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "architecture.md"))
	if err != nil {
		t.Fatalf("read docs/architecture.md: %v", err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	const lead = "Each snapshot holds one row per category:"
	start := strings.Index(doc, lead)
	if start < 0 {
		t.Fatalf("docs/architecture.md has no %q sentence", lead)
	}
	// The category names are the backticked words of the sentence, outside
	// parentheses.
	sentence := regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(doc[start+len(lead):], "")
	if end := regexp.MustCompile(`\.\s`).FindStringIndex(sentence); end != nil {
		sentence = sentence[:end[0]]
	}
	var out []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(sentence, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// codeCategories is every category a cycle can store: the catalog steps,
// replication and the extra steps of a PG16+ collector with an advisor.
func codeCategories(t *testing.T) []string {
	t.Helper()
	cfg := &config.Config{}
	cfg.Advisor.Enabled = true
	c := New(nil, cfg, 170000, func(string, string, ...any) {})
	names := []string{"replication"}
	for _, st := range append(c.catalogSteps(), c.extraSteps()...) {
		names = append(names, st.name)
	}
	sort.Strings(names)
	rows, err := snapshotRows(&Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	stored := make([]string, 0, len(rows))
	for _, r := range rows {
		stored = append(stored, r.Category)
	}
	if strings.Join(stored, ",") != strings.Join(names, ",") {
		t.Fatalf("stored categories %v differ from the collected ones %v", stored, names)
	}
	return names
}

func TestArchitectureDocListsTheSnapshotCategories(t *testing.T) {
	doc, code := docCategories(t), codeCategories(t)
	if len(code) < 12 {
		t.Fatalf("only %d categories from the code: %v", len(code), code)
	}
	if strings.Join(doc, ",") != strings.Join(code, ",") {
		t.Fatalf("docs/architecture.md lists %v, the collector stores %v", doc, code)
	}
}

func TestArchitectureDocDoesNotListUnreadViews(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pg_sequences") {
		t.Fatal("docs/architecture.md names pg_sequences, which the collector no longer reads")
	}
}

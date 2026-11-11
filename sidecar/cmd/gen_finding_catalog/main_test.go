package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/findingcat"
)

// No concurrent access tests: the generator is a one-shot build tool over
// the read-only registry.

var docsDir = filepath.Join("..", "..", "..", "docs")

func readDoc(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docsDir, name))
	if err != nil {
		t.Fatalf("read docs/%s: %v", name, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

func TestGeneratedCatalogIsCommitted(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := readDoc(t, "generated/finding-catalog.md"); got != want {
		t.Fatal("docs/generated/finding-catalog.md is stale: run " +
			"`go run ./cmd/gen_finding_catalog` from sidecar/")
	}
	again, _ := Render()
	if again != got {
		t.Fatal("Render is not deterministic")
	}
}

func TestCatalogRowsCarryRegistryFields(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"| [`unused_index`](../findings.md#unused_index) | analyzer | info, warning |",
		"`analyzer.unused_index_window_days`",
		"`drop_unused_index` (moderate)",
		"| [`schema_lint:*`](../findings.md#schema_lint) |",
		"## query_tuning",
		"also shown under `query_tuning`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered catalog lacks %q", want)
		}
	}
	for _, c := range findingcat.All() {
		if !strings.Contains(got, "(../findings.md#"+Anchor(c)+")") {
			t.Errorf("category %q has no row in the catalog", c.Name)
		}
	}
}

var (
	headingRE = regexp.MustCompile("(?m)^#{2,4} `?([a-z0-9_]+)`?\\s*$")
	anchorRE  = regexp.MustCompile(`<a id="([a-z0-9_]+)"></a>`)
)

func TestEveryCategoryHasAFindingsAnchor(t *testing.T) {
	doc := readDoc(t, "findings.md")
	anchors := map[string]bool{}
	for _, m := range headingRE.FindAllStringSubmatch(doc, -1) {
		anchors[m[1]] = true
	}
	for _, m := range anchorRE.FindAllStringSubmatch(doc, -1) {
		anchors[m[1]] = true
	}
	for _, c := range findingcat.All() {
		if !anchors[Anchor(c)] {
			t.Errorf("docs/findings.md has no section or anchor %q for %q", Anchor(c), c.Name)
		}
	}
	if !strings.Contains(doc, "generated/finding-catalog.md") {
		t.Error("docs/findings.md does not link the generated catalog")
	}
}

func TestAnchorStripsFamilyColon(t *testing.T) {
	cases := map[string]string{"schema_lint:": "schema_lint", "unused_index": "unused_index",
		"": ""}
	for name, want := range cases {
		if got := Anchor(findingcat.Category{Name: name}); got != want {
			t.Errorf("Anchor(%q) = %q, want %q", name, got, want)
		}
	}
}

// yamlPaths collects every dotted YAML key of a config struct.
func yamlPaths(t reflect.Type, prefix string, out map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		out[path] = true
		yamlPaths(f.Type, path+".", out)
	}
}

func TestConfigKeysExist(t *testing.T) {
	paths := map[string]bool{}
	yamlPaths(reflect.TypeOf(config.DefaultConfig()), "", paths)
	if !paths["analyzer.unused_index_window_days"] {
		t.Fatal("the YAML walker found no analyzer keys")
	}
	for _, c := range findingcat.All() {
		for _, key := range c.ConfigKeys {
			if !paths[key] {
				t.Errorf("category %q names config key %q, which does not exist", c.Name, key)
			}
		}
	}
}

func TestActionsHaveExecutorContracts(t *testing.T) {
	for _, c := range findingcat.All() {
		for _, action := range c.Actions {
			if _, ok := executor.ContractForActionType(action); !ok {
				t.Errorf("category %q names action %q, which has no contract", c.Name, action)
			}
		}
	}
	if tier := contractTier("no_such_action"); tier != "" {
		t.Errorf("contractTier(unknown) = %q, want \"\"", tier)
	}
}

func TestRunWritesAndChecks(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sub", "catalog.md")
	if err := run([]string{"-out", out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	want, _ := Render()
	if string(written) != want {
		t.Fatal("run wrote something other than Render()")
	}
	if err := run([]string{"-check", "-out", out}); err != nil {
		t.Fatalf("check of a fresh file: %v", err)
	}
	if err := os.WriteFile(out, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-check", "-out", out})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("check of a stale file = %v, want a stale error", err)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	if err := run([]string{"-bogus"}); err == nil {
		t.Error("run accepted an unknown flag")
	}
	missing := filepath.Join(t.TempDir(), "absent.md")
	err := run([]string{"-check", "-out", missing})
	if err == nil || !strings.Contains(err.Error(), "absent.md") {
		t.Errorf("check of a missing file = %v, want an error naming it", err)
	}
}

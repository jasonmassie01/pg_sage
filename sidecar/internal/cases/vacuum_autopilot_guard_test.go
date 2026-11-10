package cases

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// vacuum-10: every category vacuumAutopilotCandidate maps must have a
// producer. A branch that nothing produces is dead code whose own tests
// keep the suite green. The handled categories are read from the switch
// itself, so a branch added later is checked too.

// No concurrent access tests: the guard reads source files and a static
// registry, and shares no state.

// outOfRegistryProducers are the producers outside the analyzer's snapshot
// registry (AllRules) and the tuning agent (TuningCategories): the package
// that emits each category and the source pattern that emits it.
var outOfRegistryProducers = []struct{ category, pkg, pattern string }{
	{"xid_wraparound", "analyzer", `Category:\s+"xid_wraparound"`},
	{"bloat_remediation", "advisor", `parseLLMFindings\([^)]*"bloat_remediation"`},
	{"schema_lint:lint_bloated_table", "schema/lint", lintRuleID("lint_bloated_table")},
	{"schema_lint:lint_txid_age", "schema/lint", lintRuleID("lint_txid_age")},
	{"schema_lint:lint_mxid_age", "schema/lint", lintRuleID("lint_mxid_age")},
}

// lintRuleID matches a schema lint rule's ID method; the lint runner files
// its findings under "schema_lint:" + ID.
func lintRuleID(id string) string {
	return `ID\(\) string\s*\{\s*return "` + regexp.QuoteMeta(id) + `"`
}

func TestVacuumAutopilotCategoriesAllHaveProducers(t *testing.T) {
	registry := registryCategories()
	handled := map[string]bool{}
	for _, category := range vacuumAutopilotCategories(t) {
		handled[category] = true
		t.Run(category, func(t *testing.T) {
			if _, ok := vacuumAutopilotCandidate(SourceFinding{Category: category},
				testClassify); !ok {
				t.Fatalf("the switch names %q but vacuumAutopilotCandidate does not map it",
					category)
			}
			if !producedBy(t, registry, category) {
				t.Fatalf("category %q has no producer: no analyzer rule, tuning agent or "+
					"known producer emits it; delete its branch or register its producer",
					category)
			}
		})
	}
	for _, p := range outOfRegistryProducers {
		if !handled[p.category] || registry[p.category] {
			t.Errorf("stale producer entry %q: handled=%v, in the registry=%v",
				p.category, handled[p.category], registry[p.category])
		}
	}
}

// The guard fails closed: a category that nothing emits is not produced.
func TestVacuumAutopilotGuardRejectsAnUnproducedCategory(t *testing.T) {
	const ghost = "no_such_vacuum_category"
	registry := registryCategories()
	if len(registry) == 0 || !registry["table_bloat"] || !registry["vacuum_tuning"] {
		t.Fatalf("registry = %v, want the analyzer and tuning categories", registry)
	}
	if producedBy(t, registry, ghost) {
		t.Fatalf("%q reads as produced", ghost)
	}
	if producedIn(t, "analyzer", regexp.QuoteMeta(strconv.Quote(ghost))) {
		t.Fatalf("the analyzer source scan matched %q", ghost)
	}
	if !producedIn(t, "analyzer", `Category:\s+"xid_wraparound"`) {
		t.Fatal("the analyzer source scan missed the xid_wraparound producer")
	}
}

func registryCategories() map[string]bool {
	produced := map[string]bool{}
	for _, rule := range analyzer.AllRules {
		for _, category := range rule.Categories {
			produced[category] = true
		}
	}
	for _, category := range analyzer.TuningCategories() {
		produced[category] = true
	}
	return produced
}

func producedBy(t *testing.T, registry map[string]bool, category string) bool {
	t.Helper()
	if registry[category] {
		return true
	}
	for _, p := range outOfRegistryProducers {
		if p.category == category {
			return producedIn(t, p.pkg, p.pattern)
		}
	}
	return false
}

// producedIn reports whether a non-test source file of the internal
// package pkg matches pattern.
func producedIn(t *testing.T, pkg, pattern string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", filepath.FromSlash(pkg), "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list the producer package %s: %v (%d files)", pkg, err, len(files))
	}
	re := regexp.MustCompile(pattern)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read producer source %s: %v", file, err)
		}
		if re.Match(src) {
			return true
		}
	}
	return false
}

// vacuumAutopilotCategories returns the string cases of the switch in
// vacuumAutopilotCandidate, sorted.
func vacuumAutopilotCategories(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "vacuum_autopilot.go", nil, 0)
	if err != nil {
		t.Fatalf("parse vacuum_autopilot.go: %v", err)
	}
	var categories []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "vacuumAutopilotCandidate" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("case %T in vacuumAutopilotCandidate is not a string literal", expr)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote case %s: %v", lit.Value, err)
				}
				categories = append(categories, value)
			}
			return true
		})
	}
	if len(categories) == 0 {
		t.Fatal("found no case categories in vacuumAutopilotCandidate")
	}
	sort.Strings(categories)
	return categories
}

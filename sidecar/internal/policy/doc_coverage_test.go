package policy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docs/policy-gate.md is the operator's reference for the gate's verdicts
// and reasons (policy-09). Every Verdict and Reason constant the package
// declares must appear there in backticks, so a new reason cannot ship
// without its row. The constants are read with go/parser, not a hand list.
//
// No concurrent access tests: the collector and the gap check are pure
// functions over files read once.

const (
	policyGateDocPath    = "../../../docs/policy-gate.md"
	configurationDocPath = "../../../docs/configuration.md"
	// The package declares 38 reasons and 5 verdicts today (the plan
	// counted 36 reasons; two were added since). A drop below these means
	// the collector lost constants, not that the gate has fewer.
	minDocumentedReasons  = 38
	minDocumentedVerdicts = 5
)

// policyConsts are the string values of the package's Reason and Verdict
// constants, sorted.
type policyConsts struct {
	Reasons  []string
	Verdicts []string
}

// collectPolicyConsts parses the non-test Go files of dir and returns the
// values of every constant of type Reason or Verdict: grouped or single
// specs, typed (`X Reason = "x"`) or converted (`X = Reason("x")`). A
// typed constant whose value is not a string literal is an error, so a
// constant the collector cannot read is never silently skipped.
func collectPolicyConsts(dir string) (policyConsts, error) {
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return policyConsts{}, fmt.Errorf("list Go files in %s: %w", dir, err)
	}
	var out policyConsts
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return policyConsts{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if err := collectFileConsts(fset, file, &out); err != nil {
			return policyConsts{}, err
		}
	}
	sort.Strings(out.Reasons)
	sort.Strings(out.Verdicts)
	return out, nil
}

func collectFileConsts(fset *token.FileSet, file *ast.File, out *policyConsts) error {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		var previous *ast.ValueSpec
		for _, raw := range gen.Specs {
			spec := raw.(*ast.ValueSpec)
			if spec.Type == nil && len(spec.Values) == 0 && previous != nil {
				// Implicit repetition: the spec repeats the previous
				// spec's type and values, as the compiler reads it.
				spec = &ast.ValueSpec{Names: spec.Names, Type: previous.Type,
					Values: previous.Values}
			}
			if err := collectSpec(fset, spec, out); err != nil {
				return err
			}
			previous = spec
		}
	}
	return nil
}

func collectSpec(fset *token.FileSet, spec *ast.ValueSpec, out *policyConsts) error {
	declared := identName(spec.Type)
	for i, name := range spec.Names {
		if i >= len(spec.Values) {
			if declared == "Reason" || declared == "Verdict" {
				return fmt.Errorf("%s: %s %s has no value", fset.Position(name.Pos()),
					declared, name.Name)
			}
			continue
		}
		kind, value, ok := constValue(declared, spec.Values[i])
		if kind == "" {
			continue
		}
		if !ok {
			return fmt.Errorf("%s: %s %s is not a string literal", fset.Position(name.Pos()),
				kind, name.Name)
		}
		if kind == "Reason" {
			out.Reasons = append(out.Reasons, value)
		} else {
			out.Verdicts = append(out.Verdicts, value)
		}
	}
	return nil
}

// constValue returns the constant's kind (Reason, Verdict or "" for any
// other type) and its string value.
func constValue(declared string, expr ast.Expr) (string, string, bool) {
	if call, ok := expr.(*ast.CallExpr); ok && declared == "" && len(call.Args) == 1 {
		if kind := identName(call.Fun); kind == "Reason" || kind == "Verdict" {
			value, ok := stringLiteral(call.Args[0])
			return kind, value, ok
		}
	}
	if declared != "Reason" && declared != "Verdict" {
		return "", "", false
	}
	value, ok := stringLiteral(expr)
	return declared, value, ok
}

// identName returns a type's name, local (Reason) or qualified
// (agentguard.Reason, as the agent decider declares its reasons).
func identName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

// undocumented returns the values doc does not name in backticks.
func undocumented(doc string, values []string) []string {
	var missing []string
	for _, value := range values {
		if !strings.Contains(doc, "`"+value+"`") {
			missing = append(missing, value)
		}
	}
	return missing
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestDocCoveragePolicyGateNamesEveryVerdictAndReason(t *testing.T) {
	consts, err := collectPolicyConsts(".")
	if err != nil {
		t.Fatalf("collect policy constants: %v", err)
	}
	t.Logf("policy package declares %d reasons and %d verdicts",
		len(consts.Reasons), len(consts.Verdicts))
	if len(consts.Reasons) < minDocumentedReasons {
		t.Fatalf("collected %d Reason constants, want at least %d: %v",
			len(consts.Reasons), minDocumentedReasons, consts.Reasons)
	}
	if len(consts.Verdicts) < minDocumentedVerdicts {
		t.Fatalf("collected %d Verdict constants, want at least %d: %v",
			len(consts.Verdicts), minDocumentedVerdicts, consts.Verdicts)
	}
	doc := readDoc(t, policyGateDocPath)
	for _, reason := range undocumented(doc, consts.Reasons) {
		t.Errorf("docs/policy-gate.md does not name reason `%s`", reason)
	}
	for _, verdict := range undocumented(doc, consts.Verdicts) {
		t.Errorf("docs/policy-gate.md does not name verdict `%s`", verdict)
	}
}

// The collector must see both declaration forms the package uses: grouped
// specs (types.go, autonomy.go) and single-line consts (gate_rollback.go,
// verification_wait.go), and exactly the five verdicts.
func TestDocCoverageCollectorSeesEveryDeclarationForm(t *testing.T) {
	consts, err := collectPolicyConsts(".")
	if err != nil {
		t.Fatalf("collect policy constants: %v", err)
	}
	wantVerdicts := []string{"blocked", "execute", "observe_only", "park", "queue_approval"}
	if !reflect.DeepEqual(consts.Verdicts, wantVerdicts) {
		t.Fatalf("verdicts = %v, want %v", consts.Verdicts, wantVerdicts)
	}
	for _, want := range []string{
		string(ReasonNarrowingDuringStop), string(ReasonDropRestore),
		string(ReasonRollbackMitigation), string(ReasonAwaitingVerification),
		string(ReasonAgentL3Envelope), string(ReasonAutonomyUnavailable),
		string(ReasonFactsUnavailable), string(ReasonDDLConflict),
	} {
		if !containsString(consts.Reasons, want) {
			t.Errorf("collector missed reason %q (got %v)", want, consts.Reasons)
		}
	}
	if dup := firstDuplicate(consts.Reasons); dup != "" {
		t.Errorf("reason %q is declared twice; the ledger cannot tell them apart", dup)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func firstDuplicate(sorted []string) string {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return sorted[i]
		}
	}
	return ""
}

package receipt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/verify"
)

// CHECK-RCP-08: the shadow auto-clear flag is evidence for a later
// promotion, never an input. The verdict (merge), the CI status (the CLI's
// GitHub mode), the receipt decision, landing and every credit
// computation must not read it. This scans their source for the shadow
// fields and columns, and checks the decisions behave the same whatever
// the flag says.

// shadowFree are the files that must never read shadow fields, relative to
// this package. The globs pick up the CLI's review and GitHub status code
// (the surfaces workstream) where it exists; a missing glob match is fine,
// a missing named file is not. The runtime wiring that computes the flag
// at completion (Deps.AutoClear) is deliberately not listed.
var shadowFree = []string{"../credit.go", "../merge.go", "decide.go", "landing.go",
	"observe.go", "../../value/review_credit.go",
	"../../../cmd/pg_sage_sidecar/change_review_receipts.go"}

var shadowFreeGlobs = []string{"../../../cmd/pg_sage_sidecar/change_review_github*.go",
	"../../../cmd/pg_sage_sidecar/change_review_gate.go",
	"../../../cmd/pg_sage_sidecar/change_review_run.go",
	"../../../cmd/pg_sage_sidecar/change_review_flow.go"}

var shadowNames = map[string]bool{"ShadowAutoClear": true, "ShadowScore": true,
	"SetShadowScore": true, "Score": true, "Eligible": true, "AutoClear": true}

func TestVerdictCreditAndCIStatusNeverReadShadowFields(t *testing.T) {
	files := append([]string{}, shadowFree...)
	for _, g := range shadowFreeGlobs {
		matches, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files = append(files, m)
			}
		}
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if shadowNames[x.Sel.Name] {
					t.Errorf("%s reads %s", fset.Position(x.Pos()), x.Sel.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && strings.Contains(x.Value, "shadow_") {
					t.Errorf("%s names a shadow column: %s", fset.Position(x.Pos()), x.Value)
				}
			}
			return true
		})
	}
}

func TestCreditFactsCarryNoShadowField(t *testing.T) {
	ty := reflect.TypeOf(changereview.CreditFacts{})
	for i := 0; i < ty.NumField(); i++ {
		if strings.Contains(strings.ToLower(ty.Field(i).Name), "shadow") {
			t.Fatalf("CreditFacts.%s lets credit depend on the shadow flag", ty.Field(i).Name)
		}
	}
}

func TestDecideIgnoresTheShadowFlag(t *testing.T) {
	obs := Observations{Before: map[int64]verify.Measurement{100: meas(500, 10, 0.1, 20)},
		After: map[int64]verify.Measurement{100: meas(500, 18, 0.1, 20)}}
	plain := decidedReview(100)
	flagged := decidedReview(100)
	flagged.ShadowAutoClear, flagged.ShadowScore = ptr(true), changereview.ShadowCorrect
	a, b := Decide(plain, windows, obs), Decide(flagged, windows, obs)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the shadow flag changed the receipt:\n%+v\n%+v", a, b)
	}
}

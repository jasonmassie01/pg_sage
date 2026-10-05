package cloudtel

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Never-widen (structural): host telemetry supplies evidence only. The
// telemetry and managed-proposal packages must not reach the trust ledger,
// the policy gate, earned autonomy or the executor, so no telemetry value
// can raise a trust level or grant an action by itself.
var trustPackages = []string{
	"internal/policy", "internal/earned", "internal/autonomy", "internal/ledger",
	"internal/executor", "internal/shadow", "internal/rollout", "internal/store",
}

func TestTelemetryPackagesCannotReachTrust(t *testing.T) {
	for _, dir := range []string{".", "../managedparam"} {
		for _, imp := range productionImports(t, dir) {
			for _, banned := range trustPackages {
				if strings.HasSuffix(imp, "/"+banned) || strings.Contains(imp, "/"+banned+"/") {
					t.Errorf("%s imports %s: telemetry must never reach trust/policy code",
						dir, imp)
				}
			}
		}
	}
}

func productionImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			out = append(out, path)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no production files", dir)
	}
	return out
}

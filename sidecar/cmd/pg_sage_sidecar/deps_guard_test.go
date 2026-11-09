package main

import (
	"bufio"
	osexec "os/exec"
	"sort"
	"strings"
	"testing"
)

// benches-11: test scaffolding stays out of the production binary. No
// pg_sage package the binary links imports internal/testdb (which pulls
// in docker locks and testing), and none imports "testing" except the
// allowlisted ones.

const sidecarModule = "github.com/pg-sage/sidecar/"

// testingAllowed are the linked pg_sage packages allowed to import
// "testing": internal/auth reads testing.Testing() to refuse a cheap
// bcrypt cost outside a test binary (hashcost.go).
var testingAllowed = map[string]bool{sidecarModule + "internal/auth": true}

// listedPackage is one package of go list -deps and its imports.
type listedPackage struct {
	path    string
	imports []string
}

// parseDepsList reads go list lines of "<path> <import> <import>...".
func parseDepsList(out string) []listedPackage {
	var pkgs []listedPackage
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		pkgs = append(pkgs, listedPackage{path: fields[0], imports: fields[1:]})
	}
	return pkgs
}

// scaffoldingViolations are the linked pg_sage packages that import
// internal/testdb, or "testing" without being allowed to, and the allowed
// ones that no longer import it (a stale allowlist entry).
func scaffoldingViolations(pkgs []listedPackage, allowed map[string]bool) []string {
	var out []string
	stillTesting := map[string]bool{}
	for _, p := range pkgs {
		if !strings.HasPrefix(p.path, sidecarModule) {
			continue
		}
		if p.path == sidecarModule+"internal/testdb" {
			out = append(out, p.path+" is linked into the binary")
		}
		for _, imp := range p.imports {
			switch {
			case imp == sidecarModule+"internal/testdb":
				out = append(out, p.path+" imports internal/testdb")
			case imp == "testing" && allowed[p.path]:
				stillTesting[p.path] = true
			case imp == "testing":
				out = append(out, p.path+` imports "testing"`)
			}
		}
	}
	for path := range allowed {
		if !stillTesting[path] {
			out = append(out, path+` is allowlisted but does not import "testing" `+
				"in the binary: remove it from testingAllowed")
		}
	}
	sort.Strings(out)
	return out
}

func TestDepsBinaryLinksNoTestScaffolding(t *testing.T) {
	cmd := osexec.Command("go", "list", "-deps",
		"-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", ".")
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*osexec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps of the sidecar binary: %v\n%s", err, stderr)
	}
	pkgs := parseDepsList(string(out))
	linked := map[string]bool{}
	for _, p := range pkgs {
		linked[p.path] = true
	}
	for _, want := range []string{sidecarModule + "sre-bench",
		sidecarModule + "internal/auth", sidecarModule + "cmd/pg_sage_sidecar"} {
		if !linked[want] {
			t.Fatalf("%s is not in the binary's graph (%d packages): the guard covers "+
				"nothing", want, len(pkgs))
		}
	}
	for _, v := range scaffoldingViolations(pkgs, testingAllowed) {
		t.Error(v)
	}
}

func TestDepsScaffoldingViolations(t *testing.T) {
	m := sidecarModule
	pkgs := parseDepsList(strings.Join([]string{
		"fmt errors",
		m + "internal/auth testing golang.org/x/crypto/bcrypt",
		m + "sre-bench context testing " + m + "internal/sre",
		m + "internal/gameday " + m + "internal/testdb",
		m + "internal/testdb testing",
		"github.com/other/lib testing",
		"",
	}, "\n"))
	if len(pkgs) != 6 || pkgs[1].path != m+"internal/auth" || len(pkgs[1].imports) != 2 {
		t.Fatalf("parsed = %+v", pkgs)
	}
	got := scaffoldingViolations(pkgs, map[string]bool{m + "internal/auth": true,
		m + "internal/stale": true})
	want := []string{
		m + `internal/gameday imports internal/testdb`,
		m + `internal/stale is allowlisted but does not import "testing" in the binary: ` +
			"remove it from testingAllowed",
		m + `internal/testdb imports "testing"`,
		m + `internal/testdb is linked into the binary`,
		m + `sre-bench imports "testing"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations =\n%s\nwant\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
	if v := scaffoldingViolations(nil, nil); len(v) != 0 {
		t.Fatalf("no packages = %v, want none", v)
	}
	if v := scaffoldingViolations(parseDepsList(m+"internal/auth testing"),
		testingAllowed); len(v) != 0 {
		t.Fatalf("only the allowlisted import = %v, want none", v)
	}
}

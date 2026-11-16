package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// observability-04 contract: every statement that writes an action_log
// outcome sits in a function that counts it for /metrics, or is listed
// here with why it does not count. A new write site that is neither fails.

// outcomeRecorders are the calls that count an outcome.
var outcomeRecorders = map[string]bool{
	"RecordOutcome": true, "RecordPoolOutcome": true, "countActionOutcome": true,
}

// outcomeSite is an exempt function: a read, a non-terminal write, or a
// write inside a transaction that countedBy counts after the commit.
type outcomeSite struct {
	reason    string
	countedBy string // function in the same directory that must count
}

// outcomeSiteExemptions are keyed by directory (relative to internal) and
// function name.
var outcomeSiteExemptions = map[string]outcomeSite{
	"executor.exceedsOscillationLimit": {reason: "read: counts past outcomes"},
	"executor.exceedsMaxRetries":       {reason: "read: counts past failures"},
	"executor.readCustodianFailures":   {reason: "read: custodian backoff"},
	"executor.loadDropRecord":          {reason: "read: the drop's row"},
	"executor.loadRollbackSQL":         {reason: "read: rollback statement"},
	"executor.claimRollback":           {reason: "non-terminal: sets rolling_back"},
	"executor.loadMonitorPlan":         {reason: "read: monitor state"},
	"executor.recordStart":             {reason: "non-terminal: inserts pending"},
	"executor.revertState":             {reason: "read: revert card state"},
	"executor.finishRegressed":         {reason: "read: the rollback's outcome"},
	"executor.CheckHysteresis":         {reason: "read: recent rollbacks"},
	"executor.resumeOrphanedMonitors":  {reason: "read: monitors to resume"},
	"executor.parkedWithhold":          {reason: "read: withheld park"},
	"executor.readLastFailure":         {reason: "read: the last-failure gauge"},
	"executor.inFlightSQL":             {reason: "read: changes in flight"},
	"executor.exposureRefusedBefore":   {reason: "read: a recorded ownership refusal"},
	"executor.supersedeSettledRestoresSQL": {
		reason: "read: writes sage.action_queue, reading the outcome"},
	"executor.supersedeSettledRevertsSQL": {
		reason: "read: writes sage.action_queue, reading the outcome"},
	"agentguard/upkeep.lastEnsureSQL": {reason: "read: the last role ensure"},
	"executor.settleStuckRow": {reason: "commits inside a reconcile transaction",
		countedBy: "reconcileStuckRollback"},
	"agentguard/grants.recordAction": {reason: "inside the contract's transaction",
		countedBy: "execute"},
}

// outcomeWriteSite is one literal naming sage.action_log and outcome.
type outcomeWriteSite struct {
	key, pos string
	counts   bool
}

// scanOutcomeSites parses every non-test Go file under dir (relative to
// internal: its key prefix) and returns the action_log outcome literals
// with whether their enclosing function counts, and which functions of
// each directory count.
func scanOutcomeSites(t *testing.T, root, dir string) ([]outcomeWriteSite, map[string]bool) {
	t.Helper()
	var sites []outcomeWriteSite
	counting := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry,
		err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") ||
			strings.HasSuffix(p, "_test.go") {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		found, counts := outcomeSitesIn(t, p, src, filepath.ToSlash(rel))
		sites = append(sites, found...)
		for k := range counts {
			counting[k] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	return sites, counting
}

// outcomeSitesIn finds the sites of one file; pkg is its directory key.
func outcomeSitesIn(t *testing.T, name string, src []byte, pkg string) (
	[]outcomeWriteSite, map[string]bool) {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var sites []outcomeWriteSite
	counting := map[string]bool{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			sites = append(sites, packageLevelSites(fs, decl, pkg)...)
			continue
		}
		key := pkg + "." + fd.Name.Name
		counts := callsRecorder(fd)
		if counts {
			counting[key] = true
		}
		for _, lit := range outcomeLiterals(fd) {
			sites = append(sites, outcomeWriteSite{key: key,
				pos: fs.Position(lit.Pos()).String(), counts: counts})
		}
	}
	return sites, counting
}

// packageLevelSites are the sites of a package-level declaration: they
// belong to no function, so each is keyed by its name and must be listed.
func packageLevelSites(fs *token.FileSet, decl ast.Decl, pkg string) []outcomeWriteSite {
	gd, ok := decl.(*ast.GenDecl)
	if !ok {
		return nil
	}
	var sites []outcomeWriteSite
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, lit := range outcomeLiterals(vs) {
			sites = append(sites, outcomeWriteSite{key: pkg + "." + vs.Names[0].Name,
				pos: fs.Position(lit.Pos()).String()})
		}
	}
	return sites
}

// outcomeLiterals are the string literals under n naming sage.action_log
// and outcome.
func outcomeLiterals(n ast.Node) []*ast.BasicLit {
	var out []*ast.BasicLit
	ast.Inspect(n, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
			strings.Contains(lit.Value, "sage.action_log") &&
			strings.Contains(lit.Value, "outcome") {
			out = append(out, lit)
		}
		return true
	})
	return out
}

// callsRecorder reports whether fd calls an outcome recorder.
func callsRecorder(fd *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			found = found || outcomeRecorders[fn.Name]
		case *ast.SelectorExpr:
			found = found || outcomeRecorders[fn.Sel.Name]
		}
		return !found
	})
	return found
}

// uncountedOutcomeSites returns the problems: sites that neither count nor
// are exempt, exemptions whose countedBy does not count, and exemptions
// that no longer match a site.
func uncountedOutcomeSites(sites []outcomeWriteSite, counting map[string]bool,
	exempt map[string]outcomeSite) []string {
	var problems []string
	seen := map[string]bool{}
	for _, s := range sites {
		seen[s.key] = true
		ex, listed := exempt[s.key]
		switch {
		case s.counts:
		case !listed:
			problems = append(problems, s.pos+": "+s.key+" writes an action_log outcome "+
				"but does not call RecordOutcome; count it after the commit or list it")
		case ex.countedBy != "":
			dir := s.key[:strings.LastIndexByte(s.key, '.')]
			if !counting[dir+"."+ex.countedBy] {
				problems = append(problems, s.key+": its counter "+ex.countedBy+
					" does not call RecordOutcome")
			}
		}
	}
	for k := range exempt {
		if !seen[k] {
			problems = append(problems, k+": exempt but no longer an outcome site")
		}
	}
	sort.Strings(problems)
	return problems
}

func TestOutcomeSites_EveryActionLogOutcomeWriteCounts(t *testing.T) {
	var sites []outcomeWriteSite
	counting := map[string]bool{}
	for _, dir := range []string{"executor", "agentguard"} {
		found, counts := scanOutcomeSites(t, "..", dir)
		sites = append(sites, found...)
		for k := range counts {
			counting[k] = true
		}
	}
	if len(sites) < 20 {
		t.Fatalf("found %d outcome sites; the scan is broken", len(sites))
	}
	for _, p := range uncountedOutcomeSites(sites, counting, outcomeSiteExemptions) {
		t.Error(p)
	}
}

// CHECK-21: an added, unlisted write site fails the contract.
func TestOutcomeSites_AnUnlistedWriteFails(t *testing.T) {
	src := []byte(`package executor
func dummyWrite() {
	_ = "INSERT INTO sage.action_log (action_type, outcome) VALUES ('x', 'failed')"
}
func countedWrite() {
	_ = "UPDATE sage.action_log SET outcome = 'failed'"
	RecordOutcome("db", "index", "failed")
}
`)
	sites, counting := outcomeSitesIn(t, "dummy.go", src, "executor")
	problems := uncountedOutcomeSites(sites, counting, nil)
	if len(problems) != 1 || !strings.Contains(problems[0], "executor.dummyWrite") {
		t.Fatalf("problems = %v, want only the unlisted dummyWrite", problems)
	}
}

// An exemption counted by a function that does not count fails too.
func TestOutcomeSites_ACountedByThatDoesNotCountFails(t *testing.T) {
	src := []byte(`package executor
func insideTx() { _ = "UPDATE sage.action_log SET outcome = 'failed'" }
func committer() { insideTx() }
`)
	sites, counting := outcomeSitesIn(t, "dummy.go", src, "executor")
	problems := uncountedOutcomeSites(sites, counting, map[string]outcomeSite{
		"executor.insideTx": {reason: "tx", countedBy: "committer"}})
	if len(problems) != 1 || !strings.Contains(problems[0], "committer") {
		t.Fatalf("problems = %v, want the non-counting committer named", problems)
	}
}

// A package-level statement is a site of its own and must be listed.
func TestOutcomeSites_APackageLevelWriteFails(t *testing.T) {
	src := []byte("package executor\nconst w = `UPDATE sage.action_log SET outcome = 'x'`\n")
	sites, counting := outcomeSitesIn(t, "dummy.go", src, "executor")
	problems := uncountedOutcomeSites(sites, counting, nil)
	if len(problems) != 1 || !strings.Contains(problems[0], "executor.w ") {
		t.Fatalf("problems = %v, want the package-level write", problems)
	}
}

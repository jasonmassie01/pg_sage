package agentguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// AGENTDB-SPEC §6.2.5: the closed list of out-of-gate paths. This census
// scans pg_sage's production Go source (internal/ and cmd/) for code that
// changes agent access: a SQL literal that alters, grants, revokes or
// terminates, in a file that names agent roles (the agentguard packages,
// the agent role names or their pattern). Every such file must be listed
// in agentAccessCensus as running under a typed gate contract, as text a
// person reads, or as one of the four closed-list paths. A new file fails
// the test until someone classifies it, which is the point.

// agentAccessCensus classifies every file the scan finds:
//   - "gate:<contracts>": runs only inside Executor.Apply for those
//     contracts;
//   - "text": the SQL is a fix or message shown to a person, never run;
//   - "out_of_gate:<path>": one of OutOfGatePaths.
var agentAccessCensus = map[string]string{
	"internal/agentguard/roles.go":              "gate:guard_role_ensure,guard_role_retire",
	"internal/agentguard/roles_ensure.go":       "gate:guard_role_ensure",
	"internal/agentguard/roles_retire.go":       "gate:guard_role_retire",
	"internal/executor/guard_role_contract.go":  "gate:guard_role_ensure,guard_role_retire",
	"internal/agentguard/envbind/evaluate.go":   "text",
	"internal/agentguard/grants/grant.go":       "gate:guard_grant",
	"internal/agentguard/grants/revoke.go":      "gate:guard_revoke",
	"internal/agentguard/grants/revoke_rows.go": "gate:guard_revoke",
	"internal/agentguard/grants/objects.go":     "text",
	"internal/executor/guard_grant_contract.go": "gate:guard_grant,guard_revoke",
	// The P1 fix (REVOKE CREATE ... FROM PUBLIC) is shown to an owner, never run.
	"internal/agentguard/preflight.go": "text",
	// Retire's REVOKE fixes are shown to the other grantor, never run.
	"internal/agentguard/residue.go": "text",
	// The kill steps run under guard_kill / guard_freeze through Executor.Apply,
	// and the same functions are the direct fallback when the gate or the
	// control database is unreachable; the stricter class is recorded.
	"internal/agentguard/kill_cluster.go":   "out_of_gate:kill_direct_fallback",
	"internal/agentguard/kill_report.go":    "text",
	"internal/agentguard/unfreeze_roles.go": "gate:guard_unfreeze",
}

// closedListAnchors are where each closed-list path lives; the anchor must
// exist (a path may not silently disappear) and must mention its marker.
var closedListAnchors = map[OutOfGatePath]struct{ file, marker string }{
	OutOfGateKillFallback:    {"internal/agentguard/kill_run.go", "appendFallback"},
	OutOfGateManualRunbook:   {"../docs/agent-guard.md", "NOLOGIN CONNECTION LIMIT 0"},
	OutOfGateBreakGlass:      {"internal/auth/break_glass.go", "break"},
	OutOfGateDecommissionAck: {"internal/decommission/ack.go", "agentdb_decommission"},
}

var (
	accessSQL = regexp.MustCompile(`\b(ALTER ROLE|DROP ROLE|CREATE ROLE|DROP OWNED|` +
		`REASSIGN OWNED|GRANT|REVOKE|NOLOGIN|pg_terminate_backend|pg_cancel_backend)\b`)
	agentRoleText  = regexp.MustCompile(`sage_agentb?_`)
	agentRoleIdent = map[string]bool{"RoleRegex": true, "RolePattern": true,
		"LoginRoleName": true, "BrokerRoleName": true, "LoginRole": true, "BrokerRole": true}
)

func sidecarRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("sidecar root %s has no go.mod: %v", root, err)
	}
	return root
}

// scanFile reports whether a Go file holds an access-changing SQL literal
// and whether it names agent roles.
func scanFile(t *testing.T, path, rel string) (sql, agent bool) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	agent = strings.HasPrefix(rel, "internal/agentguard/")
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(node.Value)
			if err != nil {
				text = node.Value
			}
			sql = sql || accessSQL.MatchString(text)
			agent = agent || agentRoleText.MatchString(text)
		case *ast.Ident:
			agent = agent || agentRoleIdent[node.Name]
		}
		return true
	})
	return sql, agent
}

func agentAccessFiles(t *testing.T) []string {
	t.Helper()
	root := sidecarRoot(t)
	var found []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string,
			d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/testsupport/") {
				return nil
			}
			if sql, agent := scanFile(t, path, rel); sql && agent {
				found = append(found, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	sort.Strings(found)
	return found
}

func TestOutOfGateClosedListIsTheSpecs(t *testing.T) {
	want := []OutOfGatePath{"kill_direct_fallback", "manual_sql_runbook",
		"break_glass_login", "decommission_acknowledgement"}
	got := OutOfGatePaths()
	if len(got) != len(want) {
		t.Fatalf("closed list = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] || !IsOutOfGatePath(want[i]) {
			t.Fatalf("closed list = %v, want %v", got, want)
		}
	}
	for _, bad := range []OutOfGatePath{"", "watchdog", "trash_purge", "KILL_DIRECT_FALLBACK"} {
		if IsOutOfGatePath(bad) {
			t.Fatalf("%q must not be on the closed list (§6.2.5: in the gate)", bad)
		}
	}
}

func TestEveryAgentAccessPathIsClassified(t *testing.T) {
	found := agentAccessFiles(t)
	if len(found) == 0 {
		t.Fatal("the census found nothing: the scan is broken (the role contracts exist)")
	}
	seen := map[string]bool{}
	for _, rel := range found {
		seen[rel] = true
		class, ok := agentAccessCensus[rel]
		if !ok {
			t.Errorf("%s changes agent access outside the census: route it through a "+
				"typed contract (Executor.Apply) or, if it is one of the closed-list "+
				"paths of §6.2.5, register it as out_of_gate:<path>", rel)
			continue
		}
		checkCensusClass(t, rel, class)
	}
	for rel := range agentAccessCensus {
		if !seen[rel] {
			t.Errorf("census lists %s, which no longer changes agent access: remove it", rel)
		}
	}
}

func checkCensusClass(t *testing.T, rel, class string) {
	t.Helper()
	kind, detail, _ := strings.Cut(class, ":")
	switch {
	case kind == "gate" && detail != "":
	case kind == "text" && detail == "":
	case kind == "out_of_gate" && IsOutOfGatePath(OutOfGatePath(detail)):
	default:
		t.Errorf("%s: census class %q is not gate:<contracts>, text or "+
			"out_of_gate:<closed-list path>", rel, class)
	}
}

func TestClosedListAnchorsExist(t *testing.T) {
	root := sidecarRoot(t)
	for path, anchor := range closedListAnchors {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(anchor.file)))
		if err != nil {
			t.Fatalf("%s: anchor %s: %v", path, anchor.file, err)
		}
		if !strings.Contains(strings.ToLower(string(raw)), strings.ToLower(anchor.marker)) {
			t.Fatalf("%s: %s no longer mentions %q", path, anchor.file, anchor.marker)
		}
	}
}

func TestCensusScanDetectsAgentAccessSQL(t *testing.T) {
	// The scanner itself: an agent role name next to a REVOKE is found, a
	// REVOKE alone or a role name alone is not, and comments never count.
	dir := t.TempDir()
	cases := map[string]struct {
		src        string
		sql, agent bool
	}{
		"both": {"package x\nvar q = \"REVOKE ALL ON t FROM \" + RoleRegex\n", true, true},
		"literal role": {"package x\nvar q = `ALTER ROLE sage_agentb_abc NOLOGIN`\n",
			true, true},
		"sql only":  {"package x\nvar q = \"GRANT pg_monitor TO pg_sage\"\n", true, false},
		"role only": {"package x\nvar q = \"sage_agent_abcdefghij\"\n", false, true},
		"comment": {"package x\n// ALTER ROLE sage_agentb_x NOLOGIN\nvar q = 1\n",
			false, false},
	}
	for name, tc := range cases {
		path := filepath.Join(dir, name+".go")
		if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
			t.Fatal(err)
		}
		sql, agent := scanFile(t, path, "cmd/x/"+name+".go")
		if sql != tc.sql || agent != tc.agent {
			t.Fatalf("%s: scan = (%v, %v), want (%v, %v)", name, sql, agent, tc.sql,
				tc.agent)
		}
	}
}

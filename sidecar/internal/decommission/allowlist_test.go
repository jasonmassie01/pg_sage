package decommission

import (
	"errors"
	"os/exec"
	"path"
	"reflect"
	"strings"
	"testing"
)

// legacyNamePattern is the G0-02 grep: after the removal, tracked files that
// still name the removed feature or its token prefix must be on the allowlist.
const legacyNamePattern = `agent[-_]?db|agt_`

// legacyPinnedFiles are files outside the package that must spell a legacy
// name: the router table and the per-route database scopes pin every
// registered route, this package's /api/v1/agentdb routes included
// (apiauth-09, agentgov-11, apiauth-08), and the v2.4.0 and v2.5.0 schema
// dumps still hold sage.agentdb_decommission (config-04, plan-01a).
var legacyPinnedFiles = map[string]bool{
	"sidecar/internal/api/router_table_probes_test.go":      true,
	"sidecar/internal/api/testdata/router_table.golden":     true,
	"sidecar/internal/api/testdata/route_scopes.golden":     true,
	"sidecar/internal/schema/testdata/v2.4.0_sage_dump.sql": true,
	"sidecar/internal/schema/testdata/v2.5.0_sage_dump.sql": true,
}

// allowedLegacyReference reports whether a tracked path may still name the
// removed feature: this package, schema migrations, the changelog, the
// review archive (AGENTDB-SPEC §10.1 G0-02) and legacyPinnedFiles.
func allowedLegacyReference(file string) bool {
	switch {
	case legacyPinnedFiles[file]:
		return true
	case strings.HasPrefix(file, "sidecar/internal/decommission/"),
		strings.HasPrefix(file, "reviews/"),
		strings.EqualFold(path.Base(file), "CHANGELOG.md"):
		return true
	case strings.HasPrefix(file, "sidecar/internal/schema/"):
		base := strings.TrimSuffix(strings.TrimSuffix(path.Base(file), ".go"), "_test")
		return strings.HasSuffix(base, "_migration")
	}
	return false
}

func TestAllowedLegacyReference(t *testing.T) {
	for file, want := range map[string]bool{
		"sidecar/internal/decommission/inventory.go":                     true,
		"sidecar/internal/schema/decommission_migration.go":              true,
		"sidecar/internal/schema/decommission_migration_test.go":         true,
		"reviews/archive/x.md":                                           true,
		"CHANGELOG.md":                                                   true,
		"sidecar/internal/schema/bootstrap.go":                           false,
		"sidecar/internal/api/router.go":                                 false,
		"docs/runbook.md":                                                false,
		"sidecar/internal/decommissionx/a.go":                            false,
		"sidecar/internal/schema/migration_helpers.go":                   false,
		"sidecar/web/src/pages/x.jsx":                                    false,
		"sidecar/internal/schema/lint/decommission_migration_fixture.go": false,
		"sidecar/internal/api/testdata/router_table.golden":              true,
		"sidecar/internal/api/router_table_probes_test.go":               true,
		"sidecar/internal/api/router_table_test.go":                      false,
		"sidecar/internal/schema/testdata/v2.4.0_sage_dump.sql":          true,
		"sidecar/internal/schema/testdata/other_dump.sql":                false,
	} {
		if got := allowedLegacyReference(file); got != want {
			t.Errorf("allowedLegacyReference(%q) = %v, want %v", file, got, want)
		}
	}
}

// G0-02: `git grep -ilE 'agent[-_]?db|agt_'` matches only the allowlist.
func TestNoLegacyReferencesOutsideTheAllowlist(t *testing.T) {
	top, err := exec.Command("git", "-c", "safe.directory=*", "rev-parse",
		"--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a readable git work tree (the check runs in CI and on the host): %v", err)
	}
	root := strings.TrimSpace(string(top))
	cmd := exec.Command("git", "-c", "safe.directory=*", "-C", root, "grep", "-ilE",
		legacyNamePattern)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
		t.Fatalf("git grep: %v", err)
	}
	var offending []string
	for _, file := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if file != "" && !allowedLegacyReference(file) {
			offending = append(offending, file)
		}
	}
	if !reflect.DeepEqual(offending, []string(nil)) {
		t.Fatalf("%d tracked files outside the allowlist still match %q:\n%s",
			len(offending), legacyNamePattern, strings.Join(offending, "\n"))
	}
}

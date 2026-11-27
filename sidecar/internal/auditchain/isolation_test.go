package auditchain

import (
	"strings"
	"testing"
)

// No concurrent access tests: isolationWarning is a pure function.

func TestIsolationWarning_SilentForSnapshotPerStatementLevels(t *testing.T) {
	for _, level := range []string{"read committed", "read uncommitted", "READ COMMITTED"} {
		if got := isolationWarning(level, "sage", "app", "role sage = x"); got != "" {
			t.Errorf("level %q: warning %q, want none", level, got)
		}
	}
}

func TestIsolationWarning_NamesLevelRoleDatabaseSourceAndFix(t *testing.T) {
	got := isolationWarning("repeatable read", "sage", "app",
		"role sage in database app = repeatable read")
	for _, want := range []string{"'repeatable read'", `role "sage"`, `database "app"`,
		"set by role sage in database app = repeatable read", "SQLSTATE 55000",
		`ALTER ROLE "sage" IN DATABASE "app" SET default_transaction_isolation = ` +
			`'read committed'`} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q is missing %q", got, want)
		}
	}
}

// Without a role or database setting the default comes from the server
// configuration or the connection's options; the warning says so.
func TestIsolationWarning_NoStoredSourceNamesServerConfig(t *testing.T) {
	got := isolationWarning("serializable", "sage", "app", "")
	if !strings.Contains(got, "'serializable'") ||
		!strings.Contains(got, "server configuration or connection options") {
		t.Fatalf("warning %q must name serializable and the server-level source", got)
	}
}

// Identifiers are quoted in the suggested fix, so a role or database
// name with a quote or space cannot turn it into different SQL.
func TestIsolationWarning_QuotesIdentifiersInTheFix(t *testing.T) {
	got := isolationWarning("serializable", `we"ird role`, "my db", "")
	want := `ALTER ROLE "we""ird role" IN DATABASE "my db" SET`
	if !strings.Contains(got, want) {
		t.Fatalf("warning %q is missing %q", got, want)
	}
}

// Zero value: an empty level (nothing read) warns rather than staying
// silent, because the caller could not prove the default is safe.
func TestIsolationWarning_EmptyLevelWarns(t *testing.T) {
	if got := isolationWarning("", "sage", "app", ""); got == "" {
		t.Fatal("an unreadable default must warn, not pass silently")
	}
}

package advisor

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// advisors-07: idle_in_transaction_session_timeout runs only at database
// scope and only with an operator's approval; statement_timeout and
// idle_session_timeout stay advice at every scope.

// No concurrent access tests: the gates are pure functions of their
// arguments.

func timeoutFinding(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "connection_tuning", Severity: "warning",
		ObjectType: "configuration", ObjectIdentifier: "instance",
		Recommendation: "end forgotten transactions", RecommendedSQL: sql,
		ActionRisk: deriveActionRisk(sql)}
}

func gateTimeout(t *testing.T, sql, platform, db string) []analyzer.Finding {
	t.Helper()
	return GateConfigFindingsHost([]analyzer.Finding{timeoutFinding(sql)}, HostMemory{},
		platform, db, []collector.PGSetting{{Name: "idle_in_transaction_session_timeout",
			Setting: "0", Unit: "ms", Context: "user"}})
}

func TestDatabaseScope_RewritesAlterSystem(t *testing.T) {
	for _, platform := range []string{"", "rds", "cloud-sql", "azure"} {
		got := gateTimeout(t,
			"ALTER SYSTEM SET idle_in_transaction_session_timeout = '5min';", platform, "app")
		if len(got) != 1 {
			t.Fatalf("%q: findings %d, want 1", platform, len(got))
		}
		f := got[0]
		stmt, ok := pgconf.ParseAlterDatabase(f.RecommendedSQL)
		if !ok || stmt.Database != `"app"` || stmt.Name != "idle_in_transaction_session_timeout" ||
			stmt.Value != "5min" || stmt.Reset {
			t.Fatalf("%q: SQL %q, want ALTER DATABASE \"app\" SET ... '5min'", platform,
				f.RecommendedSQL)
		}
		if why, _ := f.Detail[analyzer.DetailApprovalRequired].(string); why == "" {
			t.Errorf("%q: database-scope timeout must be approval-required: %v", platform,
				f.Detail)
		}
		rb, ok := pgconf.ParseAlterDatabase(f.RollbackSQL)
		if !ok || !rb.Reset || rb.Database != `"app"` {
			t.Errorf("%q: rollback hint %q, want a database RESET", platform, f.RollbackSQL)
		}
	}
}

func TestDatabaseScope_QuotesTheDatabase(t *testing.T) {
	got := gateTimeout(t, "ALTER SYSTEM SET idle_in_transaction_session_timeout TO '10min'",
		"", `Sales "EU"`)
	stmt, ok := pgconf.ParseAlterDatabase(got[0].RecommendedSQL)
	if !ok || stmt.Database != `"Sales ""EU"""` {
		t.Errorf("SQL %q: the database must be a quoted identifier", got[0].RecommendedSQL)
	}
}

func TestDatabaseScope_OutOfRangeRefused(t *testing.T) {
	for _, v := range []string{"'0'", "0", "'10s'", "'25h'"} {
		sql := "ALTER SYSTEM SET idle_in_transaction_session_timeout = " + v
		if got := gateTimeout(t, sql, "", "app"); len(got) != 0 {
			t.Errorf("%s: %d findings, want it refused as out of range: %+v", v, len(got), got)
		}
		raw := `[{"rationale":"x","recommended_sql":"` + sql + `"}]`
		if got := parseLLMFindings(raw, "connection_tuning", noopLog); len(got) != 0 {
			t.Errorf("%s: the parser must drop it too: %+v", v, got)
		}
	}
}

func TestDatabaseScope_NoDatabaseIsAdvisory(t *testing.T) {
	got := gateTimeout(t, "ALTER SYSTEM SET idle_in_transaction_session_timeout = '5min'",
		"", " ")
	if len(got) != 1 || got[0].RecommendedSQL != "" || got[0].Severity != "info" {
		t.Errorf("no database to scope to: want advisory, got %+v", got)
	}
}

func TestDatabaseScope_OtherTimeoutsAdvisory(t *testing.T) {
	for _, sql := range []string{
		"ALTER SYSTEM SET statement_timeout = '30s'",
		"ALTER SYSTEM SET idle_session_timeout = '1h'",
	} {
		for _, platform := range []string{"", "rds"} {
			got := gateTimeout(t, sql, platform, "app")
			if len(got) != 1 || got[0].RecommendedSQL != "" {
				t.Errorf("%s on %q: want advisory only, got %+v", sql, platform, got)
			}
		}
	}
	// The model's own ALTER DATABASE is not a setting change pg_sage runs.
	got := gateTimeout(t, `ALTER DATABASE app SET statement_timeout = '30s'`, "", "app")
	if len(got) != 1 || got[0].RecommendedSQL != "" {
		t.Errorf("model-authored ALTER DATABASE: want advisory, got %+v", got)
	}
}

// A RESET of the timeout is scoped the same way.
func TestDatabaseScope_Reset(t *testing.T) {
	got := gateTimeout(t, "ALTER SYSTEM RESET idle_in_transaction_session_timeout", "", "app")
	stmt, ok := pgconf.ParseAlterDatabase(got[0].RecommendedSQL)
	if !ok || !stmt.Reset {
		t.Errorf("RESET: SQL %q", got[0].RecommendedSQL)
	}
}

// State transition: through the connection advisor the timeout is
// executable only when pg_sage's role owns the database.
func TestDatabaseScope_OwnerRequired(t *testing.T) {
	resp := `[{"object_identifier":"instance","severity":"warning","kind":"setting",` +
		`"rationale":"forgotten transactions hold locks",` +
		`"recommended_sql":"ALTER SYSTEM SET idle_in_transaction_session_timeout = '5min'"}]`
	for _, owns := range []bool{false, true} {
		mgr, _ := capturingManager(t, resp)
		snap := connectionSnapshot()
		snap.ConfigData.OwnsDatabase = owns
		ctx := withConnEnv(context.Background(), connEnv{database: "app"})
		got, err := analyzeConnections(ctx, mgr, snap, config.DefaultConfig(), noopLog)
		if err != nil || len(got) != 1 {
			t.Fatalf("owns=%v: findings %v err %v", owns, got, err)
		}
		f := got[0]
		if owns && (f.RecommendedSQL == "" || f.Detail[analyzer.DetailApprovalRequired] == nil) {
			t.Errorf("owner: the timeout stays executable with approval: %+v", f)
		}
		if !owns && (f.RecommendedSQL != "" || !strings.Contains(f.Recommendation, "own")) {
			t.Errorf("not owner: the timeout must be advisory and say why: %+v", f)
		}
	}
}

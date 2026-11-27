package advisor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// advisors-10: the parts of the config SQL matrix that need no server.
// config_sql_matrix_integration_test.go runs the full matrix on real
// PostgreSQL.

// No concurrent access tests: the gate is a pure function of its input.

// matrixProviders are the provider profiles every documented GUC is gated
// for; "self-managed" is the value no managed rule matches.
var matrixProviders = []string{"self-managed", "rds", "aurora", "cloud-sql", "alloydb",
	"azure", "neon", "supabase", "lakebase"}

// matrixHost is host memory large enough (1 TiB, availability unknown)
// that no documented range edge trips a memory guard, so every refusal
// the matrix sees comes from the platform rules.
var matrixHost = HostMemory{TotalBytes: 1 << 40}

// sighupRows gives every documented GUC sighup context, which no managed
// provider can carry as ALTER DATABASE.
func sighupRows() []collector.PGSetting {
	rows := make([]collector.PGSetting, 0, len(documentedGUCs()))
	for _, name := range documentedGUCs() {
		rows = append(rows, collector.PGSetting{Name: name, Context: "sighup"})
	}
	return rows
}

func matrixFinding(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "wal_tuning", Severity: "warning",
		ObjectType: "configuration", ObjectIdentifier: "instance",
		Title: "wal_tuning recommendation for instance", Recommendation: "Tune it.",
		RecommendedSQL: sql}
}

// gateMatrix gates a single finding and requires exactly one back.
func gateMatrix(t *testing.T, f analyzer.Finding, provider, db string,
	rows []collector.PGSetting) analyzer.Finding {
	t.Helper()
	out := GateConfigFindingsHost([]analyzer.Finding{f}, matrixHost, provider, db, rows)
	if len(out) != 1 {
		t.Fatalf("%s on %s: gate returned %d findings, want 1", f.RecommendedSQL,
			provider, len(out))
	}
	return out[0]
}

// assertRefused checks a withheld change: no SQL at all, a message that
// names the setting and the provider, and the typed managed-change intent
// exactly where the provider has one.
func assertRefused(t *testing.T, provider, guc string, got analyzer.Finding) {
	t.Helper()
	if got.RecommendedSQL != "" || got.RollbackSQL != "" {
		t.Errorf("%s on %s: refused but SQL leaked: %q / %q", guc, provider,
			got.RecommendedSQL, got.RollbackSQL)
	}
	msg := strings.ToLower(got.Recommendation)
	if !strings.Contains(msg, guc) || !strings.Contains(msg, provider) {
		t.Errorf("%s on %s: refusal does not name the setting and provider: %q",
			guc, provider, got.Recommendation)
	}
	_, intent := got.Detail[managedparam.DetailKey]
	_, approval := got.Detail[analyzer.DetailApprovalRequired]
	// A database-scope setting never becomes an instance-wide provider
	// change (advisors-07).
	want := managedparam.Supported(provider) && !pgconf.DatabaseScopeApprovalOnly(guc)
	if intent != want || (intent && !approval) {
		t.Errorf("%s on %s: managed intent=%v (want %v), approval=%v", guc, provider,
			intent, want, approval)
	}
}

func TestConfigSQLMatrix_RefusalNamesSettingAndProvider(t *testing.T) {
	for _, provider := range matrixProviders[1:] {
		for _, guc := range documentedGUCs() {
			f := matrixFinding("ALTER SYSTEM SET " + guc + " = '" +
				baseForm(pgconf.Docs[guc].SafeMin, pgconf.Docs[guc]) + "'")
			got := gateMatrix(t, f, provider, "app", sighupRows())
			assertRefused(t, provider, guc, got)
		}
	}
}

func TestConfigSQLMatrix_ReservedDetailKeyLeaksNoSQL(t *testing.T) {
	const forged = "DROP DATABASE prod"
	raw := `[{"object_identifier":"instance","severity":"warning",` +
		`"rationale":"Forced checkpoints dominate.",` +
		`"recommended_sql":"ALTER SYSTEM SET max_wal_size = '4GB'",` +
		`"managed_change":{"provider":"rds","parameter":"max_wal_size",` +
		`"value":"4GB","sql":"` + forged + `"},` +
		`"sage_rollback_sql":"` + forged + `","approval_required":"no",` +
		`"current_settings":{"sage_sql":"` + forged + `"}}]`
	for _, provider := range matrixProviders {
		parsed := parseLLMFindings(raw, "wal_tuning", noopLog)
		if len(parsed) != 1 {
			t.Fatalf("%s: parsed %d findings, want 1", provider, len(parsed))
		}
		got := gateMatrix(t, parsed[0], provider, "app", sighupRows())
		blob, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("%s: marshal: %v", provider, err)
		}
		if strings.Contains(string(blob), forged) {
			t.Errorf("%s: forged SQL leaked into the finding: %s", provider, blob)
		}
		want := ""
		if provider == "self-managed" {
			want = "ALTER SYSTEM SET max_wal_size = '4GB'"
		}
		if got.RecommendedSQL != want || got.RollbackSQL != "" {
			t.Errorf("%s: SQL = %q / %q, want %q and no rollback", provider,
				got.RecommendedSQL, got.RollbackSQL, want)
		}
	}
}

func TestConfigSQLMatrix_EmptyInputs(t *testing.T) {
	if got := GateConfigFindingsHost(nil, matrixHost, "rds", "app", nil); len(got) != 0 {
		t.Errorf("nil findings gave %d", len(got))
	}
	// No pg_settings rows: nothing is confirmed, so nothing runs on a
	// managed provider and the refusal still names setting and provider.
	for _, provider := range matrixProviders[1:] {
		got := gateMatrix(t, matrixFinding("ALTER SYSTEM SET work_mem = '64MB'"), provider,
			"app", nil)
		assertRefused(t, provider, "work_mem", got)
	}
}

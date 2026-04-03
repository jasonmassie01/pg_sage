package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

func TestTransformForCloud_SelfManaged_NoChanges(t *testing.T) {
	findings := []analyzer.Finding{
		{
			RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'",
			Category:       "memory_tuning",
		},
	}
	result := TransformForCloud(findings, "self-managed", "mydb")
	if len(result) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result))
	}
	if result[0].RecommendedSQL != "ALTER SYSTEM SET work_mem = '64MB'" {
		t.Fatalf("expected unchanged SQL, got: %s",
			result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_EmptyPlatform_NoChanges(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	result := TransformForCloud(findings, "", "mydb")
	if result[0].RecommendedSQL != "ALTER SYSTEM SET work_mem = '64MB'" {
		t.Fatalf("expected unchanged SQL for empty platform, got: %s",
			result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_RDS_RewritesToAlterDatabase(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	if len(result) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result))
	}
	expected := `ALTER DATABASE "mydb" SET work_mem = '64MB'`
	if result[0].RecommendedSQL != expected {
		t.Fatalf("expected %q, got %q",
			expected, result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_CloudSQL_RewritesToAlterDatabase(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'"},
	}
	result := TransformForCloud(findings, "cloud-sql", "postgres")
	expected := `ALTER DATABASE "postgres" SET max_wal_size = '4GB'`
	if result[0].RecommendedSQL != expected {
		t.Fatalf("expected %q, got %q",
			expected, result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_Aurora_RewritesToAlterDatabase(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '128MB'"},
	}
	result := TransformForCloud(findings, "aurora", "proddb")
	expected := `ALTER DATABASE "proddb" SET work_mem = '128MB'`
	if result[0].RecommendedSQL != expected {
		t.Fatalf("expected %q, got %q",
			expected, result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_AlloyDB_RewritesToAlterDatabase(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	result := TransformForCloud(findings, "alloydb", "testdb")
	expected := `ALTER DATABASE "testdb" SET work_mem = '64MB'`
	if result[0].RecommendedSQL != expected {
		t.Fatalf("expected %q, got %q",
			expected, result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_Azure_RewritesToAlterDatabase(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	result := TransformForCloud(findings, "azure", "azuredb")
	expected := `ALTER DATABASE "azuredb" SET work_mem = '64MB'`
	if result[0].RecommendedSQL != expected {
		t.Fatalf("expected %q, got %q",
			expected, result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_RollbackSQL_Generated(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	expectedRollback := `ALTER DATABASE "mydb" RESET work_mem`
	if result[0].RollbackSQL != expectedRollback {
		t.Fatalf("expected rollback %q, got %q",
			expectedRollback, result[0].RollbackSQL)
	}
}

func TestTransformForCloud_AlterSystemReset_Rewritten(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM RESET work_mem"},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	expected := `ALTER DATABASE "mydb" RESET work_mem`
	if result[0].RecommendedSQL != expected {
		t.Fatalf("expected %q, got %q",
			expected, result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_RestartRequired_DropsSQL(t *testing.T) {
	// max_connections requires restart — on managed services,
	// it must be changed via the platform console.
	findings := []analyzer.Finding{
		{
			RecommendedSQL: "ALTER SYSTEM SET max_connections = 200",
			Severity:       "warning",
			Recommendation: "Increase max_connections",
		},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	if len(result) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result))
	}
	if result[0].RecommendedSQL != "" {
		t.Fatalf("expected empty SQL for restart-required GUC, got: %s",
			result[0].RecommendedSQL)
	}
	if result[0].Severity != "info" {
		t.Fatalf("expected severity downgraded to info, got: %s",
			result[0].Severity)
	}
	if !strings.Contains(result[0].Recommendation, "requires a restart") {
		t.Fatalf("expected restart note in recommendation, got: %s",
			result[0].Recommendation)
	}
	if !strings.Contains(result[0].Recommendation, "rds") {
		t.Fatalf("expected platform name in recommendation, got: %s",
			result[0].Recommendation)
	}
}

func TestTransformForCloud_SharedBuffers_DropsSQL(t *testing.T) {
	findings := []analyzer.Finding{
		{
			RecommendedSQL: "ALTER SYSTEM SET shared_buffers = '4GB'",
			Severity:       "warning",
		},
	}
	result := TransformForCloud(findings, "cloud-sql", "mydb")
	if result[0].RecommendedSQL != "" {
		t.Fatalf("expected empty SQL for shared_buffers on cloud-sql, got: %s",
			result[0].RecommendedSQL)
	}
	if result[0].Severity != "info" {
		t.Fatalf("expected info severity, got: %s", result[0].Severity)
	}
}

func TestTransformForCloud_WalLevel_DropsSQL(t *testing.T) {
	findings := []analyzer.Finding{
		{
			RecommendedSQL: "ALTER SYSTEM SET wal_level = 'logical'",
			Severity:       "warning",
		},
	}
	result := TransformForCloud(findings, "aurora", "mydb")
	if result[0].RecommendedSQL != "" {
		t.Fatalf("expected empty SQL for wal_level on aurora, got: %s",
			result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_EmptySQL_Passthrough(t *testing.T) {
	findings := []analyzer.Finding{
		{
			Category:       "vacuum_tuning",
			RecommendedSQL: "",
			Recommendation: "Everything looks fine",
		},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	if len(result) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result))
	}
	if result[0].Recommendation != "Everything looks fine" {
		t.Fatalf("expected unchanged finding")
	}
}

func TestTransformForCloud_AlterTable_NotRewritten(t *testing.T) {
	// ALTER TABLE SET is for per-table vacuum overrides — not
	// rewritten, just passed through.
	findings := []analyzer.Finding{
		{
			RecommendedSQL: "ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)",
		},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	if result[0].RecommendedSQL != findings[0].RecommendedSQL {
		t.Fatalf("expected ALTER TABLE to pass through, got: %s",
			result[0].RecommendedSQL)
	}
}

func TestTransformForCloud_MixedFindings(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
		{RecommendedSQL: "ALTER SYSTEM SET max_connections = 200"},
		{RecommendedSQL: "ALTER TABLE public.t SET (fillfactor = 90)"},
		{RecommendedSQL: ""},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	if len(result) != 4 {
		t.Fatalf("expected 4 findings, got %d", len(result))
	}
	// work_mem: rewritten to ALTER DATABASE
	if !strings.HasPrefix(result[0].RecommendedSQL, "ALTER DATABASE") {
		t.Fatalf("expected ALTER DATABASE for work_mem, got: %s",
			result[0].RecommendedSQL)
	}
	// max_connections: restart-required, SQL dropped
	if result[1].RecommendedSQL != "" {
		t.Fatalf("expected empty SQL for max_connections, got: %s",
			result[1].RecommendedSQL)
	}
	// ALTER TABLE: unchanged
	if !strings.HasPrefix(result[2].RecommendedSQL, "ALTER TABLE") {
		t.Fatalf("expected ALTER TABLE unchanged, got: %s",
			result[2].RecommendedSQL)
	}
	// Empty: unchanged
	if result[3].RecommendedSQL != "" {
		t.Fatalf("expected empty SQL unchanged")
	}
}

func TestTransformForCloud_QuotedDBName_NotDoubled(t *testing.T) {
	findings := []analyzer.Finding{
		{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	// DB name already quoted
	result := TransformForCloud(findings, "rds", `"my-db"`)
	if !strings.Contains(result[0].RecommendedSQL, `"my-db"`) {
		t.Fatalf("expected single-quoted db name, got: %s",
			result[0].RecommendedSQL)
	}
	if strings.Contains(result[0].RecommendedSQL, `""my-db""`) {
		t.Fatalf("db name was double-quoted: %s",
			result[0].RecommendedSQL)
	}
}

func TestExtractSettingName_AlterSystemSet(t *testing.T) {
	cases := []struct {
		sql  string
		want string
	}{
		{"ALTER SYSTEM SET work_mem = '64MB'", "work_mem"},
		{"ALTER SYSTEM SET max_wal_size = '4GB'", "max_wal_size"},
		{"ALTER SYSTEM RESET work_mem", "work_mem"},
		{"ALTER SYSTEM SET shared_buffers = '2GB'", "shared_buffers"},
		{"alter system set WORK_MEM = '64MB'", "work_mem"},
		{"ALTER TABLE t SET (fillfactor = 90)", ""},
		{"CREATE INDEX idx ON t(id)", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := extractSettingName(tc.sql)
		if got != tc.want {
			t.Errorf("extractSettingName(%q) = %q, want %q",
				tc.sql, got, tc.want)
		}
	}
}

func TestIsManagedService(t *testing.T) {
	managed := []string{"rds", "aurora", "cloud-sql", "alloydb", "azure"}
	for _, p := range managed {
		if !IsManagedService(p) {
			t.Errorf("expected %q to be managed", p)
		}
	}
	notManaged := []string{"self-managed", "unknown", "", "on-prem"}
	for _, p := range notManaged {
		if IsManagedService(p) {
			t.Errorf("expected %q to NOT be managed", p)
		}
	}
}

func TestTransformForCloud_AllPlatforms(t *testing.T) {
	// Every managed platform should rewrite ALTER SYSTEM to ALTER DATABASE.
	platforms := []string{"rds", "aurora", "cloud-sql", "alloydb", "azure"}
	for _, p := range platforms {
		t.Run(p, func(t *testing.T) {
			findings := []analyzer.Finding{
				{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"},
			}
			result := TransformForCloud(findings, p, "testdb")
			if !strings.HasPrefix(result[0].RecommendedSQL, "ALTER DATABASE") {
				t.Fatalf("expected ALTER DATABASE on %s, got: %s",
					p, result[0].RecommendedSQL)
			}
		})
	}
}

func TestTransformForCloud_RollbackSQL_ClearedForRestart(t *testing.T) {
	findings := []analyzer.Finding{
		{
			RecommendedSQL: "ALTER SYSTEM SET max_connections = 200",
			RollbackSQL:    "ALTER SYSTEM SET max_connections = 100",
		},
	}
	result := TransformForCloud(findings, "rds", "mydb")
	if result[0].RollbackSQL != "" {
		t.Fatalf("expected empty rollback SQL for restart-required GUC, got: %s",
			result[0].RollbackSQL)
	}
}

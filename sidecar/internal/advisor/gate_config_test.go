package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// GateConfigFindings is the one set of configuration gates every GUC or
// storage-parameter change passes, whoever proposed it: the advisor's
// remaining sub-advisors and the tuning agent (roadmap 2.2).

func configFinding(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "memory_tuning", Severity: "info",
		ObjectType: "configuration", ObjectIdentifier: "instance", Title: "t",
		Detail: map[string]any{}, Recommendation: "why", RecommendedSQL: sql,
		ActionRisk: "moderate"}
}

func TestGateConfigFindings(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Setting: "4096", Unit: "kB"}}
	for _, tc := range []struct {
		name, sql, cloud string
		hostMem          int64
		wantSQL          string // "" means not executable
		approval, keep   bool
	}{
		{"work_mem self-managed", "ALTER SYSTEM SET work_mem = '64MB'", "", 0,
			"ALTER SYSTEM SET work_mem = '64MB'", false, true},
		{"work_mem on rds", "ALTER SYSTEM SET work_mem = '64MB'", "rds", 0,
			`ALTER DATABASE "app" SET work_mem = '64MB'`, false, true},
		{"shared_buffers without host memory", "ALTER SYSTEM SET shared_buffers = '1GB'",
			"", 0, "", false, true},
		{"shared_buffers grounded", "ALTER SYSTEM SET shared_buffers = '4GB'", "",
			16 << 30, "ALTER SYSTEM SET shared_buffers = '4GB'", true, true},
		{"not allowlisted", "ALTER SYSTEM SET fsync = 'off'", "", 0, "", false, true},
		{"out of range", "ALTER SYSTEM SET work_mem = '64TB'", "", 0, "", false, false},
		{"autovacuum reloption", "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)",
			"", 0, "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := GateConfigFindings([]analyzer.Finding{configFinding(tc.sql)}, tc.hostMem,
				tc.cloud, "app", settings)
			if !tc.keep {
				if len(out) != 0 {
					t.Fatalf("an out-of-range value is dropped: %+v", out)
				}
				return
			}
			if len(out) != 1 {
				t.Fatalf("findings = %+v", out)
			}
			f := out[0]
			if strings.TrimSpace(f.RecommendedSQL) != tc.wantSQL {
				t.Fatalf("sql = %q, want %q", f.RecommendedSQL, tc.wantSQL)
			}
			_, approval := f.Detail[analyzer.DetailApprovalRequired]
			if approval != tc.approval {
				t.Fatalf("approval required = %v, want %v (%v)", approval, tc.approval, f.Detail)
			}
			if tc.wantSQL == "" && !strings.Contains(f.Recommendation, "(") {
				t.Fatalf("a non-executable change says why: %q", f.Recommendation)
			}
		})
	}
}

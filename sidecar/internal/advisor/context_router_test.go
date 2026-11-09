package advisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// managedPlatforms is every provider IsManagedService accepts.
var managedPlatforms = []string{
	"rds", "aurora", "cloud-sql", "alloydb", "azure", "neon", "supabase", "lakebase",
}

// contextFixture is a pg_settings (name, context) dump recorded from a
// real server (testdata/pg_settings_context_pg<major>.json).
type contextFixture struct {
	ServerVersion string                `json:"server_version"`
	Settings      []collector.PGSetting `json:"settings"`
}

func loadContextFixture(t *testing.T, file string) contextFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture %s: %v", file, err)
	}
	var fx contextFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture %s: %v", file, err)
	}
	if len(fx.Settings) == 0 || fx.ServerVersion == "" {
		t.Fatalf("fixture %s is empty: %+v", file, fx)
	}
	return fx
}

func fixtureContext(fx contextFixture, name string) string {
	for _, s := range fx.Settings {
		if s.Name == name {
			return s.Context
		}
	}
	return ""
}

// TestContextRouterEveryDocumentedGUC routes every documented GUC on every
// managed provider with the contexts PostgreSQL 14 and 17 report. (No
// PostgreSQL 18 image exists on the build host; its only context change
// among these GUCs, autovacuum_max_workers becoming sighup, is in the
// restart list either way.)
func TestContextRouterEveryDocumentedGUC(t *testing.T) {
	for _, file := range []string{"pg_settings_context_pg14.json",
		"pg_settings_context_pg17.json"} {
		fx := loadContextFixture(t, file)
		for name := range pgconf.Docs {
			settingContext := fixtureContext(fx, name)
			if settingContext == "" {
				t.Fatalf("%s: fixture lacks documented GUC %s", file, name)
			}
			for _, platform := range managedPlatforms {
				t.Run(fmt.Sprintf("%s/%s/%s", fx.ServerVersion[:2], platform, name),
					func(t *testing.T) {
						checkRouted(t, platform, name, settingContext, fx.Settings)
					})
			}
		}
	}
}

func checkRouted(t *testing.T, platform, name, settingContext string,
	settings []collector.PGSetting) {
	t.Helper()
	in := analyzer.Finding{Severity: "warning", Recommendation: "tune " + name,
		RecommendedSQL: "ALTER SYSTEM SET " + name + " = '42'"}
	got := TransformForCloud([]analyzer.Finding{in}, platform, "appdb", settings)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	_, hasIntent := managedparam.IntentFromDetail(f.Detail)
	if settingContext == "user" && !pgconf.RequiresRestart(name) {
		want := `ALTER DATABASE "appdb" SET ` + name + " = '42'"
		if f.RecommendedSQL != want || f.Severity != "warning" || hasIntent {
			t.Fatalf("user context: sql=%q severity=%s intent=%v, want %q",
				f.RecommendedSQL, f.Severity, hasIntent, want)
		}
		return
	}
	if f.RecommendedSQL != "" || f.RollbackSQL != "" || f.Severity != "info" {
		t.Fatalf("%s context emitted SQL: sql=%q rollback=%q severity=%s",
			settingContext, f.RecommendedSQL, f.RollbackSQL, f.Severity)
	}
	if hasIntent != managedparam.Supported(platform) {
		t.Fatalf("%s context on %s: managed intent=%v, want %v",
			settingContext, platform, hasIntent, managedparam.Supported(platform))
	}
	if f.Recommendation == in.Recommendation {
		t.Fatal("advisory finding carries no provider guidance")
	}
}

func TestContextRouterMissingSettingsNeverAlterDatabase(t *testing.T) {
	sqls := []string{"ALTER SYSTEM SET work_mem = '64MB'", "ALTER SYSTEM RESET work_mem",
		"ALTER SYSTEM SET max_wal_size = '4GB'"}
	for _, platform := range managedPlatforms {
		for _, sql := range sqls {
			for label, settings := range map[string][][]collector.PGSetting{
				"absent": nil, "nil": {nil}, "empty": {{}},
				"other": {{{Name: "random_page_cost", Context: "user"}}},
			} {
				got := TransformForCloud([]analyzer.Finding{{RecommendedSQL: sql}},
					platform, "appdb", settings...)[0]
				if got.RecommendedSQL != "" || got.RollbackSQL != "" || got.Severity != "info" {
					t.Fatalf("%s/%s/%s: unknown context emitted %q", platform, label, sql,
						got.RecommendedSQL)
				}
				if !strings.Contains(got.Recommendation, "context") {
					t.Fatalf("%s/%s: advisory note does not name the missing context: %q",
						platform, label, got.Recommendation)
				}
			}
		}
	}
}

func TestContextRouterUnknownContextsFailClosed(t *testing.T) {
	for _, settingContext := range []string{"", "backend", "superuser-backend",
		"internal", "USER", "bogus"} {
		settings := []collector.PGSetting{{Name: "work_mem", Context: settingContext}}
		for _, platform := range managedPlatforms {
			got := TransformForCloud([]analyzer.Finding{{
				RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}}, platform, "db",
				settings)[0]
			if got.RecommendedSQL != "" || got.Severity != "info" {
				t.Fatalf("%s context %q emitted %q", platform, settingContext,
					got.RecommendedSQL)
			}
			// The managed intent stays operator-approved: a parameter group or
			// flag can carry the change whatever its context.
			if _, intent := managedparam.IntentFromDetail(got.Detail); intent !=
				managedparam.Supported(platform) {
				t.Fatalf("%s context %q: managed intent = %v", platform, settingContext, intent)
			}
		}
	}
}

func TestContextRouterSuperuserNeedsSetPrivilege(t *testing.T) {
	sql := "ALTER SYSTEM SET log_min_duration_statement = '1s'"
	for _, platform := range managedPlatforms {
		allowed := TransformForCloud([]analyzer.Finding{{RecommendedSQL: sql}}, platform, "db",
			[]collector.PGSetting{{Name: "log_min_duration_statement", Context: "superuser",
				SetPrivilege: true}})[0]
		want := `ALTER DATABASE "db" SET log_min_duration_statement = '1s'`
		if allowed.RecommendedSQL != want {
			t.Fatalf("%s: privileged superuser setting = %q, want %q", platform,
				allowed.RecommendedSQL, want)
		}
		refused := TransformForCloud([]analyzer.Finding{{RecommendedSQL: sql}}, platform, "db",
			[]collector.PGSetting{{Name: "log_min_duration_statement",
				Context: "superuser"}})[0]
		if refused.RecommendedSQL != "" || refused.Severity != "info" {
			t.Fatalf("%s: unprivileged superuser setting emitted %q", platform,
				refused.RecommendedSQL)
		}
	}
}

func TestContextRouterSighupGetsManagedIntentNotSQL(t *testing.T) {
	settings := []collector.PGSetting{{Name: "max_wal_size", Context: "sighup"}}
	for _, platform := range managedPlatforms {
		got := TransformForCloud([]analyzer.Finding{{Severity: "warning",
			RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'"}}, platform, "db",
			settings)[0]
		if got.RecommendedSQL != "" || got.Severity != "info" {
			t.Fatalf("%s: sighup setting emitted %q", platform, got.RecommendedSQL)
		}
		in, ok := managedparam.IntentFromDetail(got.Detail)
		if ok != managedparam.Supported(platform) {
			t.Fatalf("%s: managed intent=%v", platform, ok)
		}
		if ok && (in.Parameter != "max_wal_size" || in.Value != "4GB") {
			t.Fatalf("%s: intent = %+v", platform, in)
		}
		guidance := platformSettingsSurface[platform]
		if tenantProvider(platform) {
			guidance = tenantConfigGuidance(platform, "max_wal_size", "sighup")
		}
		if !ok && !strings.Contains(got.Recommendation, guidance) {
			t.Fatalf("%s: guidance does not name where to change it: %q", platform,
				got.Recommendation)
		}
	}
}

func TestContextRouterEmptyDatabaseNameIsAdvisory(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	for _, platform := range managedPlatforms {
		got := TransformForCloud([]analyzer.Finding{{
			RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}}, platform, " ", settings)[0]
		if got.RecommendedSQL != "" || got.Recommendation == "" {
			t.Fatalf("%s: no database name still emitted %q", platform, got.RecommendedSQL)
		}
	}
}

// State transition: a tenant provider's user-context change is executable
// without a caller rollback; the executor captures the prior value.
func TestContextRouterNeonUserChangeWithoutRollbackIsExecutable(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	got := TransformForCloud([]analyzer.Finding{{
		RecommendedSQL: "ALTER SYSTEM SET work_mem = '16MB'"}}, "neon", "app", settings)[0]
	if got.RecommendedSQL != `ALTER DATABASE "app" SET work_mem = '16MB'` {
		t.Fatalf("Neon user-context change not executable: %+v", got)
	}
	if got.RollbackSQL != `ALTER DATABASE "app" RESET work_mem` || got.Severity == "info" {
		t.Fatalf("rollback hint = %q severity = %s", got.RollbackSQL, got.Severity)
	}
}

func TestContextRouterSelfManagedKeepsAlterSystem(t *testing.T) {
	settings := []collector.PGSetting{{Name: "max_wal_size", Context: "sighup"}}
	in := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'"}}
	for _, platform := range []string{"", "self-managed", "unknown", "on-prem"} {
		got := TransformForCloud(in, platform, "db", settings)
		if !reflect.DeepEqual(got, in) {
			t.Fatalf("%q: self-managed finding changed: %+v", platform, got)
		}
	}
}

func TestContextRouterDoesNotMutateInput(t *testing.T) {
	settings := []collector.PGSetting{{Name: "max_wal_size", Context: "sighup"}}
	detail := map[string]any{"k": "v"}
	in := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'",
		Detail: detail}}
	_ = TransformForCloud(in, "cloud-sql", "db", settings)
	if in[0].RecommendedSQL != "ALTER SYSTEM SET max_wal_size = '4GB'" || len(detail) != 1 {
		t.Fatalf("input mutated: %+v detail=%v", in[0], detail)
	}
}

func TestContextRouterConcurrentCallsAgree(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"},
		{Name: "max_wal_size", Context: "sighup"}}
	in := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '8MB'"},
		{RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'", Detail: map[string]any{}}}
	want := TransformForCloud(in, "rds", "db", settings)
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := TransformForCloud(in, "rds", "db", settings); !reflect.DeepEqual(got, want) {
				errs <- fmt.Sprintf("%+v", got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent transform diverged: %s", e)
	}
}

// CHECK-14: on Cloud SQL, AlloyDB and Azure no gated finding asks for
// ALTER DATABASE ... SET max_wal_size, with real pg_settings contexts.
func TestGateConfigNeverRewritesMaxWALSizeToAlterDatabase(t *testing.T) {
	fx := loadContextFixture(t, "pg_settings_context_pg17.json")
	for _, platform := range []string{"cloud-sql", "alloydb", "azure"} {
		f := configFinding("ALTER SYSTEM SET max_wal_size = '4GB'")
		out := GateConfigFindingsHost([]analyzer.Finding{f}, HostMemory{}, platform, "app",
			fx.Settings)
		for _, g := range out {
			upper := strings.ToUpper(g.RecommendedSQL)
			if strings.HasPrefix(upper, "ALTER DATABASE") &&
				strings.Contains(upper, "SET MAX_WAL_SIZE") {
				t.Fatalf("%s: refused rewrite survived the gates: %q", platform,
					g.RecommendedSQL)
			}
		}
		if len(out) != 1 || out[0].Recommendation == "" {
			t.Fatalf("%s: advisory finding lost: %+v", platform, out)
		}
	}
}

// An ALTER SYSTEM the parser cannot read names no setting to route, and
// every managed provider refuses ALTER SYSTEM: it must not stay executable.
func TestContextRouterUnparsableAlterSystemIsAdvisory(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	for _, platform := range managedPlatforms {
		for _, sql := range []string{"ALTER SYSTEM RESET ALL", "alter system set work_mem",
			"ALTER SYSTEM"} {
			got := TransformForCloud([]analyzer.Finding{{Severity: "warning",
				RecommendedSQL: sql, RollbackSQL: "ALTER SYSTEM RESET work_mem"}},
				platform, "db", settings)[0]
			if got.RecommendedSQL != "" || got.RollbackSQL != "" || got.Severity != "info" {
				t.Fatalf("%s %q: sql=%q rollback=%q severity=%s", platform, sql,
					got.RecommendedSQL, got.RollbackSQL, got.Severity)
			}
			if !strings.Contains(got.Recommendation, "ALTER SYSTEM") {
				t.Fatalf("%s %q: note does not explain the refusal: %q", platform, sql,
					got.Recommendation)
			}
		}
	}
	other := analyzer.Finding{RecommendedSQL: "VACUUM (ANALYZE) public.t"}
	if got := TransformForCloud([]analyzer.Finding{other}, "rds", "db", settings)[0]; !reflect.
		DeepEqual(got, other) {
		t.Fatalf("non-setting SQL changed: %+v", got)
	}
}

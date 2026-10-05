package advisor

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Host memory from managed-cloud telemetry grounds the memory gates:
// shared_buffers needs host RAM to be proposed at all, work_mem is capped
// against it, and memory pressure (little available RAM) withholds every
// memory increase. Unknown memory keeps today's behavior.

var memSettings = []collector.PGSetting{
	{Name: "work_mem", Setting: "4096", Unit: "kB"},
	{Name: "shared_buffers", Setting: "131072", Unit: "8kB"},
	{Name: "maintenance_work_mem", Setting: "65536", Unit: "kB"},
}

func gateOne(t *testing.T, sql, cloud string, host HostMemory) analyzer.Finding {
	t.Helper()
	out := GateConfigFindingsHost([]analyzer.Finding{configFinding(sql)}, host, cloud, "app",
		memSettings)
	if len(out) != 1 {
		t.Fatalf("findings = %+v", out)
	}
	return out[0]
}

func TestSharedBuffersOnManagedBecomesManagedProposal(t *testing.T) {
	host := HostMemory{TotalBytes: 16 << 30, AvailableBytes: 8 << 30}
	for _, cloud := range []string{"rds", "aurora", "cloud-sql"} {
		f := gateOne(t, "ALTER SYSTEM SET shared_buffers = '4GB'", cloud, host)
		if f.RecommendedSQL != "" || f.RollbackSQL != "" {
			t.Fatalf("%s: managed shared_buffers must not be SQL: %q", cloud, f.RecommendedSQL)
		}
		in, ok := managedparam.IntentFromDetail(f.Detail)
		if !ok || in.Provider != cloud || in.Parameter != "shared_buffers" || in.Value != "4GB" {
			t.Fatalf("%s: intent = %+v, %t (detail %v)", cloud, in, ok, f.Detail)
		}
		if _, approval := f.Detail[analyzer.DetailApprovalRequired]; !approval {
			t.Fatalf("%s: a managed change always needs an operator", cloud)
		}
	}
}

func TestSharedBuffersWithoutHostMemoryStaysAdvisory(t *testing.T) {
	f := gateOne(t, "ALTER SYSTEM SET shared_buffers = '4GB'", "rds", HostMemory{})
	if _, ok := managedparam.IntentFromDetail(f.Detail); ok || f.RecommendedSQL != "" {
		t.Fatalf("ungrounded shared_buffers must stay advisory: %+v", f)
	}
	if !strings.Contains(f.Recommendation, "host memory is unknown") {
		t.Fatalf("recommendation = %q", f.Recommendation)
	}
}

func TestSharedBuffersAboveFractionRefusedEvenWithTelemetry(t *testing.T) {
	f := gateOne(t, "ALTER SYSTEM SET shared_buffers = '12GB'", "rds",
		HostMemory{TotalBytes: 16 << 30})
	if _, ok := managedparam.IntentFromDetail(f.Detail); ok || f.RecommendedSQL != "" {
		t.Fatalf("12GB of 16GB must be refused: %+v", f)
	}
}

// Other managed providers keep their own forms: no managed intent.
func TestManagedIntentOnlyForSupportedProviders(t *testing.T) {
	host := HostMemory{TotalBytes: 16 << 30}
	for _, cloud := range []string{"", "self-managed", "alloydb", "azure"} {
		f := gateOne(t, "ALTER SYSTEM SET shared_buffers = '4GB'", cloud, host)
		if _, ok := managedparam.IntentFromDetail(f.Detail); ok {
			t.Fatalf("%q must not carry a managed intent: %v", cloud, f.Detail)
		}
	}
	// A restricted (not restart-required) setting on RDS also routes.
	f := gateOne(t, "ALTER SYSTEM SET max_wal_size = '4GB'", "rds", host)
	if in, ok := managedparam.IntentFromDetail(f.Detail); !ok || in.Parameter != "max_wal_size" {
		t.Fatalf("restricted max_wal_size on rds = %+v", f.Detail)
	}
	// Not restart-required, yet a managed change still needs an operator.
	if _, approval := f.Detail[analyzer.DetailApprovalRequired]; !approval {
		t.Fatalf("a managed max_wal_size change needs an operator: %v", f.Detail)
	}
	// A setting ALTER DATABASE can carry keeps its SQL and gets no intent.
	f = gateOne(t, "ALTER SYSTEM SET work_mem = '64MB'", "rds", host)
	if _, ok := managedparam.IntentFromDetail(f.Detail); ok ||
		!strings.HasPrefix(f.RecommendedSQL, "ALTER DATABASE") {
		t.Fatalf("work_mem on rds = %+v", f)
	}
}

func TestWorkMemGuardBoundaries(t *testing.T) {
	host := HostMemory{TotalBytes: 16 << 30} // 5% = 819.2MB
	ok := gateOne(t, "ALTER SYSTEM SET work_mem = '800MB'", "", host)
	if ok.RecommendedSQL == "" {
		t.Fatalf("800MB is within 5%% of 16GB: %+v", ok)
	}
	refused := gateOne(t, "ALTER SYSTEM SET work_mem = '820MB'", "", host)
	if refused.RecommendedSQL != "" || !strings.Contains(refused.Recommendation, "work_mem") {
		t.Fatalf("820MB exceeds 5%% of 16GB: %+v", refused)
	}
	unknown := gateOne(t, "ALTER SYSTEM SET work_mem = '1GB'", "", HostMemory{})
	if unknown.RecommendedSQL == "" {
		t.Fatalf("unknown host memory keeps today's behavior: %+v", unknown)
	}
}

func TestMemoryPressureWithholdsIncreases(t *testing.T) {
	pressure := HostMemory{TotalBytes: 16 << 30, AvailableBytes: 512 << 20} // 3.1%
	up := gateOne(t, "ALTER SYSTEM SET work_mem = '64MB'", "", pressure)
	if up.RecommendedSQL != "" || !strings.Contains(up.Recommendation, "available") {
		t.Fatalf("raising work_mem under memory pressure: %+v", up)
	}
	// Lowering (16MB running -> 8MB) relieves pressure. 2MB would be below
	// the documented safe minimum and dropped by the range gate.
	high := []collector.PGSetting{{Name: "work_mem", Setting: "16384", Unit: "kB"}}
	out := GateConfigFindingsHost([]analyzer.Finding{configFinding(
		"ALTER SYSTEM SET work_mem = '8MB'")}, pressure, "", "app", high)
	if len(out) != 1 {
		t.Fatalf("findings = %+v", out)
	}
	if down := out[0]; down.RecommendedSQL == "" {
		t.Fatalf("lowering work_mem relieves pressure and stays executable: %+v", down)
	}
	notMem := gateOne(t, "ALTER SYSTEM SET random_page_cost = '1.1'", "", pressure)
	if notMem.RecommendedSQL == "" {
		t.Fatalf("non-memory settings are unaffected: %+v", notMem)
	}
	edge := HostMemory{TotalBytes: 100 << 20, AvailableBytes: 5 << 20} // exactly 5%
	if f := gateOne(t, "ALTER SYSTEM SET work_mem = '5MB'", "", edge); f.RecommendedSQL == "" {
		t.Fatalf("exactly at the floor is not pressure: %+v", f)
	}
}

// Never-widen: telemetry never removes an approval requirement, and on
// managed providers it never produces executable SQL that zero telemetry
// would not also produce.
func TestHostMemoryNeverWidens(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	sqls := []string{"ALTER SYSTEM SET shared_buffers = '2GB'",
		"ALTER SYSTEM SET work_mem = '256MB'", "ALTER SYSTEM SET max_connections = '300'",
		"ALTER SYSTEM SET maintenance_work_mem = '1GB'",
		"ALTER SYSTEM SET random_page_cost = '1.1'"}
	clouds := []string{"", "rds", "aurora", "cloud-sql"}
	for i := 0; i < 500; i++ {
		sql, cloud := sqls[rng.Intn(len(sqls))], clouds[rng.Intn(len(clouds))]
		total := int64(rng.Intn(64)+1) << 30
		host := HostMemory{TotalBytes: total, AvailableBytes: rng.Int63n(total)}
		with, without := gateOne(t, sql, cloud, host), gateOne(t, sql, cloud, HostMemory{})
		_, apWith := with.Detail[analyzer.DetailApprovalRequired]
		_, apWithout := without.Detail[analyzer.DetailApprovalRequired]
		if apWithout && !apWith && without.RecommendedSQL != "" {
			t.Fatalf("telemetry removed approval: %s on %q host %+v", sql, cloud, host)
		}
		if cloud != "" && with.RecommendedSQL != "" && without.RecommendedSQL == "" {
			t.Fatalf("telemetry made %s executable on %s", sql, cloud)
		}
		if strings.Contains(sql, "shared_buffers") && with.RecommendedSQL != "" && !apWith {
			t.Fatalf("executable shared_buffers without approval: %+v", with)
		}
	}
}

func TestAdvisorHostMemorySource(t *testing.T) {
	a := &Advisor{}
	if got := a.hostMemory(); got != (HostMemory{}) {
		t.Fatalf("no source = %+v", got)
	}
	a.WithHostMemoryBytes(8 << 30)
	if got := a.hostMemory(); got.TotalBytes != 8<<30 {
		t.Fatalf("static = %+v", got)
	}
	a.WithHostMemorySource(func() HostMemory {
		return HostMemory{TotalBytes: 32 << 30,
			AvailableBytes: 1 << 30}
	})
	if got := a.hostMemory(); got.TotalBytes != 32<<30 || got.AvailableBytes != 1<<30 {
		t.Fatalf("telemetry source = %+v", got)
	}
	a.WithHostMemorySource(func() HostMemory { return HostMemory{} })
	if got := a.hostMemory(); got.TotalBytes != 8<<30 {
		t.Fatalf("unknown telemetry falls back to the operator figure: %+v", got)
	}
}

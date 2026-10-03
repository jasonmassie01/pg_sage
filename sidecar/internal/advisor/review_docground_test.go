package advisor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// G3-B01: unitless GUC values are in each GUC's PostgreSQL base unit
// (work_mem/maintenance_work_mem kB; shared_buffers/effective_cache_size/
// wal_buffers 8kB pages; max_wal_size MB). Unit suffixes are
// case-sensitive (kB, MB, GB, TB); whitespace before the unit is allowed.
var baseUnitCases = []struct {
	guc, value string
	wantOK     bool
	why        string
}{
	// work_mem: base unit kB. SafeRange 4MB..2GB.
	{"work_mem", "268435456", false, "256MB in bytes = 256GB in kB"},
	{"work_mem", "65536", true, "64MB in kB"},
	{"work_mem", "64MB", true, "explicit MB"},
	{"work_mem", "64 MB", true, "whitespace before unit"},
	{"work_mem", "8388608B", true, "8MB in bytes"},
	{"work_mem", "64mb", false, "units are case-sensitive in PG"},
	{"work_mem", "64Mb", false, "units are case-sensitive in PG"},
	{"work_mem", "4096", true, "4MB lower boundary in kB"},
	{"work_mem", "4095", false, "just below 4MB"},
	{"work_mem", "2GB", true, "upper boundary"},
	{"work_mem", "2097153", false, "just above 2GB in kB"},
	{"work_mem", "-1", false, "negative"},
	// maintenance_work_mem: base unit kB. 64MB..8GB.
	{"maintenance_work_mem", "1048576", true, "1GB in kB"},
	{"maintenance_work_mem", "1073741824", false, "1GB in bytes = 1TB"},
	{"maintenance_work_mem", "1GB", true, "explicit GB"},
	// shared_buffers: base unit 8kB pages. 128MB..256GB.
	{"shared_buffers", "16384", true, "128MB in 8kB pages"},
	{"shared_buffers", "16383", false, "just below 128MB"},
	{"shared_buffers", "1048576", true, "8GB in pages"},
	{"shared_buffers", "8GB", true, "explicit GB"},
	{"shared_buffers", "137438953472", false, "128GB in bytes"},
	// effective_cache_size: base unit 8kB pages. 256MB..1TB.
	{"effective_cache_size", "524288", true, "4GB in pages"},
	{"effective_cache_size", "4294967296", false, "4GB in bytes"},
	// wal_buffers: base unit 8kB pages. 1MB..1GB, -1 = auto.
	{"wal_buffers", "2048", true, "16MB in pages"},
	{"wal_buffers", "16777216", false, "16MB in bytes = 128GB"},
	{"wal_buffers", "-1", true, "auto-tune sentinel"},
	{"wal_buffers", "16MB", true, "explicit MB"},
	// max_wal_size: base unit MB. 512MB..64GB.
	{"max_wal_size", "2048", true, "2GB in MB"},
	{"max_wal_size", "4096MB", true, "explicit MB"},
	{"max_wal_size", "1073741824", false, "1GB in bytes"},
	{"max_wal_size", "1TB", false, "above 64GB"},
	// Unitless parameters are unchanged.
	{"random_page_cost", "1.1", true, "ratio"},
	{"random_page_cost", "10", false, "ratio above max"},
	{"checkpoint_completion_target", "0.9", true, "ratio"},
	{"checkpoint_completion_target", "0.99", false, "ratio above max"},
	{"default_statistics_target", "500", true, "count"},
	{"default_statistics_target", "5000", false, "count above max"},
	{"effective_io_concurrency", "200", true, "count"},
	{"effective_io_concurrency", "5000", false, "count above max"},
	// Limits ported from the deleted ValidateConfigRecommendation (G3-D08).
	{"max_connections", "200", true, "count"},
	{"max_connections", "5", false, "below 10"},
	{"autovacuum_vacuum_scale_factor", "0.05", true, "ratio"},
	{"autovacuum_vacuum_scale_factor", "0", false, "zero disables scaling"},
	{"autovacuum_vacuum_threshold", "50", true, "count"},
	{"autovacuum_vacuum_cost_delay", "2ms", true, "time with unit"},
	{"autovacuum_vacuum_cost_delay", "2", true, "base unit ms"},
	{"autovacuum_vacuum_cost_delay", "1s", false, "1000ms above 100ms"},
	{"autovacuum_vacuum_cost_limit", "2000", true, "count"},
	{"autovacuum_vacuum_cost_limit", "-1", true, "use vacuum_cost_limit"},
	{"autovacuum_vacuum_cost_limit", "0", false, "zero stalls vacuum"},
	// Added with the config allowlist (G-P0-1).
	{"checkpoint_timeout", "900", true, "base unit s"},
	{"checkpoint_timeout", "900s", true, "explicit seconds"},
	{"checkpoint_timeout", "2h", false, "above 1h"},
	{"min_wal_size", "4GB", true, "explicit GB"},
	{"min_wal_size", "1024", true, "1GB in MB"},
	{"min_wal_size", "16MB", false, "below 32MB"},
	{"autovacuum_analyze_scale_factor", "0.05", true, "ratio"},
	{"autovacuum_analyze_scale_factor", "0", false, "zero"},
	{"autovacuum_analyze_threshold", "50", true, "count"},
	{"autovacuum_vacuum_insert_scale_factor", "0.2", true, "ratio"},
	{"autovacuum_vacuum_insert_threshold", "1000", true, "count"},
	{"autovacuum_vacuum_insert_threshold", "-1", true, "disables insert vacuum"},
	{"autovacuum_naptime", "30s", true, "time"},
	{"autovacuum_naptime", "30", true, "base unit s"},
	{"autovacuum_naptime", "1d", false, "above 10min"},
	{"autovacuum_max_workers", "5", true, "count"},
	{"autovacuum_max_workers", "100", false, "above 20"},
}

func TestValidateGUCValue_BaseUnits(t *testing.T) {
	for _, c := range baseUnitCases {
		ok, reason := ValidateGUCValue(c.guc, c.value)
		if ok != c.wantOK {
			t.Errorf("ValidateGUCValue(%s=%q) = %v (%s), want %v: %s",
				c.guc, c.value, ok, reason, c.wantOK, c.why)
		}
	}
}

// Every documented GUC must be covered by the base-unit table so a new
// entry cannot ship without a unit decision.
func TestValidateGUCValue_EveryDocumentedGUCCovered(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range baseUnitCases {
		covered[c.guc] = true
	}
	for name := range pgconf.Docs {
		if !covered[name] {
			t.Errorf("pgconf.Docs[%q] has no base-unit test case", name)
		}
	}
}

func TestValidateConfigSQL_UnitlessBytesRejected(t *testing.T) {
	cases := []struct {
		sql    string
		wantOK bool
	}{
		{"ALTER SYSTEM SET work_mem = 268435456;", false},
		{"ALTER SYSTEM SET work_mem = '65536';", true},
		{"ALTER SYSTEM SET shared_buffers TO 16384;", true},
		{"ALTER SYSTEM SET max_wal_size = '2048';", true},
		// Reloptions are validated with the same documented ranges.
		{"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0);", false},
		{"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02, " +
			"autovacuum_vacuum_threshold = 1000);", true},
		{"ALTER TABLE public.orders SET (fillfactor = 90);", true},
	}
	for _, c := range cases {
		ok, reason := ValidateConfigSQL(c.sql)
		if ok != c.wantOK {
			t.Errorf("ValidateConfigSQL(%q) = %v (%s), want %v",
				c.sql, ok, reason, c.wantOK)
		}
	}
}

// End to end through the LLM parse path: the "256MB in bytes" answer
// must not become a finding.
func TestParseLLMFindings_DropsUnitlessBytesWorkMem(t *testing.T) {
	raw := `[{"object_identifier":"instance","severity":"warning",` +
		`"rationale":"256MB","recommended_sql":"ALTER SYSTEM SET work_mem = 268435456"}]`
	got := parseLLMFindings(raw, "memory_tuning", noopLog)
	if len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
}

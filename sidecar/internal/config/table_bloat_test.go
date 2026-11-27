package config

import (
	"strings"
	"testing"
)

// analyzer.table_bloat_min_bytes (vacuum-04): the least heap size for which
// the freeze custodian plans a bloat rebuild. Default 1 GiB; 0 (unset)
// means the default; a negative value is refused at load.
// No concurrency test: the accessor reads one immutable field.

func TestTableBloatMinBytes_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if DefaultTableBloatMinBytes != 1<<30 ||
		cfg.Analyzer.TableBloatMinBytes != DefaultTableBloatMinBytes ||
		cfg.Analyzer.BloatPlanMinBytes() != 1<<30 {
		t.Fatalf("default = %d (const %d, accessor %d), want 1 GiB",
			cfg.Analyzer.TableBloatMinBytes, DefaultTableBloatMinBytes,
			cfg.Analyzer.BloatPlanMinBytes())
	}
	if DefaultConfig().Analyzer.TableBloatMinBytes != DefaultTableBloatMinBytes {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestTableBloatMinBytes_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int64
	}{{"0", 1 << 30}, {"1", 1}, {"1073741823", 1<<30 - 1},
		{"10737418240", 10 << 30}} {
		cfg, err := loadRCAYAML(t, "analyzer:\n  table_bloat_min_bytes: "+tc.yaml+"\n")
		if err != nil {
			t.Fatalf("%s: Load: %v", tc.yaml, err)
		}
		if got := cfg.Analyzer.BloatPlanMinBytes(); got != tc.want {
			t.Errorf("%s: floor = %d, want %d", tc.yaml, got, tc.want)
		}
	}
}

func TestTableBloatMinBytes_ZeroValueStructUsesDefault(t *testing.T) {
	var analyzer AnalyzerConfig
	if got := analyzer.BloatPlanMinBytes(); got != DefaultTableBloatMinBytes {
		t.Fatalf("zero-value floor = %d, want the 1 GiB default", got)
	}
}

func TestTableBloatMinBytes_NegativeRefused(t *testing.T) {
	_, err := loadRCAYAML(t, "analyzer:\n  table_bloat_min_bytes: -1\n")
	if err == nil || !strings.Contains(err.Error(), "analyzer.table_bloat_min_bytes") {
		t.Fatalf("err = %v, want a refusal naming the key", err)
	}
}

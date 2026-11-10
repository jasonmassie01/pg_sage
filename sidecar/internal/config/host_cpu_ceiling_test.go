package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// collection-08: safety.cpu_ceiling_pct is the collector's session-load
// ceiling; verify's host-CPU admission has its own key, which inherits
// an explicit cpu_ceiling_pct (with a warning) so no guardrail loosens on
// upgrade.

func hostCPUWarnings(out *bytes.Buffer) int {
	return strings.Count(out.String(), "WARN: safety.host_cpu_ceiling_pct is unset")
}

func TestHostCPUCeilingDefaultWithoutConfigFile(t *testing.T) {
	out := captureConfigWarnings(t)
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Safety.EffectiveHostCPUCeilingPct(); got != 90 {
		t.Errorf("effective host_cpu_ceiling_pct = %d, want 90", got)
	}
	if cfg.Safety.CPUCeilingPct != 90 {
		t.Errorf("cpu_ceiling_pct = %d, want 90", cfg.Safety.CPUCeilingPct)
	}
	if n := hostCPUWarnings(out); n != 0 {
		t.Errorf("defaults warned %d times: %q", n, out.String())
	}
	if got := DefaultConfig().Safety.EffectiveHostCPUCeilingPct(); got != 90 {
		t.Errorf("DefaultConfig host ceiling = %d, want 90", got)
	}
}

func TestHostCPUCeilingInheritsExplicitSessionCeiling(t *testing.T) {
	out := captureConfigWarnings(t)
	cfg := loadFromYAML(t, standaloneIOYAML+"safety:\n  cpu_ceiling_pct: 70\n")
	if got := cfg.Safety.EffectiveHostCPUCeilingPct(); got != 70 {
		t.Errorf("effective host ceiling = %d, want 70 (inherited)", got)
	}
	if n := hostCPUWarnings(out); n != 1 {
		t.Errorf("inheritance warned %d times, want 1: %q", n, out.String())
	}
	if !strings.Contains(out.String(), "WARN") || !strings.Contains(out.String(), "70") {
		t.Errorf("warning %q does not say WARN and the inherited value", out.String())
	}
}

func TestHostCPUCeilingExplicitKeyWins(t *testing.T) {
	out := captureConfigWarnings(t)
	cfg := loadFromYAML(t, standaloneIOYAML+
		"safety:\n  cpu_ceiling_pct: 70\n  host_cpu_ceiling_pct: 85\n")
	if got := cfg.Safety.EffectiveHostCPUCeilingPct(); got != 85 {
		t.Errorf("effective host ceiling = %d, want 85", got)
	}
	if cfg.Safety.CPUCeilingPct != 70 {
		t.Errorf("cpu_ceiling_pct = %d, want 70 (independent)", cfg.Safety.CPUCeilingPct)
	}
	if n := hostCPUWarnings(out); n != 0 {
		t.Errorf("an explicit host key warned %d times: %q", n, out.String())
	}
}

func TestHostCPUCeilingOnlyHostKeyLeavesSessionCeiling(t *testing.T) {
	cfg := loadFromYAML(t, standaloneIOYAML+"safety:\n  host_cpu_ceiling_pct: 60\n")
	if cfg.Safety.CPUCeilingPct != 90 || cfg.Safety.EffectiveHostCPUCeilingPct() != 60 {
		t.Errorf("session/host = %d/%d, want 90/60", cfg.Safety.CPUCeilingPct,
			cfg.Safety.EffectiveHostCPUCeilingPct())
	}
}

func TestHostCPUCeilingValidation(t *testing.T) {
	for _, pct := range []string{"-1", "101", "1000"} {
		_, err := loadYAMLResult(t, standaloneIOYAML+"safety:\n  host_cpu_ceiling_pct: "+pct+"\n")
		if err == nil || !strings.Contains(err.Error(), "safety.host_cpu_ceiling_pct") {
			t.Errorf("host_cpu_ceiling_pct %s: err = %v, want a range error naming the key",
				pct, err)
		}
	}
	for _, pct := range []string{"1", "100"} {
		cfg, err := loadYAMLResult(t, standaloneIOYAML+"safety:\n  host_cpu_ceiling_pct: "+pct+"\n")
		if err != nil {
			t.Errorf("host_cpu_ceiling_pct %s rejected: %v", pct, err)
			continue
		}
		if got := cfg.Safety.EffectiveHostCPUCeilingPct(); pct == "1" && got != 1 ||
			pct == "100" && got != 100 {
			t.Errorf("host_cpu_ceiling_pct %s: effective %d", pct, got)
		}
	}
}

func TestEffectiveHostCPUCeilingZeroValues(t *testing.T) {
	cases := []struct {
		name      string
		host, cpu int
		want      int
	}{
		// No ceiling configured at all: verify keeps its own (stricter) default.
		{"both unset", 0, 0, 0},
		{"session only", 0, 75, 75},
		{"host only", 65, 0, 65},
		{"both", 55, 75, 55},
	}
	for _, tc := range cases {
		s := SafetyConfig{HostCPUCeilingPct: tc.host, CPUCeilingPct: tc.cpu}
		if got := s.EffectiveHostCPUCeilingPct(); got != tc.want {
			t.Errorf("%s: effective = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCPUCeilingDocNamesSessionLoad(t *testing.T) {
	cpu, ok := fieldDoc("safety.cpu_ceiling_pct")
	if !ok || !strings.Contains(cpu, "active client sessions / max_connections") ||
		strings.Contains(cpu, "host CPU") || strings.Contains(cpu, "database-host CPU") {
		t.Errorf("cpu_ceiling_pct doc = %q, want the session-load meaning", cpu)
	}
	host, ok := fieldDoc("safety.host_cpu_ceiling_pct")
	if !ok || !strings.Contains(host, "host CPU") {
		t.Errorf("host_cpu_ceiling_pct doc = %q, want the host CPU meaning", host)
	}
}

// fieldDoc is the doc tag of the safety.<key> field.
func fieldDoc(path string) (string, bool) {
	key := strings.TrimPrefix(path, "safety.")
	st := reflect.TypeOf(SafetyConfig{})
	for i := 0; i < st.NumField(); i++ {
		if strings.Split(st.Field(i).Tag.Get("yaml"), ",")[0] == key {
			return st.Field(i).Tag.Get("doc"), true
		}
	}
	return "", false
}

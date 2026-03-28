package store

import (
	"testing"
)

func TestValidateConfigKey(t *testing.T) {
	tests := []struct {
		key     string
		wantErr bool
	}{
		{"collector.interval_seconds", false},
		{"trust.level", false},
		{"llm.enabled", false},
		{"retention.snapshots_days", false},
		{"nonexistent.key", true},
		{"", true},
		{"drop table", true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			err := validateConfigKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfigKey(%q) error = %v, wantErr %v",
					tt.key, err, tt.wantErr)
			}
		})
	}
}

func TestValidateConfigValue(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"valid int", "collector.interval_seconds", "60", false},
		{"int too low", "collector.interval_seconds", "3", true},
		{"int negative", "collector.batch_size", "-1", true},
		{"int zero for pos", "collector.batch_size", "0", true},
		{"valid nonneg", "analyzer.slow_query_threshold_ms", "0", false},
		{"valid pct", "analyzer.index_bloat_threshold_pct", "50", false},
		{"pct too high", "analyzer.index_bloat_threshold_pct", "101", true},
		{"pct negative", "analyzer.index_bloat_threshold_pct", "-1", true},
		{"valid pct1", "safety.cpu_ceiling_pct", "90", false},
		{"pct1 zero", "safety.cpu_ceiling_pct", "0", true},
		{"valid float", "analyzer.cache_hit_ratio_warning", "0.95", false},
		{"float too high", "analyzer.cache_hit_ratio_warning", "1.5", true},
		{"float negative", "analyzer.cache_hit_ratio_warning", "-0.1", true},
		{"valid bool true", "llm.enabled", "true", false},
		{"valid bool false", "llm.enabled", "false", false},
		{"invalid bool", "llm.enabled", "yes", true},
		{"valid trust", "trust.level", "advisory", false},
		{"invalid trust", "trust.level", "invalid", true},
		{"valid exec mode", "execution_mode", "auto", false},
		{"invalid exec mode", "execution_mode", "yolo", true},
		{"valid string", "llm.endpoint", "https://api.example.com", false},
		{"empty string ok", "llm.endpoint", "", false},
		{"not a number", "collector.interval_seconds", "abc", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfigValue(tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfigValue(%q, %q) error = %v, wantErr %v",
					tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"abc", "***"},
		{"abcd", "****"},
		{"sk-12345678", "*******5678"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := maskSecret(tt.input)
			if got != tt.want {
				t.Errorf("maskSecret(%q) = %q, want %q",
					tt.input, got, tt.want)
			}
		})
	}
}

func TestCoerceValue(t *testing.T) {
	tests := []struct {
		key   string
		value string
		want  any
	}{
		{"collector.interval_seconds", "60", 60},
		{"analyzer.cache_hit_ratio_warning", "0.95", 0.95},
		{"llm.enabled", "true", true},
		{"llm.enabled", "false", false},
		{"llm.endpoint", "https://api.example.com", "https://api.example.com"},
		{"trust.level", "advisory", "advisory"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			got := coerceValue(tt.key, tt.value)
			switch v := tt.want.(type) {
			case int:
				if gi, ok := got.(int); !ok || gi != v {
					t.Errorf("coerceValue = %v (%T), want %v", got, got, v)
				}
			case float64:
				if gf, ok := got.(float64); !ok || gf != v {
					t.Errorf("coerceValue = %v (%T), want %v", got, got, v)
				}
			case bool:
				if gb, ok := got.(bool); !ok || gb != v {
					t.Errorf("coerceValue = %v (%T), want %v", got, got, v)
				}
			case string:
				if gs, ok := got.(string); !ok || gs != v {
					t.Errorf("coerceValue = %v (%T), want %v", got, got, v)
				}
			}
		})
	}
}

func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty empty = %v, want nil", got)
	}
	if got := nullIfEmpty("hello"); got == nil || *got != "hello" {
		t.Errorf("nullIfEmpty non-empty = %v, want hello", got)
	}
}

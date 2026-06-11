package advisor

import (
	"strings"
	"testing"
)

func TestDocContext_IncludesDocs(t *testing.T) {
	ctx := DocContext(0, "work_mem", "random_page_cost", "unknown_guc")
	if !strings.Contains(ctx, "work_mem") || !strings.Contains(ctx, "Safe range") {
		t.Errorf("missing work_mem doc:\n%s", ctx)
	}
	if !strings.Contains(ctx, "random_page_cost") {
		t.Errorf("missing random_page_cost doc")
	}
	if strings.Contains(ctx, "unknown_guc") {
		t.Error("unknown GUC should be skipped")
	}
}

func TestValidateGUCValue(t *testing.T) {
	cases := []struct {
		guc, value string
		wantOK     bool
	}{
		{"work_mem", "256MB", true},
		{"work_mem", "8GB", false},          // > 2GB safe max
		{"work_mem", "1MB", false},          // < 4MB safe min
		{"random_page_cost", "1.1", true},
		{"random_page_cost", "0.1", false},  // < 1.0
		{"random_page_cost", "10", false},   // > 4.0
		{"checkpoint_completion_target", "0.9", true},
		{"default_statistics_target", "500", true},
		{"default_statistics_target", "5000", false},
		{"unknown_guc", "whatever", true},   // unknown passes
		{"shared_buffers", "8GB", true},
	}
	for _, c := range cases {
		ok, _ := ValidateGUCValue(c.guc, c.value)
		if ok != c.wantOK {
			t.Errorf("ValidateGUCValue(%s,%s) = %v, want %v", c.guc, c.value, ok, c.wantOK)
		}
	}
}

func TestValidateConfigSQL(t *testing.T) {
	cases := []struct {
		sql    string
		wantOK bool
	}{
		{"ALTER SYSTEM SET work_mem = '256MB';", true},
		{"ALTER SYSTEM SET work_mem = '8GB';", false},
		{"ALTER SYSTEM SET random_page_cost = 1.1;", true},
		{"ALTER SYSTEM SET random_page_cost TO 0.1;", false},
		{"VACUUM public.orders;", true}, // non-config passes
		{"", true},
	}
	for _, c := range cases {
		ok, _ := ValidateConfigSQL(c.sql)
		if ok != c.wantOK {
			t.Errorf("ValidateConfigSQL(%q) = %v, want %v", c.sql, ok, c.wantOK)
		}
	}
}

func TestParseMemoryToBytes(t *testing.T) {
	cases := map[string]float64{"256MB": 256 << 20, "1GB": 1 << 30, "512kB": 512 << 10, "1024": 1024}
	for in, want := range cases {
		got, err := parseMemoryToBytes(in)
		if err != nil || got != want {
			t.Errorf("parseMemoryToBytes(%q) = %v,%v want %v", in, got, err, want)
		}
	}
}

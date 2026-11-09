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
		{"work_mem", "8GB", false}, // > 2GB safe max
		{"work_mem", "1MB", false}, // < 4MB safe min
		{"random_page_cost", "1.1", true},
		{"random_page_cost", "0.1", false}, // < 1.0
		{"random_page_cost", "10", false},  // > 4.0
		{"checkpoint_completion_target", "0.9", true},
		{"default_statistics_target", "500", true},
		{"default_statistics_target", "5000", false},
		{"unknown_guc", "whatever", true}, // unknown passes
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

// advisors-06: no documented GUC means no documentation block at all.
func TestDocContext_NoKnownGUCIsEmpty(t *testing.T) {
	for _, gucs := range [][]string{nil, {}, {"wal_level", "no_such_guc"}} {
		if got := DocContext(17, gucs...); got != "" {
			t.Errorf("DocContext(17, %v) = %q, want empty", gucs, got)
		}
	}
}

// Version notes appear only on the majors they are verified for.
func TestDocContext_VersionNotesFollowTheMajor(t *testing.T) {
	cases := []struct {
		guc, note string
		on, off   []int
	}{
		{"effective_io_concurrency", "raised the default to 16", []int{18, 19},
			[]int{0, 14, 17}},
		{"autovacuum_max_workers", "autovacuum_worker_slots", []int{18}, []int{0, 17}},
		{"max_wal_size", "pg_stat_checkpointer", []int{17, 18}, []int{0, 14, 16}},
	}
	for _, c := range cases {
		for _, major := range c.on {
			got := DocContext(major, c.guc)
			if !strings.Contains(got, c.note) || !strings.Contains(got, "Version note") {
				t.Errorf("PG%d %s lacks note %q:\n%s", major, c.guc, c.note, got)
			}
		}
		for _, major := range c.off {
			got := DocContext(major, c.guc)
			if strings.Contains(got, c.note) || strings.Contains(got, "Version note") {
				t.Errorf("PG%d %s carries a note it must not:\n%s", major, c.guc, got)
			}
			if !strings.Contains(got, c.guc+":") {
				t.Errorf("PG%d %s lost its doc entry:\n%s", major, c.guc, got)
			}
		}
	}
}

package verify

import (
	"strings"
	"testing"
)

func TestDataIOQuerySelectsSourceByServerVersion(t *testing.T) {
	cases := []struct {
		version  int
		source   string
		contains string
		excludes string
	}{
		{140000, IOSourcePGStatDatabase, "pg_stat_bgwriter", "pg_stat_io"},
		{150013, IOSourcePGStatDatabase, "buffers_backend", "pg_stat_io"},
		{160000, IOSourcePGStatIO, "op_bytes", "read_bytes"},
		{170006, IOSourcePGStatIO, "op_bytes", "read_bytes"},
		{180000, IOSourcePGStatIO, "read_bytes", "op_bytes"},
	}
	for _, test := range cases {
		query, source, err := dataIOQuery(test.version)
		if err != nil || source != test.source {
			t.Fatalf("version %d: source=%q err=%v", test.version, source, err)
		}
		if !strings.Contains(query, test.contains) || strings.Contains(query, test.excludes) {
			t.Fatalf("version %d query does not match its catalog: %s", test.version, query)
		}
	}
	for _, version := range []int{0, 90600, 130014} {
		if _, _, err := dataIOQuery(version); err == nil ||
			!strings.Contains(err.Error(), "PostgreSQL 14") {
			t.Fatalf("version %d accepted without pg_stat_wal: %v", version, err)
		}
	}
}

func TestPGStatIOQueryExcludesWALObjectRows(t *testing.T) {
	for _, version := range []int{160000, 180000} {
		query, _, err := dataIOQuery(version)
		if err != nil || !strings.Contains(query, "'relation'") ||
			!strings.Contains(query, "'temp relation'") {
			t.Fatalf("version %d must count only relation IO: %s %v", version, query, err)
		}
	}
}

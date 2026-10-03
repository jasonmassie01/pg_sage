package partition

import (
	"testing"
	"time"
)

func TestDayStartIsUTCMidnight(t *testing.T) {
	east := time.FixedZone("UTC+10", 10*3600)
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 10, 3, 23, 59, 59, 999, time.UTC),
			time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		// 08:00 on the 4th at UTC+10 is 22:00 on the 3rd in UTC.
		{time.Date(2026, 10, 4, 8, 0, 0, 0, east), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := DayStart(c.in); !got.Equal(c.want) || got.Location() != time.UTC {
			t.Errorf("DayStart(%s) = %s, want %s UTC", c.in, got, c.want)
		}
	}
}

func TestPartitionNamesAreDailyAndParseBack(t *testing.T) {
	day := time.Date(2026, 1, 9, 17, 4, 0, 0, time.UTC)
	if got := QueryStore.DayName(day); got != "query_store_p20260109" {
		t.Fatalf("DayName = %q", got)
	}
	if got := Snapshots.HistoryName(); got != "snapshots_history" {
		t.Fatalf("HistoryName = %q", got)
	}
	if got := Snapshots.DefaultName(); got != "snapshots_default" {
		t.Fatalf("DefaultName = %q", got)
	}
	got, ok := QueryStore.parseDay("query_store_p20260109")
	if !ok || !got.Equal(time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("parseDay = %s %v", got, ok)
	}
	for _, bad := range []string{"query_store_p2026010", "query_store_p20261340",
		"snapshots_p20260109", "query_store_history", "query_store_default", "query_store_pabcdefgh", ""} {
		if _, ok := QueryStore.parseDay(bad); ok {
			t.Errorf("parseDay(%q) accepted", bad)
		}
	}
}

func TestParseUpperBound(t *testing.T) {
	cases := map[string]time.Time{
		"FOR VALUES FROM (MINVALUE) TO ('2026-10-04 00:00:00+00')": time.Date(2026, 10, 4,
			0, 0, 0, 0, time.UTC),
		"FOR VALUES FROM ('2026-10-03 00:00:00+00') TO ('2026-10-04 00:00:00+00')": time.Date(
			2026, 10, 4, 0, 0, 0, 0, time.UTC),
		"FOR VALUES FROM (MINVALUE) TO ('2026-10-04 02:00:00+02')": time.Date(2026, 10, 4,
			0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseUpperBound(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseUpperBound(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "DEFAULT", "FOR VALUES FROM (MINVALUE) TO (MAXVALUE)",
		"FOR VALUES IN (1)", "FOR VALUES FROM (MINVALUE) TO ('not a time')"} {
		if _, err := parseUpperBound(bad); err == nil {
			t.Errorf("parseUpperBound(%q) accepted", bad)
		}
	}
}

func TestTablesAreKnownHistoryTables(t *testing.T) {
	if QueryStore.Name != "query_store" || QueryStore.Column != "captured_at" {
		t.Fatalf("QueryStore = %+v", QueryStore)
	}
	if Snapshots.Name != "snapshots" || Snapshots.Column != "collected_at" {
		t.Fatalf("Snapshots = %+v", Snapshots)
	}
	if got := len(HistoryTables()); got != 2 {
		t.Fatalf("HistoryTables = %d, want query_store and snapshots", got)
	}
}

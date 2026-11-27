package config

import (
	"strconv"
	"strings"
	"testing"
)

// market-13: analyzer.subscription_lag_minutes (5), analyzer.cdc_stall_minutes
// (10) and replication.cdc_heartbeats (empty). A heartbeat names a table and a
// column pg_sage reads with one SELECT max(col): an identifier that is not a
// plain name is refused at load, never interpolated.
//
// No concurrent access tests: Load builds a fresh Config per call and shares
// nothing.

func TestReplicationKeys_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.SubscriptionLagMinutes != 5 || DefaultSubscriptionLagMinutes != 5 {
		t.Errorf("subscription_lag_minutes = %d (const %d), want 5",
			cfg.Analyzer.SubscriptionLagMinutes, DefaultSubscriptionLagMinutes)
	}
	if cfg.Analyzer.CDCStallMinutes != 10 || DefaultCDCStallMinutes != 10 {
		t.Errorf("cdc_stall_minutes = %d (const %d), want 10",
			cfg.Analyzer.CDCStallMinutes, DefaultCDCStallMinutes)
	}
	if len(cfg.Replication.CDCHeartbeats) != 0 {
		t.Errorf("cdc_heartbeats = %+v, want none by default", cfg.Replication.CDCHeartbeats)
	}
	d := DefaultConfig()
	if d.Analyzer.SubscriptionLagMinutes != 5 || d.Analyzer.CDCStallMinutes != 10 {
		t.Errorf("DefaultConfig disagrees with Load(nil): %d, %d",
			d.Analyzer.SubscriptionLagMinutes, d.Analyzer.CDCStallMinutes)
	}
}

func TestReplicationKeys_PartialAnalyzerSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "analyzer:\n  slow_query_threshold_ms: 500\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.SubscriptionLagMinutes != 5 || cfg.Analyzer.CDCStallMinutes != 10 {
		t.Fatalf("got %d, %d; want the defaults 5 and 10 kept",
			cfg.Analyzer.SubscriptionLagMinutes, cfg.Analyzer.CDCStallMinutes)
	}
}

func TestReplicationKeys_ExplicitAndBoundaryValues(t *testing.T) {
	for _, v := range []int{0, 1, 60, 10080} {
		body := "analyzer:\n  subscription_lag_minutes: " + strconv.Itoa(v) +
			"\n  cdc_stall_minutes: " + strconv.Itoa(v) + "\n"
		cfg, err := loadRCAYAML(t, body)
		if err != nil {
			t.Fatalf("%d: Load: %v", v, err)
		}
		if cfg.Analyzer.SubscriptionLagMinutes != v || cfg.Analyzer.CDCStallMinutes != v {
			t.Errorf("%d: got %d, %d", v, cfg.Analyzer.SubscriptionLagMinutes,
				cfg.Analyzer.CDCStallMinutes)
		}
	}
}

func TestReplicationKeys_OutOfRangeRefused(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"subscription_lag_minutes", "-1"}, {"subscription_lag_minutes", "10081"},
		{"cdc_stall_minutes", "-1"}, {"cdc_stall_minutes", "10081"},
	} {
		_, err := loadRCAYAML(t, "analyzer:\n  "+tc.key+": "+tc.val+"\n")
		if err == nil || !strings.Contains(err.Error(), "analyzer."+tc.key) {
			t.Errorf("%s=%s: err = %v, want a refusal naming the key", tc.key, tc.val, err)
		}
	}
}

func heartbeatYAML(slot, table, column, maxAge string) string {
	return "replication:\n  cdc_heartbeats:\n    - slot: " + slot +
		"\n      table: " + table + "\n      column: " + column +
		"\n      max_age_minutes: " + maxAge + "\n"
}

func TestCDCHeartbeats_ValidEntryLoads(t *testing.T) {
	cfg, err := loadRCAYAML(t, heartbeatYAML("debezium", "cdc.heartbeat", "ts", "5"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hb := cfg.Replication.CDCHeartbeats
	if len(hb) != 1 || hb[0].Slot != "debezium" || hb[0].Table != "cdc.heartbeat" ||
		hb[0].Column != "ts" || hb[0].MaxAgeMinutes != 5 {
		t.Fatalf("heartbeats = %+v, want the one entry as written", hb)
	}
	cfg, err = loadRCAYAML(t, heartbeatYAML("s1", "heartbeat", "beat_at", "1"))
	if err != nil || cfg.Replication.CDCHeartbeats[0].Table != "heartbeat" {
		t.Fatalf("unqualified table: %+v, %v; want it accepted", cfg, err)
	}
}

// Invalid input: a table or column that is not a plain identifier (a quote,
// a semicolon, a space, three parts) is refused at load, naming the entry.
func TestCDCHeartbeats_InvalidIdentifiersRefused(t *testing.T) {
	for _, tc := range []struct{ table, column, want string }{
		{`'cdc."hb"'`, "ts", "table"},
		{`'cdc.hb; DROP TABLE x'`, "ts", "table"},
		{`'a.b.c'`, "ts", "table"},
		{`'cdc. hb'`, "ts", "table"},
		{`''`, "ts", "table"},
		{"cdc.hb", `'ts"'`, "column"},
		{"cdc.hb", `'ts;'`, "column"},
		{"cdc.hb", `''`, "column"},
	} {
		_, err := loadRCAYAML(t, heartbeatYAML("s1", tc.table, tc.column, "5"))
		if err == nil || !strings.Contains(err.Error(), "replication.cdc_heartbeats[0]") ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("table=%s column=%s: err = %v, want a refusal naming %s",
				tc.table, tc.column, err, tc.want)
		}
	}
}

func TestCDCHeartbeats_SlotAndMaxAgeRequired(t *testing.T) {
	for _, tc := range []struct{ slot, maxAge, want string }{
		{`''`, "5", "slot"},
		{"s1", "0", "max_age_minutes"},
		{"s1", "-3", "max_age_minutes"},
		{"s1", "10081", "max_age_minutes"},
	} {
		_, err := loadRCAYAML(t, heartbeatYAML(tc.slot, "cdc.hb", "ts", tc.maxAge))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("slot=%s max_age=%s: err = %v, want a refusal naming %s",
				tc.slot, tc.maxAge, err, tc.want)
		}
	}
}

func TestCDCHeartbeats_DuplicateSlotRefused(t *testing.T) {
	body := "replication:\n  cdc_heartbeats:\n" +
		"    - {slot: s1, table: a.hb, column: ts, max_age_minutes: 5}\n" +
		"    - {slot: s1, table: b.hb, column: ts, max_age_minutes: 5}\n"
	_, err := loadRCAYAML(t, body)
	if err == nil || !strings.Contains(err.Error(), "replication.cdc_heartbeats[1]") ||
		!strings.Contains(err.Error(), "s1") {
		t.Fatalf("err = %v, want the second entry for slot s1 refused", err)
	}
}

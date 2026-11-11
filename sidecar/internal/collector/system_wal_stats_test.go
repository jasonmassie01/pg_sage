package collector

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// advisors-05: the system snapshot carries the cluster's pg_stat_wal
// counter with its epoch, so the WAL advisor can difference a day of it.

// No concurrent access tests: these are pure SQL-text and JSON checks.

func TestSystemStats_WALStatsJSONRoundTrip(t *testing.T) {
	epoch := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	in := SystemStats{WALBytes: 12 << 30, WALStatsEpoch: epoch,
		CheckpointsRequested: 30, CheckpointsTimed: 2}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"cluster_wal_bytes":12884901888`,
		`"wal_stats_epoch":"2026-11-10T08:00:00Z"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON %s lacks %s", raw, key)
		}
	}
	var out SystemStats
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.WALBytes != 12<<30 || !out.WALStatsEpoch.Equal(epoch) ||
		out.CheckpointsRequested != 30 || out.CheckpointsTimed != 2 {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

// A legacy row (before advisors-05) decodes to an unknown WAL counter:
// zero bytes and a zero epoch, which the advisor reads as "unknown".
func TestSystemStats_LegacyRowHasNoWALStats(t *testing.T) {
	var legacy SystemStats
	if err := json.Unmarshal([]byte(`{"total_checkpoints":40,"wal_lsn":99}`),
		&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.WALBytes != 0 || !legacy.WALStatsEpoch.IsZero() || legacy.WALLSN != 99 {
		t.Errorf("legacy row = %+v, want no WAL stats and the LSN kept", legacy)
	}
	raw, err := json.Marshal(SystemStats{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cluster_wal_bytes") ||
		strings.Contains(string(raw), "wal_stats_epoch") {
		t.Errorf("unknown WAL stats are written as values: %s", raw)
	}
}

// The checkpoint counters split at PostgreSQL 17 (pg_stat_bgwriter's
// checkpoints_req/checkpoints_timed before, pg_stat_checkpointer's
// num_requested/num_timed from 17); no variant reads buffers_backend.
func TestSystemStatsSQL_CheckpointColumnsByVersion(t *testing.T) {
	cases := []struct {
		version   int
		want, not string
	}{
		{140000, "v.checkpoints_req AS checkpoints_requested", "num_requested"},
		{160000, "v.checkpoints_req AS checkpoints_requested", "num_requested"},
		{170000, "v.num_requested AS checkpoints_requested", "checkpoints_req "},
		{180000, "v.num_requested AS checkpoints_requested", "checkpoints_req "},
	}
	for _, c := range cases {
		sql := systemStatsSQLFor(c.version)
		if !strings.Contains(sql, c.want) || strings.Contains(sql, c.not) {
			t.Errorf("version %d: SQL lacks %q or has %q", c.version, c.want, c.not)
		}
		if strings.Contains(sql, "buffers_backend") {
			t.Errorf("version %d reads buffers_backend", c.version)
		}
	}
	if !strings.Contains(walStatsSQL, "pg_stat_wal") ||
		!strings.Contains(walStatsSQL, "stats_reset") {
		t.Errorf("walStatsSQL = %q, want pg_stat_wal's wal_bytes and stats_reset",
			walStatsSQL)
	}
}

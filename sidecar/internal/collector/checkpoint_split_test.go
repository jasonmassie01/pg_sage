package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// forecast-03: the system snapshot keeps the timed and requested
// checkpoint counters apart (timed ones are counted every
// checkpoint_timeout even when skipped), and on PG 18+ the performed
// count, num_done. total_checkpoints stays for its existing readers.

// No concurrent access tests: collectSystem reads one row per call and
// the SQL selection is a pure function of the version.

func TestSystemStatsSQL_CheckpointSplit(t *testing.T) {
	for name, tc := range map[string]struct {
		sql     string
		want    []string
		mustNot []string
	}{
		"pg14": {systemStatsSQL14, []string{"checkpoints_timed AS checkpoints_timed",
			"checkpoints_req AS checkpoints_requested"}, []string{"num_done"}},
		"pg17": {systemStatsSQL17, []string{"num_timed AS checkpoints_timed",
			"num_requested AS checkpoints_requested"}, []string{"num_done"}},
		"pg18": {systemStatsSQL18, []string{"num_timed AS checkpoints_timed",
			"num_requested AS checkpoints_requested", "num_done"}, nil},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.sql, w) {
				t.Errorf("%s: SQL does not select %q", name, w)
			}
		}
		for _, w := range tc.mustNot {
			if strings.Contains(tc.sql, w) {
				t.Errorf("%s: SQL selects %q, which this version lacks", name, w)
			}
		}
		if !strings.Contains(tc.sql, "AS total_checkpoints") {
			t.Errorf("%s: total_checkpoints is gone; its readers need it", name)
		}
		if got := strings.Contains(tc.sql, "AS checkpoints_done"); !got {
			t.Errorf("%s: checkpoints_done column missing (want NULL before PG 18)", name)
		}
	}
}

func TestSystemStatsSQLFor_PicksTheVersionsView(t *testing.T) {
	for _, tc := range []struct {
		version int
		want    string
	}{
		{0, systemStatsSQL14}, {140000, systemStatsSQL14}, {160009, systemStatsSQL14},
		{170000, systemStatsSQL17}, {170010, systemStatsSQL17},
		{180000, systemStatsSQL18}, {190000, systemStatsSQL18},
	} {
		if got := systemStatsSQLFor(tc.version); got != tc.want {
			t.Errorf("version %d: picked the wrong system stats SQL", tc.version)
		}
	}
}

func TestSystemStats_CheckpointSplitJSON(t *testing.T) {
	raw, err := json.Marshal(SystemStats{TotalCheckpoints: 7, CheckpointsTimed: 3,
		CheckpointsRequested: 4})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"total_checkpoints":7`, `"checkpoints_timed":3`,
		`"checkpoints_requested":4`} {
		if !strings.Contains(s, want) {
			t.Errorf("snapshot JSON %s lacks %s", s, want)
		}
	}
	if strings.Contains(s, "checkpoints_done") {
		t.Errorf("an unread num_done is written: %s", s)
	}
	done := int64(5)
	raw, _ = json.Marshal(SystemStats{CheckpointsDone: &done})
	if !strings.Contains(string(raw), `"checkpoints_done":5`) {
		t.Errorf("num_done is not written: %s", raw)
	}
	var back SystemStats
	if err := json.Unmarshal([]byte(`{"total_checkpoints":9}`), &back); err != nil ||
		back.CheckpointsDone != nil || back.TotalCheckpoints != 9 {
		t.Errorf("a legacy row reads back as %+v (%v)", back, err)
	}
}

// The live counters: an explicit CHECKPOINT is a requested checkpoint on
// every version, so swapped or summed columns show up here. Run against
// PG 14-16 (pg_stat_bgwriter) and PG 17+ (pg_stat_checkpointer).
func TestCollectSystem_CheckpointSplitFromServer(t *testing.T) {
	pool := exclusivePool(t, "requested checkpoints across a forced CHECKPOINT")
	version := serverVersion(t, pool)
	c := New(pool, testConfig(), version, noopLog)
	ctx := context.Background()
	before, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if _, err := pool.Exec(ctx, "CHECKPOINT"); err != nil {
		t.Fatalf("CHECKPOINT: %v", err)
	}
	after := systemAfterCheckpoint(t, ctx, c, before.CheckpointsRequested)
	if after.CheckpointsRequested < before.CheckpointsRequested+1 {
		t.Fatalf("requested %d -> %d across a CHECKPOINT", before.CheckpointsRequested,
			after.CheckpointsRequested)
	}
	if after.CheckpointsTimed < before.CheckpointsTimed {
		t.Fatalf("timed went back %d -> %d", before.CheckpointsTimed, after.CheckpointsTimed)
	}
	if after.TotalCheckpoints != after.CheckpointsTimed+after.CheckpointsRequested {
		t.Fatalf("total %d != timed %d + requested %d", after.TotalCheckpoints,
			after.CheckpointsTimed, after.CheckpointsRequested)
	}
	switch {
	case version < 180000 && after.CheckpointsDone != nil:
		t.Fatalf("PG %d has no num_done, got %d", version, *after.CheckpointsDone)
	case version >= 180000 && (after.CheckpointsDone == nil || before.CheckpointsDone == nil ||
		*after.CheckpointsDone < *before.CheckpointsDone+1):
		t.Fatalf("num_done did not count the CHECKPOINT: %v -> %v",
			before.CheckpointsDone, after.CheckpointsDone)
	}
}

// systemAfterCheckpoint reads the system snapshot until the requested
// count passes requested, for at most 10 s: PostgreSQL 14 reports the
// checkpointer's counters through the statistics collector, asynchronously,
// so the first read after a CHECKPOINT can still show the old count.
func systemAfterCheckpoint(t *testing.T, ctx context.Context, c *Collector,
	requested int64) SystemStats {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		after, err := c.collectSystem(ctx)
		if err != nil {
			t.Fatalf("collectSystem: %v", err)
		}
		if after.CheckpointsRequested > requested || time.Now().After(deadline) {
			return after
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A server that reports 18 but lacks num_done (a fork, or a view an
// extension replaced) falls back to the PG 17 columns.
func TestCollectSystem_MissingNumDoneFallsBack(t *testing.T) {
	pool := testPool(t)
	version := serverVersion(t, pool)
	if version < 170000 || version >= 180000 {
		t.Skipf("needs PG 17 (pg_stat_checkpointer without num_done), server is %d", version)
	}
	c := New(pool, testConfig(), 180000, noopLog)
	s, err := c.collectSystem(context.Background())
	if err != nil {
		t.Fatalf("collectSystem without num_done: %v", err)
	}
	if s.CheckpointsDone != nil || s.TotalCheckpoints != s.CheckpointsTimed+
		s.CheckpointsRequested || s.MaxConnections <= 0 {
		t.Fatalf("fallback read %+v", s)
	}
}

func TestSystemStatsFallback(t *testing.T) {
	missingView := &pgconn.PgError{Code: "42P01"}
	missingColumn := &pgconn.PgError{Code: "42703"}
	for _, tc := range []struct {
		version int
		err     error
		want    string
	}{
		{170000, missingView, systemStatsSQL14},
		{180000, missingView, systemStatsSQL14},
		{180000, missingColumn, systemStatsSQL17},
		{180000, fmt.Errorf("read system stats: %w", missingColumn), systemStatsSQL17},
		{170000, missingColumn, ""}, // PG 17 SQL reads no PG 18 column
		{160000, missingView, ""},   // pg_stat_bgwriter is the oldest read
		{180000, &pgconn.PgError{Code: "57014"}, ""},
		{180000, errors.New("connection reset"), ""},
		{180000, nil, ""},
	} {
		if got := systemStatsFallback(tc.version, tc.err); got != tc.want {
			t.Errorf("version %d, %v: wrong fallback", tc.version, tc.err)
		}
	}
}

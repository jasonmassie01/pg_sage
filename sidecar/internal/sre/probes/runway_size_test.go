package probes

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Fleet WAL-runway dedupe: the databases' total size moved out of
// wal_runway (read on every pass of every fleet runtime) into its own
// cluster_database_size probe, measured once per cluster per pass.
// wal_runway now names the cluster (system identifier, postmaster start,
// role) so runtimes on one cluster can share the measurement.

func TestCatalog_WALRunwayNoLongerSumsDatabaseSizes(t *testing.T) {
	spec, ok := Catalog().Spec(WALRunwayProbe)
	if !ok || spec.Version != "v2" {
		t.Fatalf("wal_runway = %+v (%v), want v2", spec, ok)
	}
	for _, v := range spec.Variants {
		if strings.Contains(v.SQL, "pg_database_size") {
			t.Fatal("wal_runway still sums every database's size")
		}
		for _, col := range []string{"system_identifier", "server_started_at", "role_name"} {
			if !strings.Contains(v.SQL, col) {
				t.Errorf("wal_runway does not select %s", col)
			}
		}
	}
	size, ok := Catalog().Spec(ClusterDatabaseSizeProbe)
	if !ok || size.Family != FamilyRunway || size.Args != ArgsNone {
		t.Fatalf("cluster_database_size = %+v (%v)", size, ok)
	}
	for _, v := range size.Variants {
		if !strings.Contains(v.SQL, "pg_database_size") ||
			!strings.Contains(v.SQL, "has_database_privilege") {
			t.Fatalf("cluster_database_size SQL = %s", v.SQL)
		}
	}
}

func TestWALRunwayOf_DecodesTheCluster(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	w, err := WALRunwayOf(okResult(WALRunwayProbe, nil, Row{"wal_position_bytes": 1e9,
		"max_wal_size_bytes": int64(1 << 30), "wal_keep_size_bytes": int64(0),
		"max_slot_wal_keep_size_bytes": int64(-1), "wal_segment_size_bytes": int64(16 << 20),
		"in_recovery": false, "system_identifier": "7001", "server_started_at": start,
		"role_name": "sage"}))
	if err != nil || w.SystemID != "7001" || !w.StartedAt.Equal(start) ||
		w.RoleName != "sage" || w.PositionBytes != 1e9 {
		t.Fatalf("wal runway = %+v (%v)", w, err)
	}
	// The sizes are not wal_runway's any more: unknown until measured.
	if !math.IsNaN(w.DatabaseBytes) || !math.IsNaN(w.UnreadableDatabases) {
		t.Fatalf("sizes decoded from wal_runway: %v %v", w.DatabaseBytes,
			w.UnreadableDatabases)
	}
}

func TestClusterSizeOf(t *testing.T) {
	bytes, unreadable, err := ClusterSizeOf(okResult(ClusterDatabaseSizeProbe, nil,
		Row{"database_bytes": int64(5e9), "databases_unreadable": int64(2)}))
	if err != nil || bytes != 5e9 || unreadable != 2 {
		t.Fatalf("size = %v %v (%v)", bytes, unreadable, err)
	}
	b, u, err := ClusterSizeOf(okResult(ClusterDatabaseSizeProbe, nil, Row{}))
	if err != nil || !math.IsNaN(b) || !math.IsNaN(u) {
		t.Fatalf("an empty row = %v %v (%v), want unknown", b, u, err)
	}
	if _, _, err := ClusterSizeOf(Result{ProbeID: ClusterDatabaseSizeProbe,
		Status: StatusError, Reason: "statement_timeout"}); err == nil {
		t.Fatal("a failed measurement decoded")
	}
	if _, _, err := ClusterSizeOf(okResult(WALRunwayProbe, nil,
		Row{"database_bytes": int64(1)})); err == nil {
		t.Fatal("another probe's result decoded as a cluster size")
	}
}

package probes

import (
	"testing"
)

// wal_runway names its cluster and cluster_database_size measures the
// databases this role may connect to, against real PostgreSQL.

func TestCatalog_WALRunwayNamesTheCluster(t *testing.T) {
	want, runner := readLiveIdentity(t)
	res := runner.Run(t.Context(), WALRunwayProbe, Args{})
	w, err := WALRunwayOf(res)
	if err != nil || res.Version != "v2" {
		t.Fatalf("wal_runway = %+v (%v)", res, err)
	}
	if w.SystemID != want.systemID || !w.StartedAt.Equal(want.started) || w.RoleName == "" {
		t.Fatalf("wal runway cluster = %q %v %q, want %q %v", w.SystemID, w.StartedAt,
			w.RoleName, want.systemID, want.started)
	}
}

func TestCatalog_ClusterDatabaseSizeMeasuresReadableDatabases(t *testing.T) {
	pool, ctx := livePool(t)
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, ClusterDatabaseSizeProbe,
		Args{})
	bytes, unreadable, err := ClusterSizeOf(res)
	if err != nil || !Known(bytes) || bytes <= 0 || unreadable != 0 {
		t.Fatalf("cluster size = %v %v (%v): %+v", bytes, unreadable, err, res)
	}
	var own float64
	if err := pool.QueryRow(ctx, "SELECT pg_database_size(current_database())::float8").
		Scan(&own); err != nil {
		t.Fatalf("own size: %v", err)
	}
	if bytes < own {
		t.Fatalf("cluster size %v is smaller than this database alone (%v)", bytes, own)
	}
}

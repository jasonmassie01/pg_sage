package probes

import (
	"testing"
	"time"
)

// CHECK-07 against real PostgreSQL: the compared probes report the
// server's real identity, exactly as the server states it.

type liveIdentity struct {
	started    time.Time
	systemID   string
	timeline   int64
	inRecovery bool
	version    int64
}

func readLiveIdentity(t *testing.T) (liveIdentity, *Runner) {
	t.Helper()
	pool, ctx := livePool(t)
	var id liveIdentity
	if err := pool.QueryRow(ctx, `SELECT pg_postmaster_start_time(),
		(SELECT system_identifier::text FROM pg_control_system()),
		CASE WHEN pg_is_in_recovery()
		     THEN (SELECT timeline_id::int8 FROM pg_control_checkpoint())
		     ELSE ('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))
		          ::bit(32)::int8 END,
		pg_is_in_recovery(), current_setting('server_version_num')::int8`).
		Scan(&id.started, &id.systemID, &id.timeline, &id.inRecovery,
			&id.version); err != nil {
		t.Fatalf("read the identity directly: %v", err)
	}
	return id, NewRunner(pool, Catalog(), NewLimiter(1))
}

func (l liveIdentity) check(t *testing.T, probe ID, got ServerIdentity) {
	t.Helper()
	role := ServerRolePrimary
	if l.inRecovery {
		role = ServerRoleStandby
	}
	if !got.StartedAt.Equal(l.started) || got.SystemID != l.systemID ||
		got.TimelineID != l.timeline || got.Role != role || got.VersionNum != l.version {
		t.Fatalf("%s identity = %+v, want %+v (role %s)", probe, got, l, role)
	}
	if got.TimelineID < 1 || got.SystemID == "" {
		t.Fatalf("%s identity has no timeline or system identifier: %+v", probe, got)
	}
}

func TestCatalog_ConnectionSaturationCarriesTheIdentity(t *testing.T) {
	want, runner := readLiveIdentity(t)
	res := runner.Run(t.Context(), ConnectionSaturation, Args{})
	gs, err := ConnectionGroups(res)
	if err != nil || len(gs) == 0 {
		t.Fatalf("connection_saturation = %+v (%v)", res, err)
	}
	if res.Version != "v3" {
		t.Fatalf("connection_saturation version = %s, want v3", res.Version)
	}
	for _, g := range gs {
		want.check(t, ConnectionSaturation, g.Identity)
	}
}

func TestCatalog_WALCheckpointCarriesTheIdentity(t *testing.T) {
	want, runner := readLiveIdentity(t)
	res := runner.Run(t.Context(), WALCheckpoint, Args{})
	w, err := WALStats(res)
	if err != nil {
		t.Fatalf("wal_checkpoint = %+v (%v)", res, err)
	}
	if res.Version != "v2" || !Known(w.WALBytes) {
		t.Fatalf("wal_checkpoint = %+v (version %s)", w, res.Version)
	}
	want.check(t, WALCheckpoint, w.Identity)
}

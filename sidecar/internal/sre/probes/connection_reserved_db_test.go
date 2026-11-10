package probes

import "testing"

// connection_saturation v4 against real PostgreSQL: reserved_connections
// is every slot a normal role cannot use, superuser_reserved_connections
// plus (from PostgreSQL 16) reserved_connections.
func TestCatalog_ConnectionSaturationReservedSlotsMatchTheServer(t *testing.T) {
	pool, ctx := livePool(t)
	var version, superReserved, reserved int64
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int8,
		current_setting('superuser_reserved_connections')::int8,
		COALESCE(current_setting('reserved_connections', true), '0')::int8`).
		Scan(&version, &superReserved, &reserved); err != nil {
		t.Fatalf("read the reserved slots directly: %v", err)
	}
	want := superReserved
	if version >= 160000 {
		want += reserved
	}
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, ConnectionSaturation, Args{})
	gs, err := ConnectionGroups(res)
	if err != nil || len(gs) == 0 {
		t.Fatalf("connection_saturation = %+v (%v)", res, err)
	}
	if res.Version != "v4" {
		t.Fatalf("connection_saturation version = %s, want v4", res.Version)
	}
	for _, g := range gs {
		if g.ReservedConnections != want {
			t.Fatalf("reserved_connections = %d, want %d (server %d)",
				g.ReservedConnections, want, version)
		}
	}
}

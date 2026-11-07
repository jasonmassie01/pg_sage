package gameday

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// benches-02: a local game-day target is told apart from the monitored
// clusters by cluster identity (system identifier, else the shared
// catalog and the postmaster start time), never by DSN text alone.

var clusterStart = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

// fakeCluster answers the identity and target probes like one cluster.
type fakeCluster struct {
	mu       sync.Mutex
	id       ClusterIdentity
	sysErr   error
	identErr error
	factsErr error
	marker   *string
	replicas int64
	queries  int
	closed   int
}

func (c *fakeCluster) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries++
	if err := ctx.Err(); err != nil {
		return fakeRow{err: err}
	}
	switch {
	case strings.Contains(sql, "pg_control_system"):
		if c.sysErr != nil {
			return fakeRow{err: c.sysErr}
		}
		return fakeRow{vals: []any{c.id.SystemID}}
	case strings.Contains(sql, "pg_postmaster_start_time"):
		if c.identErr != nil {
			return fakeRow{err: c.identErr}
		}
		return fakeRow{vals: []any{c.id.PostmasterStart, c.id.CatalogHash, c.id.InRecovery}}
	case strings.Contains(sql, "shobj_description"):
		if c.factsErr != nil {
			return fakeRow{err: c.factsErr}
		}
		return fakeRow{vals: []any{c.marker, c.replicas}}
	}
	return fakeRow{err: fmt.Errorf("fake cluster: unexpected SQL %q", sql)}
}

// Close counts the target connections the provider closed.
func (c *fakeCluster) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("fake row: scan %d values into %d targets", len(r.vals), len(dest))
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.vals[i].(string)
		case **string:
			*d = r.vals[i].(*string)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		case *bool:
			*d = r.vals[i].(bool)
		case *int64:
			*d = r.vals[i].(int64)
		default:
			return fmt.Errorf("fake row: unsupported scan target %T", d)
		}
	}
	return nil
}

func marked(s string) *string { return &s }

// prodCluster is the monitored cluster.
func prodCluster() *fakeCluster {
	return &fakeCluster{id: ClusterIdentity{SystemID: "7300000000000000001",
		PostmasterStart: clusterStart, CatalogHash: "hash-prod"}}
}

// scratchCluster is a disposable cluster with the marker.
func scratchCluster() *fakeCluster {
	return &fakeCluster{id: ClusterIdentity{SystemID: "7300000000000000002",
		PostmasterStart: clusterStart.Add(time.Hour), CatalogHash: "hash-scratch"},
		marker: marked(TargetMarker)}
}

var errPermission = &pgconn.PgError{Code: "42501",
	Message: "permission denied for function pg_control_system"}

func TestReadClusterIdentityReadsEveryProbe(t *testing.T) {
	c := prodCluster()
	c.id.InRecovery = true
	got, err := ReadClusterIdentity(context.Background(), c)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if got != c.id {
		t.Fatalf("identity = %+v, want %+v", got, c.id)
	}
}

func TestReadClusterIdentityFallsBackWithoutPgControlSystem(t *testing.T) {
	c := prodCluster()
	c.sysErr = errPermission
	got, err := ReadClusterIdentity(context.Background(), c)
	if err != nil {
		t.Fatalf("a revoked pg_control_system must only weaken the identity: %v", err)
	}
	if got.SystemID != "" || got.CatalogHash != "hash-prod" ||
		!got.PostmasterStart.Equal(clusterStart) {
		t.Fatalf("fallback identity = %+v", got)
	}
}

func TestReadClusterIdentityErrors(t *testing.T) {
	cause := errors.New("relation pg_database is unreadable")
	c := prodCluster()
	c.sysErr, c.identErr = errPermission, cause
	if _, err := ReadClusterIdentity(context.Background(), c); !errors.Is(err, cause) {
		t.Fatalf("both probes failing: err = %v, want it to wrap the cause", err)
	}
	c = prodCluster()
	c.sysErr = io.ErrUnexpectedEOF
	if _, err := ReadClusterIdentity(context.Background(), c); !errors.Is(err,
		io.ErrUnexpectedEOF) {
		t.Fatalf("a lost connection is not a privilege error: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadClusterIdentity(ctx, prodCluster()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: err = %v, want context.Canceled", err)
	}
	if _, err := ReadClusterIdentity(context.Background(), nil); err == nil {
		t.Fatal("a nil querier gave an identity")
	}
	c = prodCluster()
	c.sysErr, c.id.CatalogHash = errPermission, ""
	if _, err := ReadClusterIdentity(context.Background(), c); err == nil {
		t.Fatal("an identity with neither a system identifier nor a catalog hash was accepted")
	}
}

func TestSameCluster(t *testing.T) {
	later := clusterStart.Add(time.Second)
	id := func(sys, hash string, start time.Time) ClusterIdentity {
		return ClusterIdentity{SystemID: sys, CatalogHash: hash, PostmasterStart: start}
	}
	cases := []struct {
		name string
		a, b ClusterIdentity
		want bool
	}{
		{"equal system ids", id("1", "h1", clusterStart), id("1", "h2", later), true},
		{"different system ids, equal catalog",
			id("1", "h", clusterStart), id("2", "h", clusterStart), false},
		{"one system id unknown, equal catalog and start",
			id("", "h", clusterStart), id("1", "h", clusterStart), true},
		{"both unknown, equal catalog and start",
			id("", "h", clusterStart), id("", "h", clusterStart), true},
		{"equal catalog, different start",
			id("", "h", clusterStart), id("1", "h", later), false},
		{"equal start, different catalog",
			id("", "h1", clusterStart), id("", "h2", clusterStart), false},
	}
	for _, tc := range cases {
		if got := sameCluster(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameCluster = %v, want %v", tc.name, got, tc.want)
		}
		if got := sameCluster(tc.b, tc.a); got != tc.want {
			t.Errorf("%s (swapped): sameCluster = %v, want %v", tc.name, got, tc.want)
		}
	}
}

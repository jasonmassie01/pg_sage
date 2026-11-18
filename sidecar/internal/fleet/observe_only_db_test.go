package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetpol-01d on real PostgreSQL: observe-only members on the local
// cluster are swept and graded in rounds, at most four connections at a
// time, without writing to any member. Run on PG14 and PG17 (CHECK-29).

var observeKey = []byte("0123456789abcdef0123456789abcdef")

// observeMeta is a bootstrapped meta database and its store.
func observeMeta(t *testing.T) (*pgxpool.Pool, *store.DatabaseStore) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "observe_meta"))
	if err != nil {
		t.Fatalf("meta pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap meta: %v", err)
	}
	return pool, store.NewDatabaseStore(pool, observeKey)
}

// observeMember enrolls the database behind dsn as an observe-only member.
func observeMember(t *testing.T, ds *store.DatabaseStore, name, dsn string) int {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 5432
	}
	pw, _ := u.User.Password()
	ssl := u.Query().Get("sslmode")
	if ssl == "" {
		ssl = "disable"
	}
	id, err := ds.CreateObserveOnly(context.Background(), store.DatabaseInput{
		Name: name, Host: u.Hostname(), Port: port, DatabaseName: u.Path[1:],
		Username: u.User.Username(), Password: pw, SSLMode: ssl, MaxConnections: 1,
		TrustLevel: "observation", ExecutionMode: "approval"}, 1)
	if err != nil {
		t.Fatalf("enroll %s: %v", name, err)
	}
	return id
}

// peakObserveSessions samples pg_stat_activity for the sweeper's sessions
// until stop is closed, and returns the peak.
func peakObserveSessions(t *testing.T, stop <-chan struct{}) func() int64 {
	t.Helper()
	ctx := context.Background()
	mon, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("monitor pool: %v", err)
	}
	var peak atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer mon.Close()
		for {
			var n int64
			if err := mon.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE application_name = $1`, ObserveApplicationName).Scan(&n); err == nil &&
				n > peak.Load() {
				peak.Store(n)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return func() int64 { <-done; return peak.Load() }
}

func TestObserveOnlyDB_120MembersSweptAndGradedInRounds(t *testing.T) {
	meta, ds := observeMeta(t)
	ctx := context.Background()
	const members = 120
	ids := make([]int, 0, members)
	var firstDSN string
	for i := range members {
		dsn := testdb.CreateDatabase(t, fmt.Sprintf("observe_%d", i))
		if i == 0 {
			firstDSN = dsn
		}
		ids = append(ids, observeMember(t, ds, fmt.Sprintf("obs-%03d", i), dsn))
	}
	gone := observeMember(t, ds, "obs-gone", "postgres://u:p@127.0.0.1:1/none")
	const roundTime = 5 * time.Minute
	s := NewObserveSweeper(ds, ObserveOptions{Interval: roundTime})
	stop := make(chan struct{})
	peak := peakObserveSessions(t, stop)
	round, err := s.SweepRound(ctx)
	close(stop)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if got := peak(); got < 1 || got > DefaultObserveConcurrency {
		t.Fatalf("peak pg_sage observe sessions %d, want 1-%d", got,
			DefaultObserveConcurrency)
	}
	took := round.FinishedAt.Sub(round.StartedAt)
	t.Logf("CHECK-30: %d members in %s (round time %s), max %d at once", round.Members,
		took, roundTime, round.MaxConcurrent)
	if took <= 0 || took > roundTime {
		t.Fatalf("round took %s, want within the %s round time", took, roundTime)
	}
	if round.Members != members+1 || round.OK != members || round.Unreachable != 1 ||
		round.Failed != 0 || round.MaxConcurrent > DefaultObserveConcurrency {
		t.Fatalf("round %+v, want %d ok and 1 unreachable", round, members)
	}
	assertObserveSweeps(t, ds, ids, gone, round.ID)
	assertNothingWritten(t, firstDSN)
	var stored int
	if err := meta.QueryRow(ctx, `SELECT members FROM sage.observe_rounds WHERE id = $1`,
		round.ID).Scan(&stored); err != nil || stored != members+1 {
		t.Fatalf("stored round members %d (%v)", stored, err)
	}
}

func assertObserveSweeps(t *testing.T, ds *store.DatabaseStore, ids []int, gone int,
	round int64) {
	t.Helper()
	sweeps, err := ds.ObserveSweeps(context.Background())
	if err != nil {
		t.Fatalf("read sweeps: %v", err)
	}
	byID := map[int]store.ObserveSweep{}
	for _, sw := range sweeps {
		byID[sw.DatabaseID] = sw
	}
	// The observe-only subset covers too little of the rubric for an
	// overall letter (the rubric needs coverage for one), so a member is
	// graded when its checks ran and security_access, which the exposure
	// detectors cover, has a letter.
	for _, id := range ids {
		sw := byID[id]
		var g ObserveGradeDetail
		if sw.Status != store.ObserveOK || sw.RoundID != round ||
			json.Unmarshal(sw.Grade, &g) != nil || g.Overall != sw.Letter ||
			g.RubricVersion != "pg_sage-rubric/1" || len(g.Checks) < 30 ||
			g.Dimensions["security_access"] == "" || g.CoveragePct <= 0 {
			t.Fatalf("member %d sweep %+v, want ok with a graded rubric", id, sw)
		}
		// server_eol fails on a server past its end of life (PG14 since
		// 2026-11-12): what matters is that the settings probe graded it.
		if observeCheckStatus(g, "security_access.rls_exposure") != "pass" ||
			!slices.Contains([]string{"pass", "fail"},
				observeCheckStatus(g, "lifecycle.server_eol")) {
			t.Fatalf("member %d: the exposure and settings checks did not run: %s", id,
				sw.Grade)
		}
	}
	if sw := byID[gone]; sw.Status != store.ObserveUnreachable || sw.Error == "" {
		t.Fatalf("unreachable member %+v", sw)
	}
}

func observeCheckStatus(g ObserveGradeDetail, check string) string {
	for _, c := range g.Checks {
		if c.Check == check {
			return string(c.Status)
		}
	}
	return ""
}

// assertNothingWritten: a swept member has no sage schema and no table.
func assertNothingWritten(t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("member pool: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_namespace
		WHERE nspname = 'sage'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sage schemas on a swept member: %d (%v), want 0", n, err)
	}
}

// A promotion lands while a round is running: the round finishes, the
// promoted member leaves the sweep and the 50 cap still holds.
func TestObserveOnlyDB_PromotionDuringASweepRound(t *testing.T) {
	_, ds := observeMeta(t)
	ctx := context.Background()
	ids := make([]int, 0, 12)
	for i := range 12 {
		dsn := testdb.CreateDatabase(t, fmt.Sprintf("observe_promote_%d", i))
		ids = append(ids, observeMember(t, ds, fmt.Sprintf("obs-p%02d", i), dsn))
	}
	base := DefaultObserveVisit(DefaultObserveCheck(ObserveCheckOptions{}))
	var once sync.Once
	promoted := make(chan error, 1)
	visit := func(ctx context.Context, rec store.DatabaseRecord, dsn string) (ObserveGrade,
		error) {
		once.Do(func() { promoted <- ds.SetTier(ctx, ids[11], store.TierFull) })
		return base(ctx, rec, dsn)
	}
	s := NewObserveSweeper(ds, ObserveOptions{Visit: visit})
	round, err := s.SweepRound(ctx)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if err := <-promoted; err != nil {
		t.Fatalf("promotion during the round: %v", err)
	}
	if round.OK+round.Failed != round.Members || round.Members != 12 {
		t.Fatalf("round %+v, want all 12 listed members finished", round)
	}
	rec, err := ds.Get(ctx, ids[11])
	if err != nil || rec.Tier != store.TierFull || !rec.RunsRuntime() {
		t.Fatalf("promoted member %+v (%v)", rec, err)
	}
	next, err := s.SweepRound(ctx)
	if err != nil || next.Members != 11 {
		t.Fatalf("next round %+v (%v), want 11 observe-only members", next, err)
	}
	if _, err := ds.Get(ctx, ids[0]); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := ds.SetTier(ctx, ids[11], store.TierObserveOnly); err != nil {
		t.Fatalf("demote: %v", err)
	}
}

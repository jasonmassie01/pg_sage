package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetpol-03 on real PostgreSQL: the census store, its one run per month
// across leaders, the withdrawn proposal, and pg_sage's own sessions read
// as no real activity (CHECK-32). Run on PG14 and PG17 (CHECK-31).

// censusMember inserts a member row directly (no connection is opened).
func censusMember(t *testing.T, meta *pgxpool.Pool, name, tier, source string) int {
	t.Helper()
	var src any
	if source != "" {
		src = source
	}
	var id int
	if err := meta.QueryRow(context.Background(), `INSERT INTO sage.databases
		(name, host, database_name, username, password_enc, tier, discovered_source,
		 owner, trust_level, execution_mode)
		VALUES ($1, 'h', $1, 'u', '\x00', $2, $3, 'data-team', 'observation', 'approval')
		RETURNING id`, name, tier, src).Scan(&id); err != nil {
		t.Fatalf("insert member %s: %v", name, err)
	}
	return id
}

func censusDiscovered(t *testing.T, meta *pgxpool.Pool, ext, status string, dbID any,
	created time.Time) {
	t.Helper()
	if _, err := meta.Exec(context.Background(), `INSERT INTO sage.discovered_databases
		(source, external_id, host, dbname, owner, status, database_id, creator, created_at,
		 first_seen, last_seen)
		VALUES ('neon', $1, 'h', $1, 'data-team', $2, $3, 'agent:claude', $4, now(), now())`,
		ext, status, dbID, created); err != nil {
		t.Fatalf("insert discovered %s: %v", ext, err)
	}
}

func TestCensusDB_RowsProposalsAndWithdrawal(t *testing.T) {
	meta, _ := observeMeta(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	branch := censusMember(t, meta, "branch-a", "observe_only", "neon")
	censusDiscovered(t, meta, "branch-a", "enrolled", branch, now.Add(-60*day))
	censusDiscovered(t, meta, "br-b", "candidate", nil, now.Add(-20*day))
	censusMember(t, meta, "prod", "full", "")
	cs := NewCensusStore(meta)
	m := map[string]CensusMeasure{
		"branch-a": {QuietFor: 45 * day, ObservedFrom: tp(now.Add(-60 * day)),
			SizeBytes: ip(-1), Basis: "provider"},
		"prod": {LastActivityAt: tp(now.Add(-day)), ObservedFrom: tp(now.Add(-60 * day)),
			SizeBytes: ip(1 << 30), Basis: "fixture"},
	}
	c := NewCensus(cs, CensusOptions{Holder: "a", Measure: measures(m, nil),
		Now: func() time.Time { return now }})
	run, ok, err := c.RunOnce(ctx)
	if err != nil || !ok {
		t.Fatalf("RunOnce = %v %v", ok, err)
	}
	if run.Databases != 3 || run.Proposals != 1 || run.AgentCreated != 2 {
		t.Fatalf("run %+v, want 3 databases, 2 agent-created, 1 proposal", run)
	}
	assertCensusRows(t, meta, run.ID, now)
	props, err := cs.CensusProposals(ctx)
	if err != nil || len(props) != 1 || props[0].DatabaseKey != "branch-a" ||
		props[0].RunID != run.ID || props[0].Owner != "data-team" ||
		!strings.Contains(props[0].Proposal, "45 days: propose TTL 7 days") {
		t.Fatalf("proposals %+v %v, want branch-a's", props, err)
	}
	// The owner adopts the member: the proposal is withdrawn.
	if _, err := meta.Exec(ctx, `UPDATE sage.databases SET adopted_at = now()
		WHERE id = $1`, branch); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if props, err := cs.CensusProposals(ctx); err != nil || len(props) != 0 {
		t.Fatalf("after adoption: %+v %v, want the proposal withdrawn", props, err)
	}
	subjects, err := cs.CensusSubjects(ctx)
	if err != nil || len(subjects) != 3 {
		t.Fatalf("subjects %+v %v", subjects, err)
	}
	for _, s := range subjects {
		if s.Key == "branch-a" && (!s.Adopted || !s.Discovered || s.Creator != "agent:claude") {
			t.Fatalf("adopted subject %+v", s)
		}
	}
}

func assertCensusRows(t *testing.T, meta *pgxpool.Pool, runID int64, now time.Time) {
	t.Helper()
	type got struct {
		kind, status, creator, owner, sizeNote, costNote string
		size                                             *int64
		created                                          *time.Time
		proposal                                         *string
	}
	rows, err := meta.Query(context.Background(), `SELECT database_key, kind, status,
		creator, owner, size_note, cost_note, size_bytes, created_at, proposal
		FROM sage.sprawl_census WHERE run_id = $1`, runID)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	all := map[string]got{}
	for rows.Next() {
		var k string
		var g got
		if err := rows.Scan(&k, &g.kind, &g.status, &g.creator, &g.owner, &g.sizeNote,
			&g.costNote, &g.size, &g.created, &g.proposal); err != nil {
			t.Fatalf("scan: %v", err)
		}
		all[k] = g
	}
	if len(all) != 3 {
		t.Fatalf("rows %+v, want 3", all)
	}
	b := all["branch-a"]
	if b.kind != "observe_only" || b.status != CensusMeasured || b.creator != "agent:claude" ||
		b.size != nil || !strings.Contains(b.sizeNote, "invalid size -1") ||
		b.proposal == nil || b.created == nil || !b.created.Equal(now.Add(-60*day)) {
		t.Fatalf("branch-a row %+v", b)
	}
	p := all["prod"]
	if p.size == nil || *p.size != 1<<30 || p.proposal != nil ||
		!strings.HasPrefix(p.costNote, CensusUnpriced) {
		t.Fatalf("prod row %+v", p)
	}
	n := all["neon/br-b"]
	if n.kind != CensusDiscovered || n.status != CensusNotEnrolled || n.owner != "data-team" ||
		n.creator != "agent:claude" {
		t.Fatalf("never-enrolled row %+v", n)
	}
}

func TestCensusDB_TwoLeadersWriteOneRun(t *testing.T) {
	meta, _ := observeMeta(t)
	ctx := context.Background()
	censusMember(t, meta, "m1", "full", "")
	now := time.Now().UTC()
	var wg sync.WaitGroup
	oks := make([]bool, 2)
	errs := make([]error, 2)
	for i := range 2 {
		c := NewCensus(NewCensusStore(meta), CensusOptions{Holder: fmt.Sprintf("s%d", i),
			Measure: measures(map[string]CensusMeasure{"m1": {}}, nil),
			Now:     func() time.Time { return now }})
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, oks[i], errs[i] = c.RunOnce(ctx)
		}()
	}
	wg.Wait()
	var runs, rows int
	if err := meta.QueryRow(ctx, `SELECT (SELECT count(*) FROM sage.sprawl_census_runs),
		(SELECT count(*) FROM sage.sprawl_census)`).Scan(&runs, &rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if oks[0] == oks[1] || errs[0] != nil || errs[1] != nil || runs != 1 || rows != 1 {
		t.Fatalf("oks %v errs %v runs %d rows %d, want exactly one run", oks, errs, runs, rows)
	}
}

func TestCensusDB_StaleRunIsTakenOverAndTheOldHolderRefused(t *testing.T) {
	meta, _ := observeMeta(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cs := NewCensusStore(meta)
	old, ok, err := cs.BeginCensus(ctx, CensusPeriod(now), now.Add(-2*time.Hour), "dead")
	if err != nil || !ok {
		t.Fatalf("begin the stale run: %v %v", ok, err)
	}
	if _, ok, err := cs.BeginCensus(ctx, CensusPeriod(now), now.Add(-90*time.Minute),
		"early"); err != nil || ok {
		t.Fatalf("a run started 30 minutes after the first was taken over: %v %v", ok, err)
	}
	id, ok, err := cs.BeginCensus(ctx, CensusPeriod(now), now, "new")
	if err != nil || !ok || id != old {
		t.Fatalf("take over: id %d (old %d) %v %v", id, old, ok, err)
	}
	stale := CensusRun{ID: id, Holder: "dead", RunAt: now.Add(-2 * time.Hour)}
	if err := cs.WriteCensus(ctx, stale, nil); !errors.Is(err, ErrCensusSuperseded) {
		t.Fatalf("the old holder's write: %v, want ErrCensusSuperseded", err)
	}
	if err := cs.WriteCensus(ctx, CensusRun{ID: id, Holder: "new", RunAt: now}, nil); err != nil {
		t.Fatalf("the new holder's write: %v", err)
	}
	if _, ok, _ := cs.BeginCensus(ctx, CensusPeriod(now), now.Add(3*time.Hour), "x"); ok {
		t.Fatal("a finished run was taken over")
	}
}

func TestCensusDB_PrunesRunsOlderThan400Days(t *testing.T) {
	meta, _ := observeMeta(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cs := NewCensusStore(meta)
	for _, age := range []time.Duration{401 * day, 399 * day} {
		at := now.Add(-age)
		id, ok, err := cs.BeginCensus(ctx, CensusPeriod(at), at, "a")
		if err != nil || !ok {
			t.Fatalf("begin %s: %v %v", at, ok, err)
		}
		if err := cs.WriteCensus(ctx, CensusRun{ID: id, Holder: "a", RunAt: at},
			[]CensusRow{{DatabaseKey: "k", Kind: CensusFull, Status: CensusMeasured}}); err != nil {
			t.Fatalf("write %s: %v", at, err)
		}
	}
	var oldest time.Time
	var runs int
	if err := meta.QueryRow(ctx, `SELECT count(*), min(run_at) FROM sage.sprawl_census_runs`).
		Scan(&runs, &oldest); err != nil {
		t.Fatalf("count: %v", err)
	}
	if runs != 1 || now.Sub(oldest) > 400*day {
		t.Fatalf("%d runs, oldest %s, want only the 399-day-old run", runs, oldest)
	}
}

func TestCensusDB_RecordActivity(t *testing.T) {
	meta, _ := observeMeta(t)
	ctx := context.Background()
	cs := NewCensusStore(meta)
	censusMember(t, meta, "o", "observe_only", "")
	t0 := time.Now().UTC().Truncate(time.Second)
	steps := []struct {
		calls *int64
		at    time.Time
		last  *time.Time
	}{
		{ip(100), t0, nil},                                     // baseline
		{ip(100), t0.Add(time.Hour), nil},                      // no change
		{ip(150), t0.Add(2 * time.Hour), tp(t0.Add(2 * time.Hour))}, // workload ran
		{ip(900), t0.Add(time.Hour), tp(t0.Add(2 * time.Hour))},     // out of order: ignored
		{ip(0), t0.Add(3 * time.Hour), tp(t0.Add(2 * time.Hour))},   // stats reset to 0
		{ip(7), t0.Add(4 * time.Hour), tp(t0.Add(4 * time.Hour))},   // after reset
	}
	for i, s := range steps {
		if err := cs.RecordActivity(ctx, "o", s.calls, ip(4096), s.at); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		a, err := cs.CensusActivity(ctx, "o")
		if err != nil || !a.Found || !a.BaselineAt.Equal(t0) ||
			(s.last == nil) != (a.LastActivityAt == nil) ||
			(s.last != nil && !a.LastActivityAt.Equal(*s.last)) {
			t.Fatalf("step %d: %+v %v, want last activity %v", i, a, err, s.last)
		}
	}
	if a, _ := cs.CensusActivity(ctx, "missing"); a.Found {
		t.Fatalf("a reading for a key never recorded: %+v", a)
	}
}

// TestCensusDB_SelfActivityReadsNoRealActivity is CHECK-32: a database
// whose only sessions are pg_sage's reads "no real activity"; one query
// by another role is real activity.
func TestCensusDB_SelfActivityReadsNoRealActivity(t *testing.T) {
	meta, ds := observeMeta(t)
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "census_self")
	member, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect member: %v", err)
	}
	t.Cleanup(func() { _ = member.Close(context.Background()) })
	if _, err := member.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	observeMember(t, ds, "self-only", dsn)
	cs := NewCensusStore(meta)
	t0 := time.Now().UTC()
	clock := t0
	s := NewObserveSweeper(ds, ObserveOptions{Activity: cs.RecordActivity,
		Now: func() time.Time { return clock }})
	sweep := func(at time.Time) {
		clock = at
		if r, err := s.SweepRound(ctx); err != nil || r.OK != 1 {
			t.Fatalf("sweep at %s: %+v %v", at, r, err)
		}
	}
	sweep(t0)
	for range 5 { // the sidecar's own reads, as pg_sage's role
		if _, err := member.Exec(ctx, `SELECT count(*) FROM pg_catalog.pg_class`); err != nil {
			t.Fatalf("own read: %v", err)
		}
	}
	sweep(t0.Add(31 * day))
	measure := NewCensusMeasure(nil, cs, func() time.Time { return clock })
	subject := CensusSubject{Key: "self-only", Kind: CensusObserveOnly, Discovered: true}
	m, err := measure(ctx, subject)
	row := CensusRowFor(subject, m, err, nil, clock)
	t.Logf("CHECK-32: %s: %s", row.Status, row.Note)
	if row.Status != CensusMeasured || row.LastActivityAt != nil ||
		!strings.Contains(row.Note, "no real activity") || row.SizeBytes == nil ||
		row.Proposal != "no real activity in 31 days: propose TTL 7 days" {
		t.Fatalf("self-only row %+v (measure err %v)", row, err)
	}
	otherRoleQuery(t, member)
	sweep(t0.Add(32 * day))
	m, err = measure(ctx, subject)
	if err != nil || m.LastActivityAt == nil || !m.LastActivityAt.Equal(t0.Add(32*day)) {
		t.Fatalf("after another role's query: %+v %v, want real activity", m, err)
	}
}

// otherRoleQuery runs one statement as a role that is not pg_sage's.
func otherRoleQuery(t *testing.T, member *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("census_app_%d", time.Now().UnixNano())
	if _, err := member.Exec(ctx, "CREATE ROLE "+role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = member.Exec(context.Background(), "RESET ROLE")
		_, _ = member.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	for _, sql := range []string{"SET ROLE " + role, "SELECT 1", "RESET ROLE"} {
		if _, err := member.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

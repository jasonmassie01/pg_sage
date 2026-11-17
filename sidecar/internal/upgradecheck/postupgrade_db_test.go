package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// drill-02c on a real server: the producer reads the change feed, the
// stored brief and the live catalog, raises one finding per step and
// proposes one plan per database and new version, through the plan store.

// fakeEvents is the change feed's recent events.
type fakeEvents struct {
	events []changefeed.Event
	err    error
}

func (f fakeEvents) Recent(context.Context, time.Duration, int) ([]changefeed.Event, error) {
	return f.events, f.err
}

const upgradeEventID = "version:major:140010->170002@2026-11-02T03:04:00Z"

// postUpgradeFixture is a bootstrapped database with two tables (autovacuum
// off, so only the plan analyzes them), an index on a collation whose
// recorded version now differs, and the stored pre-upgrade brief.
func postUpgradeFixture(t *testing.T) liveDB {
	t.Helper()
	d := bootstrapped(t)
	d.exec(t, `CREATE TABLE public.notes (id int PRIMARY KEY, a text COLLATE "en-US-x-icu")
		WITH (autovacuum_enabled = false)`,
		"INSERT INTO public.notes SELECT g, 'n' || g FROM generate_series(1, 500) g",
		"CREATE INDEX notes_a ON public.notes (a)",
		`CREATE TABLE public.orders WITH (autovacuum_enabled = false) AS
		 SELECT g AS id FROM generate_series(1, 2000) g`,
		`UPDATE pg_catalog.pg_collation SET collversion = '0.0'
		 WHERE collname = 'en-US-x-icu'`)
	b := Brief{Database: d.name, Target: BriefVersion{Major: 17}, Method: PGUpgrade,
		QueryTime: QueryTimeSection{Section: Section{State: SectionKnown}, Total: 2,
			Items: []TableShare{
				{Table: "public.notes", Share: 0.7, QueryIDs: []int64{11}},
				{Table: "public.orders", Share: 0.2, QueryIDs: []int64{12}}}},
		CollationReindex: ReindexSection{Section: Section{State: SectionKnown}, Total: 1,
			Items: []ReindexStep{{Action: ReindexAction, Index: "public.notes_a",
				Table: "public.notes", Collations: []string{"en-US-x-icu"}}}}}
	b.Hash = briefHash(b)
	if _, err := StoreBrief(d.ctx, d.pool, b); err != nil {
		t.Fatalf("store the pre-upgrade brief: %v", err)
	}
	return d
}

// logLines collects the producer's log.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *logLines) has(level, word string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, level+" ") && strings.Contains(s, word) {
			return true
		}
	}
	return false
}

func newProducer(t *testing.T, d liveDB, ev ChangeEvents, plans PlanCreator,
	log *logLines) *PostUpgrade {
	t.Helper()
	if plans == nil {
		plans = changeplan.NewStore(d.pool)
	}
	opts := PostUpgradeOptions{Pool: d.pool, Events: ev, Plans: plans,
		Resolver: changeplan.NewDBResolver(d.pool, d.pool)}
	if log != nil {
		opts.Logf = log.logf
	}
	p, err := NewPostUpgrade(opts)
	if err != nil {
		t.Fatalf("NewPostUpgrade: %v", err)
	}
	return p
}

func countRows(t *testing.T, d liveDB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(d.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func postUpgradePlans(t *testing.T, d liveDB) int {
	return countRows(t, d, `SELECT count(*) FROM sage.plan WHERE origin_ref = $1`,
		"post_upgrade:"+d.name+":170002")
}

func openPostUpgradeFindings(t *testing.T, d liveDB) int {
	return countRows(t, d, `SELECT count(*) FROM sage.findings
		WHERE category = $1 AND status = 'open'`, PostUpgradeCategory)
}

func TestPostUpgradeDB_MajorChangeProposesOneTwoStepPlanOnce(t *testing.T) {
	d := postUpgradeFixture(t)
	ev := fakeEvents{events: []changefeed.Event{versionEventOf(upgradeEventID)}}
	p := newProducer(t, d, ev, nil, nil)
	res, err := p.Tick(d.ctx)
	if err != nil || len(res.Planned) != 1 {
		t.Fatalf("Tick = %+v, %v; want one plan", res, err)
	}
	plan, err := changeplan.NewStore(d.pool).Get(d.ctx, res.Planned[0])
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if plan.Status != changeplan.StatusProposed || len(plan.Steps) != 2 ||
		plan.Steps[0].ProposedSQL != "ANALYZE public.notes, public.orders" ||
		plan.Steps[1].ProposedSQL != "REINDEX INDEX CONCURRENTLY public.notes_a" {
		t.Fatalf("plan = %+v, want proposed ANALYZE then the reindex", plan)
	}
	for _, s := range plan.Steps {
		var sql string
		if s.FindingID == nil {
			t.Fatalf("step %d cites no finding", s.Ordinal)
		}
		if err := d.pool.QueryRow(d.ctx, `SELECT recommended_sql FROM sage.findings
			WHERE id = $1 AND status = 'open' AND category = $2`, *s.FindingID,
			PostUpgradeCategory).Scan(&sql); err != nil || sql != s.ProposedSQL {
			t.Fatalf("step %d finding: %q, %v; want its open finding with the SQL",
				s.Ordinal, sql, err)
		}
	}
	again, err := p.Tick(d.ctx)
	if err != nil || len(again.Planned) != 0 || len(again.Existing) != 1 ||
		again.Existing[0] != res.Planned[0] {
		t.Fatalf("second Tick = %+v, %v; want the existing plan only", again, err)
	}
	if postUpgradePlans(t, d) != 1 || openPostUpgradeFindings(t, d) != 2 {
		t.Fatalf("plans %d findings %d, want 1 and 2", postUpgradePlans(t, d),
			openPostUpgradeFindings(t, d))
	}
}

func TestPostUpgradeDB_MinorChangeProposesNothing(t *testing.T) {
	d := postUpgradeFixture(t)
	ev := fakeEvents{events: []changefeed.Event{
		versionEventOf("version:minor:170004->170005@2026-11-02T03:04:00Z")}}
	res, err := newProducer(t, d, ev, nil, nil).Tick(d.ctx)
	if err != nil || len(res.Planned) != 0 || len(res.Existing) != 0 {
		t.Fatalf("minor change = %+v, %v; want nothing", res, err)
	}
	if n := countRows(t, d, `SELECT count(*) FROM sage.plan`); n != 0 {
		t.Fatalf("%d plans, want none", n)
	}
	if openPostUpgradeFindings(t, d) != 0 {
		t.Fatal("a minor change raised post-upgrade findings")
	}
}

// Concurrency without -race (no cgo here): eight producers, as eight
// sidecars, see two change events of the same upgrade at once.
func TestPostUpgradeDB_TwoChangeEventsAndEightProducersMakeOnePlan(t *testing.T) {
	d := postUpgradeFixture(t)
	ev := fakeEvents{events: []changefeed.Event{versionEventOf(upgradeEventID),
		versionEventOf("version:major:140010->170002@2026-11-02T03:09:00Z")}}
	const n = 8
	var wg sync.WaitGroup
	planned := make(chan int, n)
	errs := make(chan error, n)
	for range n {
		p := newProducer(t, d, ev, nil, nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Tick(d.ctx)
			if err != nil {
				errs <- err
				return
			}
			planned <- len(res.Planned)
		}()
	}
	wg.Wait()
	close(planned)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Tick: %v", err)
	}
	total := 0
	for k := range planned {
		total += k
	}
	if total != 1 || postUpgradePlans(t, d) != 1 || openPostUpgradeFindings(t, d) != 2 {
		t.Fatalf("planned %d, plans %d, findings %d; want one plan with two findings",
			total, postUpgradePlans(t, d), openPostUpgradeFindings(t, d))
	}
}

// failingPlans is a plan store that is down.
type failingPlans struct{ err error }

func (f failingPlans) Create(context.Context, changeplan.Plan,
	changeplan.Resolver) (changeplan.Plan, error) {
	return changeplan.Plan{}, f.err
}

func TestPostUpgradeDB_PlanStoreFailureIsLoggedAndRetriedNextCycle(t *testing.T) {
	d := postUpgradeFixture(t)
	ev := fakeEvents{events: []changefeed.Event{versionEventOf(upgradeEventID)}}
	log := &logLines{}
	down := errors.New("plan store down")
	res, err := newProducer(t, d, ev, failingPlans{err: down}, log).Tick(d.ctx)
	if !errors.Is(err, down) || !strings.Contains(err.Error(), "post-upgrade plan") ||
		len(res.Planned) != 0 {
		t.Fatalf("Tick = %+v, %v; want the store's error, named", res, err)
	}
	if !log.has("WARN", "plan store down") {
		t.Fatalf("log = %v, want a WARN with the store's error", log.lines)
	}
	if postUpgradePlans(t, d) != 0 {
		t.Fatal("a plan was stored although the store failed")
	}
	res, err = newProducer(t, d, ev, nil, log).Tick(d.ctx) // the next cycle
	if err != nil || len(res.Planned) != 1 || postUpgradePlans(t, d) != 1 {
		t.Fatalf("retry = %+v, %v; want the plan", res, err)
	}
	if openPostUpgradeFindings(t, d) != 2 {
		t.Fatalf("findings %d after the retry, want the same 2", openPostUpgradeFindings(t, d))
	}
	if !log.has("INFO", "post-upgrade plan") {
		t.Fatalf("log = %v, want the proposed plan logged", log.lines)
	}
}

func TestPostUpgradeDB_ChangeFeedFailureIsAnError(t *testing.T) {
	d := postUpgradeFixture(t)
	feedDown := errors.New("feed unreachable")
	_, err := newProducer(t, d, fakeEvents{err: feedDown}, nil, nil).Tick(d.ctx)
	if !errors.Is(err, feedDown) || !strings.Contains(err.Error(), "change feed") {
		t.Fatalf("Tick = %v, want the feed's error, named", err)
	}
}

func TestPostUpgradeDB_LiveReadsSeeRightsAndChangedCollations(t *testing.T) {
	d := postUpgradeFixture(t)
	d.exec(t, "CREATE ROLE pu_reader_"+d.name[len(d.name)-8:]+" NOLOGIN",
		"CREATE TABLE public.mine (a int)")
	role := "pu_reader_" + d.name[len(d.name)-8:]
	t.Cleanup(func() {
		_, _ = d.pool.Exec(context.Background(), "DROP OWNED BY "+role+"; "+
			"DROP ROLE IF EXISTS "+role)
	})
	d.exec(t, "ALTER TABLE public.mine OWNER TO "+role)
	tx, err := d.pool.Begin(d.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(d.ctx, "SET LOCAL ROLE "+role); err != nil {
		t.Fatal(err)
	}
	tables, err := readLiveTables(d.ctx, tx, d.major)
	if err != nil {
		t.Fatalf("readLiveTables: %v", err)
	}
	rights := map[string]bool{}
	for _, tb := range tables {
		rights[tb.Name] = tb.CanMaintain
		if strings.HasPrefix(tb.Name, "sage.") {
			t.Fatalf("pg_sage's own table %s is listed", tb.Name)
		}
	}
	if len(rights) != 3 || !rights["public.mine"] || rights["public.notes"] ||
		rights["public.orders"] {
		t.Fatalf("rights = %v, want mine only of notes, orders, mine", rights)
	}
	idx, err := readChangedIndexes(d.ctx, tx, d.major)
	if err != nil || len(idx) != 1 || idx[0].Index != "public.notes_a" || idx[0].CanMaintain {
		t.Fatalf("changed indexes = %+v, %v; want notes_a without rights", idx, err)
	}
}

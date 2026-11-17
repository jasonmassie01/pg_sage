package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// The post-upgrade producer (drill-02c): when the change feed records a
// major version change (guard-02b's server_version source), it proposes
// one plan per database and new version: ANALYZE, then the collation
// reindexes. The plan waits for a person's approval and every step runs
// through the executor; the producer itself writes only pg_sage's own
// rows (the steps' findings and the plan).

// postUpgradeWindow is how far back the feed is read for version changes.
const postUpgradeWindow = 7 * 24 * time.Hour

// VersionChange is a major version upgrade the change feed recorded.
type VersionChange struct {
	EventID string
	Old     int // server_version_num before
	New     int // server_version_num after
	At      time.Time
}

// NewMajor is the major the server was upgraded to.
func (c VersionChange) NewMajor() int { return majorOf(c.New) }

func majorOf(num int) int {
	if num >= 100000 {
		return num / 10000
	}
	return num / 100
}

// VersionChangeOf reads a major version upgrade from a change feed event
// ("version:major:<old>-><new>@<start>"); anything else is false.
func VersionChangeOf(e changefeed.Event) (VersionChange, bool) {
	if !changefeed.VersionChangeMajor(e) {
		return VersionChange{}, false
	}
	body := strings.TrimPrefix(e.EventID, "version:major:")
	body, _, _ = strings.Cut(body, "@")
	oldRaw, newRaw, ok := strings.Cut(body, "->")
	if !ok {
		return VersionChange{}, false
	}
	oldNum, errOld := strconv.Atoi(oldRaw)
	newNum, errNew := strconv.Atoi(newRaw)
	if errOld != nil || errNew != nil || oldNum < 10000 || newNum <= oldNum ||
		majorOf(newNum) == majorOf(oldNum) {
		return VersionChange{}, false
	}
	return VersionChange{EventID: e.EventID, Old: oldNum, New: newNum, At: e.OccurredAt},
		true
}

// ChangeEvents is the database's change feed (*changefeed.Feed).
type ChangeEvents interface {
	Recent(ctx context.Context, window time.Duration, limit int) ([]changefeed.Event, error)
}

// PlanCreator stores a proposed plan (*changeplan.Store).
type PlanCreator interface {
	Create(ctx context.Context, p changeplan.Plan, r changeplan.Resolver) (changeplan.Plan,
		error)
}

// PostUpgradeOptions wire a producer to one monitored database.
type PostUpgradeOptions struct {
	Pool             *pgxpool.Pool
	Events           ChangeEvents
	Plans            PlanCreator
	Resolver         changeplan.Resolver
	DatabaseID       *int
	Logf             func(level, msg string, args ...any) // nil: no log
	StatementTimeout time.Duration                        // 0: DefaultStatementTimeout
	// MaxTableBytes is tuner.analyze_max_table_mb in bytes: larger tables
	// are left out of the ANALYZE step, as the executor's own ANALYZE
	// findings leave them (0: no bound).
	MaxTableBytes int64
}

// PostUpgrade proposes the post-upgrade plans of one database.
type PostUpgrade struct {
	opts PostUpgradeOptions
}

// PostUpgradeResult is one Tick: the plans it proposed, the changes that
// already had one, what it leaves to the operator and what it skipped.
type PostUpgradeResult struct {
	Planned  []int64
	Existing []int64
	Operator []string
	Skipped  []string
}

// NewPostUpgrade checks the producer's dependencies.
func NewPostUpgrade(o PostUpgradeOptions) (*PostUpgrade, error) {
	if o.Pool == nil || o.Events == nil || o.Plans == nil || o.Resolver == nil {
		return nil, errors.New("upgradecheck: the post-upgrade producer needs the " +
			"database pool, its change feed, the plan store and a resolver")
	}
	if o.Logf == nil {
		o.Logf = func(string, string, ...any) {}
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = DefaultStatementTimeout
	}
	return &PostUpgrade{opts: o}, nil
}

// Tick proposes a plan for each major upgrade in the feed's last week
// that has none. A failure is logged and returned; the change stays in
// the feed, so the next cycle tries again.
func (p *PostUpgrade) Tick(ctx context.Context) (PostUpgradeResult, error) {
	var res PostUpgradeResult
	events, err := p.opts.Events.Recent(ctx, postUpgradeWindow, changefeed.MaxList)
	if err != nil {
		return res, fmt.Errorf("post-upgrade plans: read the change feed: %w", err)
	}
	var errs []error
	seen := map[int]bool{}
	for _, e := range events {
		ch, ok := VersionChangeOf(e)
		if !ok || seen[ch.New] {
			continue
		}
		seen[ch.New] = true
		if err := p.ensure(ctx, ch, &res); err != nil {
			p.opts.Logf("WARN", "post-upgrade plan for PostgreSQL %d (%s) not proposed, "+
				"retrying next cycle: %v", ch.NewMajor(), ch.EventID, err)
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// ensure proposes ch's plan unless one exists. Producers of one database
// and new version serialize on a session advisory lock; one that finds
// it held leaves the change to the holder.
func (p *PostUpgrade) ensure(ctx context.Context, ch VersionChange,
	res *PostUpgradeResult) error {
	conn, err := p.opts.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("post-upgrade plan: connect: %w", err)
	}
	defer conn.Release()
	var database string
	if err := conn.QueryRow(ctx, Tag+"SELECT current_database()").Scan(&database); err != nil {
		return fmt.Errorf("post-upgrade plan: read the database name: %w", err)
	}
	key := originRef(database, ch.New)
	var locked bool
	if err := conn.QueryRow(ctx, Tag+`SELECT pg_catalog.pg_try_advisory_lock(
		pg_catalog.hashtextextended($1, 0))`, key).Scan(&locked); err != nil {
		return fmt.Errorf("post-upgrade plan: lock %s: %w", key, err)
	}
	if !locked {
		return nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), Tag+`SELECT pg_catalog.pg_advisory_unlock(
			pg_catalog.hashtextextended($1, 0))`, key)
	}()
	id, err := existingPlan(ctx, conn, key)
	if err != nil || id != 0 {
		if id != 0 {
			res.Existing = append(res.Existing, id)
		}
		return err
	}
	return p.propose(ctx, conn, database, ch, res)
}

func existingPlan(ctx context.Context, conn *pgxpool.Conn, key string) (int64, error) {
	var id int64
	err := conn.QueryRow(ctx, Tag+`SELECT id FROM sage.plan
		WHERE origin = 'generator' AND origin_ref = $1 ORDER BY id LIMIT 1`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("post-upgrade plan: look up %s: %w", key, err)
	}
	return id, nil
}

// propose reads, builds, raises the steps' findings and stores the plan.
func (p *PostUpgrade) propose(ctx context.Context, conn *pgxpool.Conn, database string,
	ch VersionChange, res *PostUpgradeResult) error {
	in, err := p.read(ctx, database, ch)
	if err != nil {
		return err
	}
	d := buildPostUpgrade(in)
	res.Operator = append(res.Operator, d.Operator...)
	res.Skipped = append(res.Skipped, d.Skipped...)
	for _, s := range d.Operator {
		p.opts.Logf("WARN", "post-upgrade to PostgreSQL %d, operator step: %s",
			ch.NewMajor(), s)
	}
	if len(d.Plan.Steps) == 0 {
		return nil
	}
	for i, f := range d.Findings {
		id, err := raiseFinding(ctx, conn, f)
		if err != nil {
			return err
		}
		d.Plan.Steps[i].FindingID = &id
	}
	plan, err := p.opts.Plans.Create(ctx, d.Plan, p.opts.Resolver)
	if err != nil {
		return fmt.Errorf("post-upgrade plan: store: %w", err)
	}
	res.Planned = append(res.Planned, plan.ID)
	p.opts.Logf("INFO", "post-upgrade plan %d proposed for PostgreSQL %d: %d steps, "+
		"awaiting approval", plan.ID, ch.NewMajor(), len(plan.Steps))
	return nil
}

package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrCensusSuperseded refuses a census write by a holder whose run another
// sidecar took over (its run went stale).
var ErrCensusSuperseded = errors.New("census run was taken over by another sidecar")

// censusStaleAfter is how long an unfinished run may stay claimed before
// another sidecar (a new leader) takes it over.
const censusStaleAfter = time.Hour

// censusQueryTimeout bounds each census statement on the meta database.
const censusQueryTimeout = 30 * time.Second

// CensusDB reads and writes the sprawl census in the meta database
// (sage.sprawl_census_runs, sage.sprawl_census, sage.sprawl_activity). It
// reads sage.databases and sage.discovered_databases and writes nothing
// else.
type CensusDB struct{ pool *pgxpool.Pool }

// NewCensusStore returns the census store on the meta pool, or nil.
func NewCensusStore(pool *pgxpool.Pool) *CensusDB {
	if pool == nil {
		return nil
	}
	return &CensusDB{pool: pool}
}

var _ CensusStore = (*CensusDB)(nil)

// censusTime is t as PostgreSQL stores it (UTC, microseconds), so a run's
// claimed time compares equal when its holder writes it.
func censusTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// BeginCensus claims period's run: a new row, or an unfinished one whose
// holder claimed it more than censusStaleAfter before runAt. The unique
// period makes two leaders' claims one run.
func (s *CensusDB) BeginCensus(ctx context.Context, period, runAt time.Time,
	holder string) (int64, bool, error) {
	qctx, cancel := context.WithTimeout(ctx, censusQueryTimeout)
	defer cancel()
	var id int64
	err := s.pool.QueryRow(qctx, `/* pg_sage */ INSERT INTO sage.sprawl_census_runs AS r
		    (period, holder, run_at) VALUES ($1, $2, $3)
		 ON CONFLICT (period) DO UPDATE SET holder = EXCLUDED.holder,
		    run_at = EXCLUDED.run_at
		 WHERE r.finished_at IS NULL AND r.run_at < EXCLUDED.run_at - $4::interval
		 RETURNING id`, period.UTC(), holder, censusTime(runAt),
		fmt.Sprintf("%d seconds", int(censusStaleAfter.Seconds()))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("claiming the census run of %s in "+
			"sage.sprawl_census_runs: %w", period.Format("2006-01"), err)
	}
	return id, true, nil
}

// censusSubjectsSQL is every member, with its discovery row's creator and
// creation time when it has one, and every discovered row never enrolled.
const censusSubjectsSQL = `/* pg_sage */
SELECT m.name, m.tier, COALESCE(d.creator, ''), COALESCE(m.owner, d.owner, ''),
       COALESCE(d.created_at, m.created_at), m.discovered_source IS NOT NULL,
       m.adopted_at IS NOT NULL
  FROM sage.databases m
  LEFT JOIN LATERAL (SELECT x.creator, x.created_at, x.owner FROM sage.discovered_databases x
                      WHERE x.database_id = m.id ORDER BY x.id DESC LIMIT 1) d ON true
UNION ALL
SELECT d.source || '/' || d.external_id, 'discovered', d.creator, d.owner, d.created_at,
       true, false
  FROM sage.discovered_databases d
 WHERE d.database_id IS NULL AND d.status IN ('candidate', 'enrolling', 'ignored')
 ORDER BY 1`

// CensusSubjects lists the databases a census run covers.
func (s *CensusDB) CensusSubjects(ctx context.Context) ([]CensusSubject, error) {
	qctx, cancel := context.WithTimeout(ctx, censusQueryTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, censusSubjectsSQL)
	if err != nil {
		return nil, fmt.Errorf("reading sage.databases and sage.discovered_databases: %w", err)
	}
	defer rows.Close()
	out := []CensusSubject{}
	for rows.Next() {
		var c CensusSubject
		if err := rows.Scan(&c.Key, &c.Kind, &c.Creator, &c.Owner, &c.CreatedAt,
			&c.Discovered, &c.Adopted); err != nil {
			return nil, fmt.Errorf("scanning a census subject: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WriteCensus stores run's rows and summary in one transaction, prunes
// runs older than CensusRetention and the readings of departed members.
// A holder whose run was taken over gets ErrCensusSuperseded and writes
// nothing.
func (s *CensusDB) WriteCensus(ctx context.Context, run CensusRun,
	rows []CensusRow) error {
	qctx, cancel := context.WithTimeout(ctx, censusQueryTimeout)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin the census write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(qctx, `/* pg_sage */ UPDATE sage.sprawl_census_runs SET
		    finished_at = now(), databases = $3, agent_created = $4, reuse_eligible = $5,
		    reused = $6, reuse_share = $7, proposals = $8, unreachable = $9
		 WHERE id = $1 AND holder = $2 AND finished_at IS NULL AND run_at = $10`,
		run.ID, run.Holder, run.Databases, run.AgentCreated, run.ReuseEligible, run.Reused,
		run.ReuseShare, run.Proposals, run.Unreachable, censusTime(run.RunAt))
	if err != nil {
		return fmt.Errorf("finishing census run %d: %w", run.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: run %d (holder %s)", ErrCensusSuperseded, run.ID, run.Holder)
	}
	if err := insertCensusRows(qctx, tx, run, rows); err != nil {
		return err
	}
	if err := pruneCensus(qctx, tx, run); err != nil {
		return err
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("committing census run %d: %w", run.ID, err)
	}
	return nil
}

func insertCensusRows(ctx context.Context, tx pgx.Tx, run CensusRun,
	rows []CensusRow) error {
	if _, err := tx.Exec(ctx, `/* pg_sage */ DELETE FROM sage.sprawl_census
		WHERE run_id = $1`, run.ID); err != nil {
		return fmt.Errorf("clearing census run %d: %w", run.ID, err)
	}
	src := make([][]any, len(rows))
	for i, r := range rows {
		var prop any
		if r.Proposal != "" {
			prop = r.Proposal
		}
		src[i] = []any{run.ID, censusTime(run.RunAt), r.DatabaseKey, r.Kind, r.Status, r.Creator,
			r.Owner, r.CreatedAt, r.LastActivityAt, r.ActivityBasis, r.Reused, r.SizeBytes,
			r.SizeNote, r.CostMonth, r.CostNote, prop, r.Note}
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"sage", "sprawl_census"},
		[]string{"run_id", "run_at", "database_key", "kind", "status", "creator", "owner",
			"created_at", "last_activity_at", "activity_basis", "reused", "size_bytes",
			"size_note", "cost_month", "cost_note", "proposal", "note"},
		pgx.CopyFromRows(src))
	if err != nil {
		return fmt.Errorf("writing the %d rows of census run %d: %w", len(rows), run.ID, err)
	}
	return nil
}

func pruneCensus(ctx context.Context, tx pgx.Tx, run CensusRun) error {
	days := int(CensusRetention / (24 * time.Hour))
	if _, err := tx.Exec(ctx, `/* pg_sage */ DELETE FROM sage.sprawl_census_runs
		WHERE run_at < now() - make_interval(days => $1) AND id <> $2`, days,
		run.ID); err != nil {
		return fmt.Errorf("pruning census runs older than %d days: %w", days, err)
	}
	if _, err := tx.Exec(ctx, `/* pg_sage */ DELETE FROM sage.sprawl_activity a
		WHERE NOT EXISTS (SELECT 1 FROM sage.databases m
		                   WHERE m.name = a.database_key)`); err != nil {
		return fmt.Errorf("pruning the activity readings of departed members: %w", err)
	}
	return nil
}

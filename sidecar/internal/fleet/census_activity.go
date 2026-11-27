package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CensusActivity is an observe-only member's newest activity reading (the
// sweeper's one pg_stat_statements read per round) and its last sweep.
type CensusActivity struct {
	Found          bool   // a reading exists
	WorkloadCalls  *int64 // calls by roles other than pg_sage's; nil: unreadable
	SizeBytes      *int64
	BaselineAt     time.Time  // when the calls became readable
	ReadAt         time.Time  // the newest reading
	LastActivityAt *time.Time // the reading that first saw the calls rise
	SweepStatus    string     // the newest sweep's status; "" never swept
	SweepError     string
}

// recordActivitySQL keeps one reading per member. The calls are a
// cumulative counter, so a rise since the previous reading is real
// activity in between, stamped at this reading; a fall to a positive
// count (a stats reset) is activity since the reset. A reading older than
// the stored one is ignored. The baseline restarts when the calls first
// become readable.
const recordActivitySQL = `/* pg_sage */ INSERT INTO sage.sprawl_activity AS a
    (database_key, workload_calls, size_bytes, baseline_at, read_at)
 VALUES ($1, $2, $3, $4, $4)
 ON CONFLICT (database_key) DO UPDATE SET
    last_activity_at = CASE
        WHEN a.workload_calls IS NULL OR EXCLUDED.workload_calls IS NULL
            THEN a.last_activity_at
        WHEN EXCLUDED.workload_calls > a.workload_calls
          OR (EXCLUDED.workload_calls < a.workload_calls AND EXCLUDED.workload_calls > 0)
            THEN EXCLUDED.read_at
        ELSE a.last_activity_at END,
    baseline_at = CASE WHEN a.workload_calls IS NULL AND EXCLUDED.workload_calls IS NOT NULL
        THEN EXCLUDED.read_at ELSE a.baseline_at END,
    workload_calls = EXCLUDED.workload_calls,
    size_bytes = EXCLUDED.size_bytes,
    read_at = EXCLUDED.read_at
 WHERE EXCLUDED.read_at > a.read_at`

// RecordActivity stores an observe-only member's reading taken at at; a
// negative count or size is stored unknown.
func (s *CensusDB) RecordActivity(ctx context.Context, key string, calls, size *int64,
	at time.Time) error {
	qctx, cancel := context.WithTimeout(ctx, censusQueryTimeout)
	defer cancel()
	if _, err := s.pool.Exec(qctx, recordActivitySQL, key, nonNegative(calls),
		nonNegative(size), censusTime(at)); err != nil {
		return fmt.Errorf("recording the activity reading of %s in sage.sprawl_activity: %w",
			key, err)
	}
	return nil
}

func nonNegative(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// CensusActivity reads key's newest reading and its last sweep.
func (s *CensusDB) CensusActivity(ctx context.Context, key string) (CensusActivity, error) {
	qctx, cancel := context.WithTimeout(ctx, censusQueryTimeout)
	defer cancel()
	var a CensusActivity
	var baseline, read *time.Time
	var status, sweepErr *string
	err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT a.database_key IS NOT NULL,
		    a.workload_calls, a.size_bytes, a.baseline_at, a.read_at, a.last_activity_at,
		    w.status, w.error
		 FROM (SELECT $1::text AS key) k
		 LEFT JOIN sage.sprawl_activity a ON a.database_key = k.key
		 LEFT JOIN sage.databases m ON m.name = k.key
		 LEFT JOIN sage.observe_sweeps w ON w.database_id = m.id`, key).Scan(&a.Found,
		&a.WorkloadCalls, &a.SizeBytes, &baseline, &read, &a.LastActivityAt, &status,
		&sweepErr)
	if err != nil {
		return CensusActivity{}, fmt.Errorf("reading the activity of %s from "+
			"sage.sprawl_activity and sage.observe_sweeps: %w", key, err)
	}
	if baseline != nil {
		a.BaselineAt = *baseline
	}
	if read != nil {
		a.ReadAt = *read
	}
	if status != nil {
		a.SweepStatus, a.SweepError = *status, *sweepErr
	}
	return a, nil
}

// CensusProposal is a standing TTL proposal of the newest finished census.
type CensusProposal struct {
	RunID       int64
	RunAt       time.Time
	DatabaseKey string
	Owner       string
	Creator     string
	Proposal    string
}

// CensusProposals lists the newest finished run's TTL proposals that still
// stand: a member an owner has adopted since, or one that is gone, has its
// proposal withdrawn.
func (s *CensusDB) CensusProposals(ctx context.Context) ([]CensusProposal, error) {
	qctx, cancel := context.WithTimeout(ctx, censusQueryTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT c.run_id, c.run_at,
		    c.database_key, c.owner, c.creator, c.proposal
		 FROM sage.sprawl_census c
		 JOIN sage.databases m ON m.name = c.database_key AND m.adopted_at IS NULL
		WHERE c.proposal IS NOT NULL
		  AND c.run_id = (SELECT r.id FROM sage.sprawl_census_runs r
		                   WHERE r.finished_at IS NOT NULL
		                   ORDER BY r.period DESC LIMIT 1)
		ORDER BY c.database_key`)
	if err != nil {
		return nil, fmt.Errorf("reading the census proposals from sage.sprawl_census: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CensusProposal, error) {
		var p CensusProposal
		err := r.Scan(&p.RunID, &p.RunAt, &p.DatabaseKey, &p.Owner, &p.Creator, &p.Proposal)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("scanning a census proposal: %w", err)
	}
	return out, nil
}

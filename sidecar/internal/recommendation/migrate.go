package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// legacyFindingSQL selects the columns legacyProposal maps, from the
// findings row aliased f.
const legacyFindingSQL = `f.category, f.object_identifier, COALESCE(f.object_type, ''),
	f.title, f.severity, COALESCE(f.recommendation, ''), f.detail`

// MigrateLegacy maps pre-state-machine data into recommendations for
// database: every open finding with SQL becomes a proposed recommendation,
// and every unexecuted pending or approved action_queue row is linked to
// the revision holding its exact SQL. An approval keeps its approver and
// is pinned to that revision (created, marked migrated, when the content
// differs from the finding's). Idempotent: linked rows are skipped.
func MigrateLegacy(
	ctx context.Context, pool *pgxpool.Pool, database string,
) (MigrationReport, error) {
	s := NewStore(pool)
	var report MigrationReport
	created, err := s.migrateFindings(ctx, database)
	report.Findings = created
	if err != nil {
		return report, err
	}
	err = s.migrateQueue(ctx, database, &report)
	return report, err
}

func scanLegacyProposal(row pgx.Row, database string, extra ...any) (Proposal, error) {
	p := Proposal{DatabaseName: database}
	var detail []byte
	dest := append(extra, &p.Category, &p.Target, &p.ObjectType, &p.Title, &p.Severity,
		&p.Recommendation, &detail)
	if err := row.Scan(dest...); err != nil {
		return p, err
	}
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &p.Evidence); err != nil {
			return p, fmt.Errorf("decode finding detail: %w", err)
		}
	}
	return p, nil
}

func (s *Store) migrateFindings(ctx context.Context, database string) (int, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT f.recommended_sql,
		COALESCE(f.rollback_sql, ''), `+legacyFindingSQL+`
		FROM sage.findings f
		WHERE f.status = 'open' AND COALESCE(f.recommended_sql, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM sage.recommendation r
		                   WHERE r.finding_id = f.id AND r.state IN `+liveStatesSQL+`)
		ORDER BY f.id`)
	if err != nil {
		return 0, fmt.Errorf("list legacy findings: %w", err)
	}
	proposals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Proposal, error) {
		var forward, inverse string
		p, err := scanLegacyProposal(row, database, &forward, &inverse)
		p.ForwardSQL, p.InverseSQL = forward, inverse
		return p, err
	})
	if err != nil {
		return 0, err
	}
	created := 0
	for _, p := range proposals {
		var res ProposeResult
		err := s.withTx(ctx, func(tx pgx.Tx) error {
			var err error
			res, err = proposeTx(ctx, tx, p, SourceMigrated)
			return err
		})
		if err != nil {
			return created, fmt.Errorf("migrate finding %s/%s: %w", p.Category, p.Target, err)
		}
		if res.Outcome == OutcomeCreated {
			created++
		}
	}
	return created, nil
}

// legacyQueued is one unlinked action_queue row and the proposal it makes.
type legacyQueued struct {
	id        int64
	status    string
	decidedBy *int
	open      bool
	proposal  Proposal
}

func (s *Store) migrateQueue(
	ctx context.Context, database string, report *MigrationReport,
) error {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT q.id, q.status, q.decided_by,
		f.id IS NOT NULL, q.proposed_sql, COALESCE(q.rollback_sql, ''),
		COALESCE(f.category, ''), COALESCE(f.object_identifier, ''),
		COALESCE(f.object_type, ''), COALESCE(f.title, ''), COALESCE(f.severity, ''),
		COALESCE(f.recommendation, ''), f.detail
		FROM sage.action_queue q
		LEFT JOIN sage.findings f ON f.id = q.finding_id AND f.status = 'open'
		WHERE q.recommendation_id IS NULL AND q.action_log_id IS NULL
		  AND q.status IN ('pending', 'approved')
		ORDER BY q.id`)
	if err != nil {
		return fmt.Errorf("list legacy queued actions: %w", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (legacyQueued, error) {
		var q legacyQueued
		var forward, inverse string
		p, err := scanLegacyProposal(row, database, &q.id, &q.status, &q.decidedBy,
			&q.open, &forward, &inverse)
		p.ForwardSQL, p.InverseSQL = forward, inverse
		q.proposal = p
		return q, err
	})
	if err != nil {
		return err
	}
	for _, q := range queued {
		if err := s.migrateQueued(ctx, q, report); err != nil {
			return fmt.Errorf("migrate queued action %d: %w", q.id, err)
		}
	}
	return nil
}

// migrateQueued links one queue row to the revision of its exact SQL and
// carries its approval over.
func (s *Store) migrateQueued(
	ctx context.Context, q legacyQueued, report *MigrationReport,
) error {
	if !q.open {
		report.Skipped++ // no open finding: it can never be approved or run
		return nil
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		res, err := proposeTx(ctx, tx, q.proposal, SourceMigrated)
		if err != nil {
			return err
		}
		rec := res.Recommendation
		if _, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
			SET recommendation_id = $2, recommendation_revision = $3, content_hash = $4
			WHERE id = $1`, q.id, rec.ID, rec.Revision, rec.ContentHash); err != nil {
			return err
		}
		report.QueueLinked++
		if q.status != "approved" {
			return nil
		}
		actor := "migrated"
		if q.decidedBy != nil {
			actor = "user:" + strconv.Itoa(*q.decidedBy)
		}
		_, err = approveTx(ctx, tx, rec.ID, rec.ContentHash, actor,
			fmt.Sprintf("migrated approval of action_queue %d", q.id))
		if errors.Is(err, ErrConflict) {
			report.Skipped++ // already past approval; nothing to carry over
			return nil
		}
		if err == nil {
			report.ApprovalsMigrated++
		}
		return err
	})
}

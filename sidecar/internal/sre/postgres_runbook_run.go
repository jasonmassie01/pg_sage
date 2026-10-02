package sre

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// RecordRunbookRun stores which runbook version ran in the leased
// investigation, under the lease (a stale worker records nothing). A
// repeated record of the same version is a no-op.
func (s *PostgresStore) RecordRunbookRun(ctx context.Context, lease Lease,
	run RunbookRun) error {
	if err := lease.validate(); err != nil {
		return err
	}
	if err := run.validate(); err != nil {
		return err
	}
	hash, _ := hashBytes(run.ContentHash)
	path, _ := json.Marshal(run.Path)
	var proposal any
	if run.Proposal != nil {
		raw, _ := json.Marshal(run.Proposal)
		proposal = string(raw)
	}
	return s.inTx(ctx, "record runbook run", func(tx pgx.Tx) error {
		if _, _, err := s.lockLeased(ctx, tx, lease); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sage.sre_runbook_runs
			(deployment_id, database_id, investigation_id, runbook_id, version,
			 content_hash, outcome, path, proposal, reason, probes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10, $11)
			ON CONFLICT DO NOTHING`, string(lease.Scope.DeploymentID),
			string(lease.Scope.DatabaseID), string(lease.InvestigationID),
			string(run.RunbookID), run.Version, hash, run.Outcome, string(path), proposal,
			run.Reason, run.Probes)
		return err
	})
}

// RunbookRuns lists a runbook's runs in this database, newest first.
func (s *PostgresStore) RunbookRuns(ctx context.Context, scope Scope, id UUID,
	limit int) ([]RunbookRun, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxRunbookRuns {
		limit = maxRunbookRuns
	}
	rows, err := s.pool.Query(ctx, `SELECT r.investigation_id::text, r.version,
		    r.content_hash, r.outcome, r.path::text, COALESCE(r.proposal::text, ''),
		    r.reason, r.probes, r.created_at, v.name, COALESCE(v.signed_by, '')
		FROM sage.sre_runbook_runs r
		JOIN sage.sre_runbook_versions v ON v.deployment_id = r.deployment_id
		 AND v.database_id = r.database_id AND v.runbook_id = r.runbook_id
		 AND v.version = r.version
		WHERE r.deployment_id = $1 AND r.database_id = $2 AND r.runbook_id = $3
		ORDER BY r.created_at DESC, r.investigation_id LIMIT $4`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), limit)
	if err != nil {
		return nil, storeErr(ctx, "runbook runs", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RunbookRun, error) {
		return scanRun(r, id)
	})
	if err != nil {
		return nil, storeErr(ctx, "runbook runs", err)
	}
	if out == nil {
		out = []RunbookRun{}
	}
	return out, nil
}

func scanRun(row pgx.Row, id UUID) (RunbookRun, error) {
	run := RunbookRun{Label: RunbookRunLabel, RunbookID: id}
	var inv, path, proposal string
	var hash []byte
	if err := row.Scan(&inv, &run.Version, &hash, &run.Outcome, &path, &proposal,
		&run.Reason, &run.Probes, &run.CreatedAt, &run.Name, &run.SignedBy); err != nil {
		return run, err
	}
	run.InvestigationID, run.ContentHash = UUID(inv), hex.EncodeToString(hash)
	if err := json.Unmarshal([]byte(path), &run.Path); err != nil {
		return run, err
	}
	if proposal != "" {
		run.Proposal = &RunbookProposal{}
		if err := json.Unmarshal([]byte(proposal), run.Proposal); err != nil {
			return run, err
		}
	}
	return run, nil
}

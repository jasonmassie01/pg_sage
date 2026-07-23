package autonomy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

type postgresRetentionEnforcer struct {
	pool       *pgxpool.Pool
	batchLimit int
	now        func() time.Time
}

func (enforcer *postgresRetentionEnforcer) Apply(
	ctx context.Context, item schemaguard.Remediation,
) error {
	if enforcer == nil || enforcer.pool == nil {
		return errors.New("retention enforcement database is unavailable")
	}
	if item.Contract.RetentionWindow <= 0 || item.Invariant.RetentionColumn == "" {
		return errors.New("retention enforcement requires a time column and window")
	}
	cutoff := enforcer.currentTime().Add(-item.Contract.RetentionWindow)
	candidates, err := enforcer.candidateCount(ctx, item.Invariant, cutoff)
	if err != nil {
		return err
	}
	if item.Decision.Disposition == schemaguard.DispositionDryRun {
		return enforcer.record(ctx, item.Invariant, cutoff, candidates, 0, "dry_run")
	}
	if item.Decision.Disposition != schemaguard.DispositionApply {
		return errors.New("retention enforcement disposition is not actionable")
	}
	if err := enforcer.requireDurableDryRun(ctx, item.Invariant); err != nil {
		return err
	}
	if err := enforcer.requirePolicyConsent(ctx); err != nil {
		return err
	}
	deleted, err := enforcer.deleteBatch(ctx, item.Invariant, cutoff)
	if err != nil {
		return err
	}
	return enforcer.record(ctx, item.Invariant, cutoff, candidates, deleted, "applied")
}

func (enforcer *postgresRetentionEnforcer) requirePolicyConsent(
	ctx context.Context,
) error {
	current, err := policy.NewStore(enforcer.pool).Current(ctx, policy.Scope{})
	if err != nil {
		return fmt.Errorf("read retention policy consent: %w", err)
	}
	document, err := policy.ParseDocument(current.Document)
	if err != nil {
		return fmt.Errorf("parse retention policy consent: %w", err)
	}
	for _, class := range document.AllowedChangeClasses {
		if class == policy.ChangeRetention {
			return nil
		}
	}
	return errors.New("standing policy does not consent to retention enforcement")
}

func (enforcer *postgresRetentionEnforcer) candidateCount(
	ctx context.Context, invariant schemaguard.Invariant, cutoff time.Time,
) (int64, error) {
	query := "SELECT count(*) FROM " + qualifiedIdentifier(invariant) +
		" WHERE " + quoteIdentifier(invariant.RetentionColumn) + " < $1"
	var count int64
	if err := enforcer.pool.QueryRow(ctx, query, cutoff).Scan(&count); err != nil {
		return 0, fmt.Errorf("count retention candidates: %w", err)
	}
	return count, nil
}

func (enforcer *postgresRetentionEnforcer) requireDurableDryRun(
	ctx context.Context, invariant schemaguard.Invariant,
) error {
	var exists bool
	err := enforcer.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM sage.retention_run WHERE schema_name=$1 AND table_name=$2
		AND disposition='dry_run')`, invariant.Schema, invariant.Table).Scan(&exists)
	if err != nil {
		return fmt.Errorf("read retention dry-run state: %w", err)
	}
	if !exists {
		return errors.New("retention enforcement requires a durable successful dry run")
	}
	return nil
}

func (enforcer *postgresRetentionEnforcer) deleteBatch(
	ctx context.Context, invariant schemaguard.Invariant, cutoff time.Time,
) (int64, error) {
	limit := enforcer.batchLimit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	table := qualifiedIdentifier(invariant)
	column := quoteIdentifier(invariant.RetentionColumn)
	query := "WITH victims AS (SELECT ctid FROM " + table + " WHERE " + column +
		" < $1 ORDER BY " + column + " LIMIT $2 FOR UPDATE SKIP LOCKED) " +
		"DELETE FROM " + table + " target USING victims " +
		"WHERE target.ctid=victims.ctid"
	tag, err := enforcer.pool.Exec(ctx, query, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("apply bounded retention batch: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (enforcer *postgresRetentionEnforcer) record(
	ctx context.Context, invariant schemaguard.Invariant, cutoff time.Time,
	candidates, deleted int64, disposition string,
) error {
	_, err := enforcer.pool.Exec(ctx, `INSERT INTO sage.retention_run
		(schema_name, table_name, retention_column, cutoff_at,
		 candidate_rows, deleted_rows, disposition)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, invariant.Schema, invariant.Table,
		invariant.RetentionColumn, cutoff, candidates, deleted, disposition)
	if err != nil {
		return fmt.Errorf("record retention enforcement: %w", err)
	}
	return nil
}

func (enforcer *postgresRetentionEnforcer) currentTime() time.Time {
	if enforcer.now != nil {
		return enforcer.now()
	}
	return time.Now()
}

func qualifiedIdentifier(invariant schemaguard.Invariant) string {
	return quoteIdentifier(invariant.Schema) + "." + quoteIdentifier(invariant.Table)
}

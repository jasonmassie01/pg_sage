package provideradvice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps reconciled advice in sage.provider_recommendations, one row
// per (database, provider, provider_id), and each source's last fetch in
// sage.provider_advice_sources.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a store on pool (the database's own sage schema).
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Record is one stored recommendation.
type Record struct {
	ID         int64      `json:"id"`
	Database   string     `json:"database"`
	Provider   string     `json:"provider"`
	ProviderID string     `json:"provider_id"`
	Kind       Kind       `json:"kind"`
	Verdict    string     `json:"verdict"`
	Reason     string     `json:"reason"`
	FindingID  *int64     `json:"finding_id,omitempty"`
	Payload    Payload    `json:"payload"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// SourceStatus is a source's last fetch: when it ran, when it last
// succeeded and the error of the last attempt ("" after a success).
type SourceStatus struct {
	Database    string     `json:"database"`
	Provider    string     `json:"provider"`
	LastAttempt time.Time  `json:"last_attempt"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

var validVerdicts = map[string]bool{VerdictAgree: true, VerdictPGSageOnly: true,
	VerdictProviderOnly: true, VerdictConflict: true, VerdictUnparsed: true,
	VerdictUnknown: true}

var validKinds = map[Kind]bool{KindCreateIndex: true, KindDropIndex: true, KindUnknown: true}

func (s *Store) ready(database, provider string) error {
	switch {
	case s == nil || s.pool == nil:
		return errors.New("provider advice store has no database pool")
	case database == "" || provider == "":
		return errors.New("provider advice needs a database and a provider")
	}
	return nil
}

func checkRows(rows []Row) error {
	for _, r := range rows {
		if r.ProviderID == "" || len(r.ProviderID) > 500 || !validKinds[r.Kind] ||
			!validVerdicts[r.Verdict] {
			return fmt.Errorf("invalid provider advice row %q (kind %q, verdict %q)",
				r.ProviderID, r.Kind, r.Verdict)
		}
	}
	return nil
}

const upsertAdviceSQL = `INSERT INTO sage.provider_recommendations
	(database, provider, provider_id, kind, payload, verdict, reason, finding_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (database, provider, provider_id) DO UPDATE SET kind = EXCLUDED.kind,
	payload = EXCLUDED.payload, verdict = EXCLUDED.verdict, reason = EXCLUDED.reason,
	finding_id = EXCLUDED.finding_id, last_seen = now(), resolved_at = NULL`

const resolveAdviceSQL = `UPDATE sage.provider_recommendations SET resolved_at = now()
WHERE database = $1 AND provider = $2 AND resolved_at IS NULL
  AND provider_id <> ALL($3::text[])`

const sourceOKSQL = `INSERT INTO sage.provider_advice_sources
	(database, provider, last_attempt, last_success, last_error)
VALUES ($1, $2, now(), now(), '')
ON CONFLICT (database, provider) DO UPDATE SET last_attempt = now(),
	last_success = now(), last_error = ''`

// Apply records one successful fetch: rows are upserted, advice the
// provider no longer gives is resolved, and the source's error cleared.
// Reconciles of one database and provider are serialized.
func (s *Store) Apply(ctx context.Context, database, provider string, rows []Row) error {
	if err := s.ready(database, provider); err != nil {
		return err
	}
	if err := checkRows(rows); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("provider advice: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if err := lockSource(ctx, tx, database, provider); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		payload, err := json.Marshal(r.Payload)
		if err != nil {
			return fmt.Errorf("provider advice: encode %s: %w", r.ProviderID, err)
		}
		var finding *int64
		if r.FindingID != 0 {
			finding = &r.FindingID
		}
		batch.Queue(upsertAdviceSQL, database, provider, r.ProviderID, string(r.Kind),
			payload, r.Verdict, r.Reason, finding)
		ids = append(ids, r.ProviderID)
	}
	batch.Queue(resolveAdviceSQL, database, provider, ids)
	batch.Queue(sourceOKSQL, database, provider)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("provider advice: record %s advice of %s: %w", provider, database, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("provider advice: commit %s advice of %s: %w", provider, database, err)
	}
	return nil
}

func lockSource(ctx context.Context, tx pgx.Tx, database, provider string) error {
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', '5s', true),
		set_config('statement_timeout', '30s', true),
		pg_advisory_xact_lock(hashtextextended('sage.provider_advice/' || $1 || '/' || $2, 0))`,
		database, provider)
	if err != nil {
		return fmt.Errorf("provider advice: lock %s advice of %s: %w", provider, database, err)
	}
	return nil
}

// RecordSourceError records a failed fetch; the advice rows stay as the
// last successful fetch left them.
func (s *Store) RecordSourceError(ctx context.Context, database, provider string,
	fetchErr error) error {
	if err := s.ready(database, provider); err != nil {
		return err
	}
	if fetchErr == nil {
		return errors.New("provider advice: no source error to record")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.provider_advice_sources
		(database, provider, last_attempt, last_error) VALUES ($1, $2, now(), $3)
		ON CONFLICT (database, provider) DO UPDATE SET last_attempt = now(),
			last_error = EXCLUDED.last_error`, database, provider, clipError(fetchErr))
	if err != nil {
		return fmt.Errorf("provider advice: record the %s source error of %s: %w",
			provider, database, err)
	}
	return nil
}

func clipError(err error) string {
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

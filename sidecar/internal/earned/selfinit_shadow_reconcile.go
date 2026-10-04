package earned

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The reconciler's shadow pass (roadmap 1.4) copies the scored shadow
// decisions the ledger counts (sage.shadow_decision, counted: verified
// outside pg_sage, or a what-if) from the monitored database into the
// ledger as shadow evidence of their pair, once per decision, from its
// own cursor. It never demotes: an incorrect shadow decision only resets
// the class's promotion streak (ClassRecord).

const shadowFactsSQL = `/* pg_sage */ SELECT id, fingerprint, family, action_class, score,
	score_source, scored_at
	FROM sage.shadow_decision
	WHERE status = 'scored' AND counted
	  AND scored_at >= COALESCE($1::timestamptz,
	                            now() - make_interval(secs => $2::double precision))
	ORDER BY scored_at, id LIMIT $3`

// shadowFact is one counted shadow score.
type shadowFact struct {
	id            int64
	fingerprint   string
	family        Family
	class         ActionClass
	score, source string
	at            time.Time
}

func (r *Reconciler) shadowEvidence(ctx context.Context, res *ReconcileResult) error {
	cursor, err := r.svc.store.shadowCursor(ctx)
	if err != nil {
		return err
	}
	facts, last, err := r.shadowFacts(ctx, cursor)
	if err != nil {
		return err
	}
	for _, f := range facts {
		inserted, err := r.svc.store.insertShadowEvidence(ctx, f)
		if err != nil {
			return err
		}
		if inserted {
			res.ShadowRecorded++
		}
	}
	if res.ShadowRecorded > 0 {
		r.svc.invalidate()
	}
	if last == nil {
		return nil
	}
	return r.svc.store.saveShadowCursor(ctx, *last)
}

// shadowFacts reads the counted scores from cursor; it skips a row whose
// class is not of a self-initiated family (never written by the scorer).
func (r *Reconciler) shadowFacts(ctx context.Context, cursor *time.Time) ([]shadowFact,
	*time.Time, error) {
	rows, err := r.pool.Query(ctx, shadowFactsSQL, cursor, r.lookback.Seconds(), selfBatch)
	if err != nil {
		return nil, nil, fmt.Errorf("read shadow scores on %s: %w", r.database, err)
	}
	defer rows.Close()
	var out []shadowFact
	var last *time.Time
	for rows.Next() {
		var f shadowFact
		var family, class string
		if err := rows.Scan(&f.id, &f.fingerprint, &family, &class, &f.score, &f.source,
			&f.at); err != nil {
			return nil, nil, fmt.Errorf("read shadow scores on %s: %w", r.database, err)
		}
		f.family, f.class = Family(family), ActionClass(class)
		at := f.at.UTC()
		last = &at
		if IsSelfInitiated(f.family) && SelfFamilyFor(f.class) == f.family {
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read shadow scores on %s: %w", r.database, err)
	}
	return out, last, nil
}

func (s *PostgresStore) insertShadowEvidence(ctx context.Context, f shadowFact) (bool,
	error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.trust_shadow_evidence (deployment_id,
		database_name, shadow_id, fingerprint, family, action_class, score, source,
		observed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`,
		s.deployment, s.database, f.id, f.fingerprint, string(f.family), string(f.class),
		f.score, f.source, f.at)
	if err != nil {
		return false, storeErr("record shadow evidence", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PostgresStore) shadowCursor(ctx context.Context) (*time.Time, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT shadow_cursor FROM sage.trust_ledger_state
		WHERE deployment_id = $1 AND database_name = $2`, s.deployment, s.database).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return at, storeErr("read shadow cursor", err)
}

// saveShadowCursor moves the shadow cursor forward (never back); an
// unchanged cursor (the boundary row read again) writes nothing.
func (s *PostgresStore) saveShadowCursor(ctx context.Context, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.trust_ledger_state
		(deployment_id, database_name, shadow_cursor) VALUES ($1, $2, $3)
		ON CONFLICT (deployment_id, database_name) DO UPDATE SET
		  shadow_cursor = GREATEST(trust_ledger_state.shadow_cursor, EXCLUDED.shadow_cursor),
		  updated_at = clock_timestamp()
		WHERE trust_ledger_state.shadow_cursor IS NULL
		   OR EXCLUDED.shadow_cursor > trust_ledger_state.shadow_cursor`,
		s.deployment, s.database, at)
	return storeErr("save shadow cursor", err)
}

// GrantedLevel is the level of req's pair as granted (approved, carried
// over, grandfathered or demoted), without the downgrade signals: shadow
// mode records below it (roadmap 1.4).
func (l *Limiter) GrantedLevel(ctx context.Context, req policy.ActionRequest) (int, error) {
	f, c := FamilyForRequest(req)
	if f == "" {
		return 0, fmt.Errorf("%w: the request has no ledger pair", ErrInvalidRequest)
	}
	st, err := l.svc.Granted(ctx, f, c)
	if err != nil {
		return 0, err
	}
	return int(st.Level), nil
}

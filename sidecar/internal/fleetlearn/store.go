package fleetlearn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrFenced refuses a write whose leader lease is no longer current: a
// successor may already be writing, so the stale leader must stop.
var ErrFenced = errors.New("fleetlearn: leader lease lost; write refused")

// Fence is the leader lease a write must still hold. The zero Fence
// writes unfenced (leader election disabled).
type Fence struct {
	Holder string
	Epoch  int64
}

// Store keeps fingerprints and outcome digests in the control database,
// keyed by fleet scope: nothing is ever read across scopes.
type Store struct {
	pool  *pgxpool.Pool
	scope string
}

// NewStore returns the store of one fleet scope.
func NewStore(pool *pgxpool.Pool, scope string) *Store {
	return &Store{pool: pool, scope: scope}
}

const fenceSQL = `/* pg_sage */ SELECT 1 FROM sage.fleet_leader_lease
	WHERE scope = $1 AND holder = $2 AND epoch = $3 AND expires_at > now()
	FOR SHARE`

// inTx runs fn in a transaction that first checks the fence.
func (s *Store) inTx(ctx context.Context, op string, f Fence,
	fn func(pgx.Tx) error) error {
	if s.pool == nil {
		return fmt.Errorf("fleetlearn: %s: %w", op, errNoPool)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("fleetlearn: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if f.Holder != "" {
		var one int
		err := tx.QueryRow(ctx, fenceSQL, s.scope, f.Holder, f.Epoch).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return fmt.Errorf("fleetlearn: %s: check lease: %w", op, err)
		}
	}
	if err := fn(tx); err != nil {
		return fmt.Errorf("fleetlearn: %s: %w", op, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("fleetlearn: %s: commit: %w", op, err)
	}
	return nil
}

const saveFingerprintSQL = `/* pg_sage */ INSERT INTO sage.fleet_fingerprint
	(fleet_scope, database_name, boundary, table_shapes, index_shapes,
	 query_shapes, table_labels, computed_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (fleet_scope, database_name) DO UPDATE SET
	  boundary = EXCLUDED.boundary, table_shapes = EXCLUDED.table_shapes,
	  index_shapes = EXCLUDED.index_shapes, query_shapes = EXCLUDED.query_shapes,
	  table_labels = EXCLUDED.table_labels, computed_at = EXCLUDED.computed_at`

// SaveFingerprint upserts one database's fingerprint.
func (s *Store) SaveFingerprint(ctx context.Context, f Fence, fp Fingerprint) error {
	var labels []byte
	if fp.Labels != nil {
		raw, err := json.Marshal(fp.Labels)
		if err != nil {
			return fmt.Errorf("fleetlearn: encode labels: %w", err)
		}
		labels = raw
	}
	return s.inTx(ctx, "save fingerprint", f, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, saveFingerprintSQL, s.scope, fp.Database, fp.Boundary,
			nonNil(fp.Tables), nonNil(fp.Indexes), nonNil(fp.Queries), labels,
			fp.ComputedAt)
		return err
	})
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

const (
	digestLockSQL = `/* pg_sage */ SELECT pg_advisory_xact_lock(
		hashtextextended('sage.fleet_outcome_digest:' || $1 || ':' || $2, 0))`
	deleteDigestSQL = `/* pg_sage */ DELETE FROM sage.fleet_outcome_digest
		WHERE fleet_scope = $1 AND database_name = $2`
	insertDigestSQL = `/* pg_sage */ INSERT INTO sage.fleet_outcome_digest
		(fleet_scope, database_name, action_class, shape, improved, neutral, regressed)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
)

// SaveDigest replaces one database's outcome digest.
func (s *Store) SaveDigest(ctx context.Context, f Fence, database string,
	counts []OutcomeCount) error {
	return s.inTx(ctx, "save digest", f, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, digestLockSQL, s.scope, database); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, deleteDigestSQL, s.scope, database); err != nil {
			return err
		}
		for _, c := range counts {
			if _, err := tx.Exec(ctx, insertDigestSQL, s.scope, database, c.Class,
				c.Shape, c.Improved, c.Neutral, c.Regressed); err != nil {
				return err
			}
		}
		return nil
	})
}

// Prune drops the rows of databases that left the fleet.
func (s *Store) Prune(ctx context.Context, f Fence, keep []string) error {
	return s.inTx(ctx, "prune", f, func(tx pgx.Tx) error {
		for _, table := range []string{"sage.fleet_fingerprint", "sage.fleet_outcome_digest"} {
			_, err := tx.Exec(ctx, `/* pg_sage */ DELETE FROM `+table+
				` WHERE fleet_scope = $1 AND NOT (database_name = ANY($2))`,
				s.scope, nonNil(keep))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

const fingerprintsSQL = `/* pg_sage */ SELECT database_name, boundary,
	table_shapes, index_shapes, query_shapes, table_labels, computed_at
	FROM sage.fleet_fingerprint WHERE fleet_scope = $1 ORDER BY database_name`

// Fingerprints are every fingerprint of the scope.
func (s *Store) Fingerprints(ctx context.Context) ([]Fingerprint, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("fleetlearn: read fingerprints: %w", errNoPool)
	}
	rows, err := s.pool.Query(ctx, fingerprintsSQL, s.scope)
	if err != nil {
		return nil, fmt.Errorf("fleetlearn: read fingerprints: %w", err)
	}
	defer rows.Close()
	var out []Fingerprint
	for rows.Next() {
		var fp Fingerprint
		var labels []byte
		if err := rows.Scan(&fp.Database, &fp.Boundary, &fp.Tables, &fp.Indexes,
			&fp.Queries, &labels, &fp.ComputedAt); err != nil {
			return nil, fmt.Errorf("fleetlearn: scan fingerprint: %w", err)
		}
		if labels != nil {
			if err := json.Unmarshal(labels, &fp.Labels); err != nil {
				return nil, fmt.Errorf("fleetlearn: decode labels of %s: %w",
					fp.Database, err)
			}
		}
		out = append(out, fp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleetlearn: read fingerprints: %w", err)
	}
	return out, nil
}

const digestsSQL = `/* pg_sage */ SELECT database_name, action_class, shape,
	improved, neutral, regressed FROM sage.fleet_outcome_digest
	WHERE fleet_scope = $1 AND database_name = ANY($2) AND action_class = $3`

// Digests are the outcome counts of databases for one action class.
func (s *Store) Digests(ctx context.Context, databases []string,
	class string) (map[string][]OutcomeCount, error) {
	out := map[string][]OutcomeCount{}
	if len(databases) == 0 {
		return out, nil
	}
	if s.pool == nil {
		return nil, fmt.Errorf("fleetlearn: read digests: %w", errNoPool)
	}
	rows, err := s.pool.Query(ctx, digestsSQL, s.scope, databases, class)
	if err != nil {
		return nil, fmt.Errorf("fleetlearn: read digests: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var db string
		var c OutcomeCount
		if err := rows.Scan(&db, &c.Class, &c.Shape, &c.Improved, &c.Neutral,
			&c.Regressed); err != nil {
			return nil, fmt.Errorf("fleetlearn: scan digest: %w", err)
		}
		out[db] = append(out[db], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleetlearn: read digests: %w", err)
	}
	return out, nil
}

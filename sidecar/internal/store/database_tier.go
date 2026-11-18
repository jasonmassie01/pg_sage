package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Member tiers (fleetpol-01d). A full member gets a runtime (collector,
// analyzer, executor, pools) and counts against the 50-member cap. An
// observe-only member gets none of that: the fleet sweeper visits it with
// one short-lived read-only connection per round, and it can never have
// an action. A tier change counts against the cap of the tier it moves to.
const (
	TierFull        = "full"
	TierObserveOnly = "observe_only"

	MaxFullMembers        = 50
	MaxObserveOnlyMembers = 2000
)

// tierLockKey serializes the cap check and the write of every create and
// tier change across sidecars sharing the meta database.
const tierLockKey = `sage.databases tier cap`

// ValidTier reports whether tier is a stored member tier.
func ValidTier(tier string) bool { return tier == TierFull || tier == TierObserveOnly }

// TierCap is the most members tier may hold; 0 for an unknown tier.
func TierCap(tier string) int {
	switch tier {
	case TierFull:
		return MaxFullMembers
	case TierObserveOnly:
		return MaxObserveOnlyMembers
	}
	return 0
}

// RunsRuntime reports whether the member gets a per-database runtime: it
// is enabled and full. An unknown tier gets none (fail closed).
func (r DatabaseRecord) RunsRuntime() bool { return r.Enabled && r.Tier == TierFull }

// capError is the refusal of a member past its tier's cap. The full
// member's wording is the one the API and CSV import have always shown.
func capError(tier string) error {
	if tier == TierFull {
		return fmt.Errorf("%w: maximum of %d databases reached", ErrValidation,
			MaxFullMembers)
	}
	return fmt.Errorf("%w: maximum of %d observe-only databases reached", ErrValidation,
		MaxObserveOnlyMembers)
}

// CountTier returns the number of members of tier.
func (s *DatabaseStore) CountTier(ctx context.Context, tier string) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var n int
	if err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT count(*) FROM sage.databases
		WHERE tier = $1`, tier).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s databases: %w", tier, err)
	}
	return n, nil
}

// CreateObserveOnly inserts an observe-only member. Returns the ID.
func (s *DatabaseStore) CreateObserveOnly(
	ctx context.Context, input DatabaseInput, createdBy int,
) (int, error) {
	return s.insert(ctx, input, createdBy, discoveryColumns{tier: TierObserveOnly})
}

// lockCaps takes the cap lock in tx: every create and tier change of the
// fleet commits one at a time.
func lockCaps(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(hashtext($1))`,
		tierLockKey); err != nil {
		return fmt.Errorf("locking the member caps: %w", err)
	}
	return nil
}

// tierRoom refuses when tier already holds its cap. Call it under lockCaps.
func tierRoom(ctx context.Context, tx pgx.Tx, tier string) error {
	var n int
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT count(*) FROM sage.databases
		WHERE tier = $1`, tier).Scan(&n); err != nil {
		return fmt.Errorf("checking database count: %w", err)
	}
	if n >= TierCap(tier) {
		return capError(tier)
	}
	return nil
}

// SetTier moves member id to tier: a promotion to full counts against the
// 50, a move to observe-only against the 2,000. The member's runtime
// follows on the next meta reconcile. Moving to the tier it has is a no-op.
func (s *DatabaseStore) SetTier(ctx context.Context, id int, tier string) error {
	if !ValidTier(tier) {
		return fmt.Errorf("%w: tier %q must be %s or %s", ErrValidation, tier, TierFull,
			TierObserveOnly)
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return pgx.BeginTxFunc(qctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCaps(qctx, tx); err != nil {
			return err
		}
		var cur string
		err := tx.QueryRow(qctx, `/* pg_sage */ SELECT tier FROM sage.databases
			WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: database %d", ErrNotFound, id)
		case err != nil:
			return fmt.Errorf("reading database %d tier: %w", id, err)
		case cur == tier:
			return nil
		}
		if err := tierRoom(qctx, tx, tier); err != nil {
			return fmt.Errorf("%w: database %d stays %s", err, id, cur)
		}
		if _, err := tx.Exec(qctx, `/* pg_sage */ UPDATE sage.databases
			SET tier = $2, updated_at = now() WHERE id = $1`, id, tier); err != nil {
			return fmt.Errorf("setting database %d tier to %s: %w", id, tier, err)
		}
		return nil
	})
}

// insertRow writes a new member under the cap lock: the cap check and the
// insert commit together, so two sidecars never both take the last place.
func (s *DatabaseStore) insertRow(ctx context.Context, input DatabaseInput, createdBy int,
	disc discoveryColumns, tagsJSON []byte) (int, error) {
	tier := disc.tier
	if tier == "" {
		tier = TierFull
	}
	// The id comes first so the sealed password can be bound to its row.
	// It is taken before the transaction, which holds the create's only
	// meta connection: a refused insert burns an id, which is harmless.
	id, enc, err := s.allocateSealed(ctx, input.Password)
	if err != nil {
		return 0, err
	}
	_, actUser, actEnc, err := s.actionWriteArgs(id, input.Action)
	if err != nil {
		return 0, err
	}
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockCaps(ctx, tx); err != nil {
			return err
		}
		if err := tierRoom(ctx, tx, tier); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, insertDatabaseSQL, id, input.Name, input.Host, input.Port,
			input.DatabaseName, input.Username, enc, input.SSLMode, input.MaxConnections,
			tagsJSON, input.TrustLevel, input.ExecutionMode, createdBy,
			disc.source, disc.owner, disc.expiresAt, actUser, actEnc, tier)
		if err != nil {
			return fmt.Errorf("inserting database: %w", actionConstraintError(err))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

const insertDatabaseSQL = `/* pg_sage */ INSERT INTO sage.databases
	    (id, name, host, port, database_name, username, password_enc,
	     sslmode, max_connections, tags, trust_level,
	     execution_mode, created_by, discovered_source, owner, expires_at,
	     action_username, action_password_enc, tier)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`

// ObserveOnlyMembers are the enabled observe-only members, by id.
func (s *DatabaseStore) ObserveOnlyMembers(ctx context.Context) ([]DatabaseRecord, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+databaseColumns+`
		FROM sage.databases WHERE tier = 'observe_only' AND enabled ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing observe-only databases: %w", err)
	}
	defer rows.Close()
	var out []DatabaseRecord
	for rows.Next() {
		r, err := scanDatabaseRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing observe-only databases: %w", err)
	}
	return out, nil
}

// marshalTags is the stored form of a member's tags.
func marshalTags(tags map[string]string) ([]byte, error) {
	b, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("marshalling tags: %w", err)
	}
	return b, nil
}

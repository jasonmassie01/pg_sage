package selfconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrUnavailable: no database to keep the ledger in.
	ErrUnavailable = errors.New("selfconfig: no database")
	// ErrNotFound: the key has not been derived on this database yet.
	ErrNotFound = errors.New("selfconfig: setting not derived yet")
	// ErrUnknownKey: the key is not a derived setting.
	ErrUnknownKey = errors.New("selfconfig: not a derived setting")
	// ErrOperatorSet: the operator set the key in the configuration; it is
	// pinned there and only the configuration can change it.
	ErrOperatorSet = errors.New("selfconfig: set by the operator in the configuration")
	// ErrNotPinned: unpin of a key that is not pinned.
	ErrNotPinned = errors.New("selfconfig: setting is not pinned")
)

// lockID serializes derivation passes and pins on one database
// (pg_advisory_xact_lock, "SAGESCFG").
const lockID int64 = 0x5341474553434647

// Store keeps the derivation state and ledger in the monitored database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns the store of pool's database.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// LedgerEntry is one row of the derivation ledger.
type LedgerEntry struct {
	ID          int64      `json:"id"`
	Key         string     `json:"key"`
	Event       EventKind  `json:"event"`
	Value       *float64   `json:"value"`
	Previous    *float64   `json:"previous"`
	Evidence    []Citation `json:"evidence"`
	Bounds      Bounds     `json:"bounds"`
	Rule        string     `json:"rule"`
	RuleVersion int        `json:"rule_version"`
	Reason      string     `json:"reason"`
	Actor       string     `json:"actor"`
	At          time.Time  `json:"at"`
}

// Setting is a key's state with its newest ledger entries.
type Setting struct {
	State
	History []LedgerEntry
}

const stateColumns = `key, status, value, pending_value, active_value, shadow_value,
	shadow_since, shadow_reason, shadow_outcome, samples, pinned_value, pinned_by,
	pinned_at, operator_value, note, rule, rule_version, evidence, bounds, updated_at`

func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("selfconfig: begin: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockID); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("selfconfig: lock the derivation state: %w", err)
	}
	return tx, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadStates(ctx context.Context, q querier, key string) (map[string]State, error) {
	rows, err := q.Query(ctx, "SELECT "+stateColumns+
		" FROM sage.config_derived_setting WHERE $1 = '' OR key = $1 ORDER BY key", key)
	if err != nil {
		return nil, fmt.Errorf("selfconfig: read derivation state: %w", err)
	}
	defer rows.Close()
	out := map[string]State{}
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		out[st.Key] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("selfconfig: read derivation state: %w", err)
	}
	return out, nil
}

func scanState(rows pgx.Rows) (State, error) {
	var st State
	var since, pinnedAt *time.Time
	var outcome string
	var samples, evidence, bounds []byte
	err := rows.Scan(&st.Key, &st.Status, &st.Value, &st.Pending, &st.Active, &st.Shadow,
		&since, &st.ShadowReason, &outcome, &samples, &st.Pinned, &st.PinnedBy, &pinnedAt,
		&st.Operator, &st.Note, &st.Rule, &st.RuleVersion, &evidence, &bounds, &st.UpdatedAt)
	if err != nil {
		return st, fmt.Errorf("selfconfig: scan derivation state: %w", err)
	}
	if since != nil {
		st.ShadowSince = *since
	}
	if pinnedAt != nil {
		st.PinnedAt = *pinnedAt
	}
	st.ShadowOutcome = Outcome(outcome)
	for _, part := range []struct {
		raw []byte
		dst any
	}{{samples, &st.Samples}, {evidence, &st.Evidence}, {bounds, &st.Bounds}} {
		if err := json.Unmarshal(part.raw, part.dst); err != nil {
			return st, fmt.Errorf("selfconfig: decode state of %s: %w", st.Key, err)
		}
	}
	return st, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func saveState(ctx context.Context, tx pgx.Tx, st State) error {
	samples, _ := json.Marshal(nonNil(st.Samples))
	evidence, _ := json.Marshal(nonNilCites(st.Evidence))
	bounds, _ := json.Marshal(st.Bounds)
	_, err := tx.Exec(ctx, `INSERT INTO sage.config_derived_setting (`+stateColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
		        $17, $18, $19, $20)
		ON CONFLICT (key) DO UPDATE SET status = EXCLUDED.status, value = EXCLUDED.value,
		  pending_value = EXCLUDED.pending_value, active_value = EXCLUDED.active_value,
		  shadow_value = EXCLUDED.shadow_value, shadow_since = EXCLUDED.shadow_since,
		  shadow_reason = EXCLUDED.shadow_reason, shadow_outcome = EXCLUDED.shadow_outcome,
		  samples = EXCLUDED.samples, pinned_value = EXCLUDED.pinned_value,
		  pinned_by = EXCLUDED.pinned_by, pinned_at = EXCLUDED.pinned_at,
		  operator_value = EXCLUDED.operator_value, note = EXCLUDED.note,
		  rule = EXCLUDED.rule, rule_version = EXCLUDED.rule_version,
		  evidence = EXCLUDED.evidence, bounds = EXCLUDED.bounds,
		  updated_at = EXCLUDED.updated_at`,
		st.Key, st.Status, st.Value, st.Pending, st.Active, st.Shadow,
		nullTime(st.ShadowSince), st.ShadowReason, string(st.ShadowOutcome), samples,
		st.Pinned, st.PinnedBy, nullTime(st.PinnedAt), st.Operator, st.Note, st.Rule,
		max(st.RuleVersion, 1), evidence, bounds, st.UpdatedAt)
	if err != nil {
		return fmt.Errorf("selfconfig: save state of %s: %w", st.Key, err)
	}
	return nil
}

func appendEvents(ctx context.Context, tx pgx.Tx, st State, events []Event,
	actor string, at time.Time) error {
	evidence, _ := json.Marshal(nonNilCites(st.Evidence))
	bounds, _ := json.Marshal(st.Bounds)
	for _, ev := range events {
		_, err := tx.Exec(ctx, `INSERT INTO sage.config_derivation (key, event, value,
			previous_value, evidence, bounds, rule, rule_version, reason, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			st.Key, string(ev.Kind), ev.Value, ev.Previous, evidence, bounds, st.Rule,
			max(st.RuleVersion, 1), ev.Reason, actor, at)
		if err != nil {
			return fmt.Errorf("selfconfig: record %s of %s: %w", ev.Kind, st.Key, err)
		}
	}
	return nil
}

func nonNil(xs []float64) []float64 {
	if xs == nil {
		return []float64{}
	}
	return xs
}

func nonNilCites(xs []Citation) []Citation {
	if xs == nil {
		return []Citation{}
	}
	return xs
}

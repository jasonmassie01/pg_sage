package ha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresIdentityStore keeps monitor histories in sage.ha_identity (the
// control database: the meta database when there is one).
type PostgresIdentityStore struct{ pool *pgxpool.Pool }

// NewPostgresIdentityStore returns a store over pool.
func NewPostgresIdentityStore(pool *pgxpool.Pool) *PostgresIdentityStore {
	return &PostgresIdentityStore{pool: pool}
}

var errNoIdentityStore = errors.New("ha: no identity store")

// LoadIdentity reads key's history; found is false when there is none.
func (s *PostgresIdentityStore) LoadIdentity(ctx context.Context, key string) (Persisted,
	bool, error) {
	if s == nil || s.pool == nil {
		return Persisted{}, false, errNoIdentityStore
	}
	var p Persisted
	var role string
	var tli *int64
	var sys *string
	var started, changed *time.Time
	err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT role, timeline_id,
		system_identifier, server_started_at, last_change_at, observed_at
		FROM sage.ha_identity WHERE monitor_key = $1`, key).Scan(&role, &tli, &sys,
		&started, &changed, &p.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Persisted{}, false, nil
	}
	if err != nil {
		return Persisted{}, false, fmt.Errorf("ha: load identity %q: %w", key, err)
	}
	p.Identity.Role = Role(role)
	if tli != nil {
		p.Identity.TimelineID = *tli
	}
	if sys != nil {
		p.Identity.SystemID = *sys
	}
	if started != nil {
		p.Identity.StartedAt = *started
	}
	if changed != nil {
		p.LastChange = *changed
	}
	return p, true, nil
}

// SaveIdentity replaces key's history. Only an observed role is history.
func (s *PostgresIdentityStore) SaveIdentity(ctx context.Context, key string,
	p Persisted) error {
	switch {
	case s == nil || s.pool == nil:
		return errNoIdentityStore
	case strings.TrimSpace(key) == "":
		return errors.New("ha: save identity: empty key")
	case p.Identity.Role != RolePrimary && p.Identity.Role != RoleReplica:
		return fmt.Errorf("ha: save identity %q: role %q is not an observed role", key,
			p.Identity.Role)
	}
	observed := p.ObservedAt
	if observed.IsZero() {
		observed = time.Now()
	}
	_, err := s.pool.Exec(ctx, `/* pg_sage */ INSERT INTO sage.ha_identity
		(monitor_key, role, timeline_id, system_identifier, server_started_at,
		 last_change_at, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (monitor_key) DO UPDATE SET role = EXCLUDED.role,
		    timeline_id = EXCLUDED.timeline_id,
		    system_identifier = EXCLUDED.system_identifier,
		    server_started_at = EXCLUDED.server_started_at,
		    last_change_at = EXCLUDED.last_change_at, observed_at = EXCLUDED.observed_at`,
		key, string(p.Identity.Role), positive(p.Identity.TimelineID),
		nonEmpty(p.Identity.SystemID), nonZero(p.Identity.StartedAt), nonZero(p.LastChange),
		observed)
	if err != nil {
		return fmt.Errorf("ha: save identity %q: %w", key, err)
	}
	return nil
}

func positive(n int64) any {
	if n <= 0 {
		return nil
	}
	return n
}

func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nonZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

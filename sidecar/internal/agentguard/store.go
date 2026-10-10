package agentguard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists principals and taint in the control database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a store on the control database. A nil pool yields a
// store that validates input and fails every storage call with
// ErrUnavailable.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool is the control database pool (nil when unavailable).
func (s *Store) Pool() *pgxpool.Pool {
	if s == nil {
		return nil
	}
	return s.pool
}

func (s *Store) ready() error {
	if s == nil || s.pool == nil {
		return ErrUnavailable
	}
	return nil
}

const (
	maxActorLen   = 200
	maxTenantLen  = 200
	maxReasonLen  = 2000
	maxSourceLen  = 2000
	defaultLimit  = 50
	maxLimit      = 200
	sqlUniqueViol = "23505"
	sqlFKViol     = "23503"
)

var profilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,99}$`)

// CreateRequest describes a new principal.
type CreateRequest struct {
	Name          string
	SponsorUserID *int // nil: unsponsored (L0 for agent_* tools)
	Tenant        string
	Profile       string
	EnvCeiling    Env // "" = dev, the table default
	CreatedBy     string
}

func (r CreateRequest) normalize() (CreateRequest, error) {
	if r.EnvCeiling == "" {
		r.EnvCeiling = EnvDev
	}
	switch {
	case !ValidName(r.Name):
		return r, invalid("name %q must match ^[a-z][a-z0-9-]{1,62}$", r.Name)
	case r.SponsorUserID != nil && *r.SponsorUserID <= 0:
		return r, invalid("sponsor_user_id must be a positive user id")
	case !profilePattern.MatchString(r.Profile):
		return r, invalid("profile %q must be a profile name", r.Profile)
	case !r.EnvCeiling.Valid():
		return r, invalid("env_ceiling %q must be branch, dev, stage or prod", r.EnvCeiling)
	}
	if err := checkText("tenant", r.Tenant, 0, maxTenantLen); err != nil {
		return r, err
	}
	return r, checkText("created_by", r.CreatedBy, 1, maxActorLen)
}

// checkText requires min..max runes of valid UTF-8 without control
// characters.
func checkText(field, value string, min, max int) error {
	n := utf8.RuneCountInString(value)
	if !utf8.ValidString(value) || n < min || n > max {
		return invalid("%s must be %d to %d characters", field, min, max)
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return invalid("%s must not contain control characters", field)
	}
	return nil
}

// principalColumns reads a principal with its sponsor and taint state.
const principalColumns = `p.id, p.name, p.sponsor_user_id, p.tenant, p.profile,
	p.env_ceiling, p.status, p.frozen_reason, p.created_by, p.created_at, p.updated_at,
	(u.id IS NOT NULL),
	EXISTS (SELECT 1 FROM sage.guard_taint t
	        WHERE t.principal_id = p.id AND t.cleared_at IS NULL)`

const principalFrom = ` FROM sage.guard_principals p
	LEFT JOIN sage.users u ON u.id = p.sponsor_user_id`

func scanPrincipal(row pgx.Row) (Principal, error) {
	var p Principal
	var env, status string
	err := row.Scan(&p.ID, &p.Name, &p.SponsorUserID, &p.Tenant, &p.Profile, &env,
		&status, &p.FrozenReason, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
		&p.SponsorActive, &p.Tainted)
	p.EnvCeiling, p.Status = Env(env), Status(status)
	return p, err
}

// Create inserts a principal with a fresh id.
func (s *Store) Create(ctx context.Context, req CreateRequest) (Principal, error) {
	req, err := req.normalize()
	if err != nil {
		return Principal{}, err
	}
	if err := s.ready(); err != nil {
		return Principal{}, err
	}
	id, err := NewID()
	if err != nil {
		return Principal{}, err
	}
	_, err = s.pool.Exec(ctx, `/* pg_sage guard_principal_create v1 */
		INSERT INTO sage.guard_principals (id, name, sponsor_user_id, tenant, profile,
			env_ceiling, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, req.Name, req.SponsorUserID, req.Tenant, req.Profile, string(req.EnvCeiling),
		req.CreatedBy)
	if err != nil {
		return Principal{}, mapWriteError(err, "creating principal "+req.Name)
	}
	return s.Get(ctx, id)
}

// mapWriteError turns constraint violations into the store's errors.
func mapWriteError(err error, what string) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.Code == sqlUniqueViol && strings.Contains(pg.ConstraintName, "name"):
			return fmt.Errorf("%w: %s", ErrDuplicateName, what)
		case pg.Code == sqlFKViol && strings.Contains(pg.ConstraintName, "sponsor"):
			return fmt.Errorf("%w: %s", ErrSponsorNotFound, what)
		}
	}
	return fmt.Errorf("agentguard: %s: %w", what, err)
}

func isFKViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == sqlFKViol
}

// Get loads a principal by id.
func (s *Store) Get(ctx context.Context, id string) (Principal, error) {
	if !ValidID(id) {
		return Principal{}, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	return s.getWhere(ctx, "p.id = $1", id)
}

// GetByName loads a principal by its slug.
func (s *Store) GetByName(ctx context.Context, name string) (Principal, error) {
	if !ValidName(name) {
		return Principal{}, fmt.Errorf("%w: principal %q", ErrNotFound, name)
	}
	return s.getWhere(ctx, "p.name = $1", name)
}

func (s *Store) getWhere(ctx context.Context, where, arg string) (Principal, error) {
	if err := s.ready(); err != nil {
		return Principal{}, err
	}
	p, err := scanPrincipal(s.pool.QueryRow(ctx, `/* pg_sage guard_principal_get v1 */
		SELECT `+principalColumns+principalFrom+` WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, fmt.Errorf("%w: principal %q", ErrNotFound, arg)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("agentguard: loading principal %q: %w", arg, err)
	}
	return p, nil
}

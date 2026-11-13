package pgaudit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// roleCacheTTL is how long one read of the agent roles serves batches.
const roleCacheTTL = 60 * time.Second

// roleReadTimeout bounds one read of the control database.
const roleReadTimeout = 5 * time.Second

// Owner is the principal an agent role belongs to.
type Owner struct {
	ID   string // agp_...
	Name string // the principal's name: the slug older brokers set
}

// RoleResolver maps an agent role to the principal it belongs to.
type RoleResolver interface {
	PrincipalOfRole(role string) (Owner, bool)
}

// RoleMap is one read of sage.guard_cluster_roles: each agent role and its
// principal. A role that maps to more than one principal is absent.
type RoleMap map[string]Owner

// PrincipalOfRole is role's principal; false when it has none, or more
// than one.
func (m RoleMap) PrincipalOfRole(role string) (Owner, bool) {
	o, ok := m[role]
	return o, ok
}

// roleRow is one principal's two roles on one cluster.
type roleRow struct {
	principalID, name, loginRole, brokerRole string
}

// newRoleMap maps every role to its principal. Role names are unique only
// per cluster key, so the rows of every cluster are read: the same
// principal on several clusters is one owner, while a role two principals
// share resolves to none.
func newRoleMap(rows []roleRow) RoleMap {
	m := RoleMap{}
	shared := map[string]bool{}
	add := func(role string, o Owner) {
		if shared[role] {
			return
		}
		if prev, ok := m[role]; ok && prev.ID != o.ID {
			delete(m, role)
			shared[role] = true
			return
		}
		m[role] = o
	}
	for _, r := range rows {
		o := Owner{ID: r.principalID, Name: r.name}
		add(r.loginRole, o)
		add(r.brokerRole, o)
	}
	return m
}

// RoleQuerier is what the roles are read through: the control database's
// pool (or one connection).
type RoleQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readRoles reads every principal's roles on every cluster, whatever their
// status: a killed or retired principal's earlier sessions still resolve.
func readRoles(ctx context.Context, q RoleQuerier) ([]roleRow, error) {
	ctx, cancel := context.WithTimeout(ctx, roleReadTimeout)
	defer cancel()
	rows, err := q.Query(ctx, `/* pg_sage pgaudit v1 */ SELECT r.principal_id, p.name,
		r.login_role, r.broker_role FROM sage.guard_cluster_roles r
		JOIN sage.guard_principals p ON p.id = r.principal_id`)
	if err != nil {
		return nil, fmt.Errorf("pgaudit: read agent roles (sage.guard_cluster_roles): %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (roleRow, error) {
		var r roleRow
		err := row.Scan(&r.principalID, &r.name, &r.loginRole, &r.brokerRole)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("pgaudit: read agent roles (sage.guard_cluster_roles): %w", err)
	}
	return out, nil
}

// RoleCache serves the agent roles to every correlator, reading the
// control database at most once per roleCacheTTL. A failed read is never
// served from an older one: the batch gets no roles, so its slug claims
// and direct sessions name no principal.
type RoleCache struct {
	load    func(context.Context) ([]roleRow, error)
	onError func(error) // the first failure of each failing streak
	now     func() time.Time

	mu      sync.Mutex
	roles   RoleMap
	readAt  time.Time
	failing bool
}

// NewRoleCache reads the roles through source, the control database's
// querier at the time of each read (nil: no control database, no roles).
// onError, when set, is told the first failure of each failing streak.
func NewRoleCache(source func() RoleQuerier, onError func(error)) *RoleCache {
	load := func(ctx context.Context) ([]roleRow, error) {
		if source == nil {
			return nil, nil
		}
		q := source()
		if q == nil {
			return nil, nil
		}
		return readRoles(ctx, q)
	}
	return newRoleCache(load, onError, time.Now)
}

func newRoleCache(load func(context.Context) ([]roleRow, error), onError func(error),
	now func() time.Time) *RoleCache {
	return &RoleCache{load: load, onError: onError, now: now}
}

// Roles is the current role map; on a failed read it is nil with the error.
// A nil cache has no roles.
func (c *RoleCache) Roles(ctx context.Context) (RoleMap, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.roles != nil && now.Sub(c.readAt) < roleCacheTTL {
		return c.roles, nil
	}
	rows, err := c.load(ctx)
	if err != nil {
		c.roles = nil
		if !c.failing && c.onError != nil {
			c.onError(err)
		}
		c.failing = true
		return nil, err
	}
	c.roles, c.readAt, c.failing = newRoleMap(rows), now, false
	return c.roles, nil
}

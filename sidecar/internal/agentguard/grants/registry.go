package grants

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

const grantColumns = `id, database_id::text, principal_id, lane, capability, object_kind,
  object_oid::int8, object_name, COALESCE(schema_oid, 0)::int8, columns, privileges, grantor,
  granted_at, expires_at, revoked_at, state, COALESCE(revoke_detail, ''), grant_action_id,
  revoke_action_id`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	var oid, nsp int64
	err := row.Scan(&g.ID, &g.DatabaseID, &g.PrincipalID, &g.Lane, &g.Capability,
		&g.ObjectKind, &oid, &g.ObjectName, &nsp, &g.Columns, &g.Privileges, &g.Grantor,
		&g.GrantedAt, &g.ExpiresAt, &g.RevokedAt, &g.State, &g.RevokeDetail,
		&g.GrantActionID, &g.RevokeActionID)
	g.ObjectOID, g.SchemaOID = uint32(oid), uint32(nsp)
	return g, err
}

// Get reads one registry row.
func Get(ctx context.Context, q Querier, id int64) (Grant, error) {
	g, err := scanGrant(q.QueryRow(ctx, `/* pg_sage guard_grant v1 */ SELECT `+grantColumns+
		` FROM sage.guard_grants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, fmt.Errorf("%w: grant %d", agentguard.ErrNotFound, id)
	}
	if err != nil {
		return Grant{}, fmt.Errorf("grants: reading grant %d: %w", id, err)
	}
	return g, nil
}

// Filter selects registry rows of one principal, newest first.
type Filter struct {
	PrincipalID string
	// State is active, revoked or revoke_incomplete; "" is every state.
	State  string
	Limit  int    // 1-200
	Cursor string // NextCursor of the previous page
}

// Page is one page of registry rows.
type Page struct {
	Items      []Grant `json:"items"`
	NextCursor string  `json:"next_cursor"`
}

func parseCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil || n <= 0 {
		return 0, invalidf("cursor %q is not a page cursor", c)
	}
	return n, nil
}

func validState(s string) bool {
	return s == "" || s == StateActive || s == StateRevoked || s == StateRevokeIncomplete
}

// List pages a principal's registry rows by id, newest first (index
// guard_grants_principal_idx).
func List(ctx context.Context, q Querier, f Filter) (Page, error) {
	if !agentguard.ValidID(f.PrincipalID) {
		return Page{}, invalidf("principal %q", f.PrincipalID)
	}
	if f.Limit < 1 || f.Limit > 200 || !validState(f.State) {
		return Page{}, invalidf("limit must be 1-200 and state active, revoked or " +
			"revoke_incomplete")
	}
	before, err := parseCursor(f.Cursor)
	if err != nil {
		return Page{}, err
	}
	rows, err := q.Query(ctx, `/* pg_sage guard_grant v1 */ SELECT `+grantColumns+`
		FROM sage.guard_grants WHERE principal_id = $1
		  AND ($2 = '' OR state = $2) AND ($3 = 0 OR id < $3)
		ORDER BY id DESC LIMIT $4`, f.PrincipalID, f.State, before, f.Limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("grants: listing grants: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Grant, error) {
		return scanGrant(r)
	})
	if err != nil {
		return Page{}, fmt.Errorf("grants: listing grants: %w", err)
	}
	page := Page{Items: items}
	if len(items) > f.Limit {
		page.Items = items[:f.Limit]
		page.NextCursor = strconv.FormatInt(page.Items[f.Limit-1].ID, 10)
	}
	if page.Items == nil {
		page.Items = []Grant{}
	}
	return page, nil
}

// insertGrantSQL records one grant; expires_at is computed on this
// database's clock.
const insertGrantSQL = `/* pg_sage guard_grant v1 */
INSERT INTO sage.guard_grants (database_id, principal_id, lane, capability, object_kind,
  object_oid, object_name, schema_oid, columns, privileges, grantor, expires_at,
  grant_action_id)
VALUES ($1::uuid, $2, 'broker', $3, $4, $5::int8::oid, $6, $7::int8::oid, $8, $9, $10,
  now() + make_interval(secs => $11), $12)
RETURNING ` + grantColumns

// openSchemaRow is the principal's active schema USAGE row, locked.
const openSchemaRowSQL = `/* pg_sage guard_grant v1 */ SELECT ` + grantColumns + `
FROM sage.guard_grants WHERE principal_id = $1 AND object_oid = $2::int8::oid
  AND object_kind = 'schema' AND state = 'active' AND revoked_at IS NULL
ORDER BY id LIMIT 1 FOR UPDATE`

// extendSchemaRowSQL keeps a schema row until its latest relation grant.
const extendSchemaRowSQL = `/* pg_sage guard_grant v1 */
UPDATE sage.guard_grants SET expires_at = GREATEST(expires_at, $2) WHERE id = $1
RETURNING ` + grantColumns

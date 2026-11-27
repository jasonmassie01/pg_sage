package onboarding

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableOwner is a role owning user tables whose privileges pg_sage's role
// does not have.
type TableOwner struct {
	Role      string
	Tables    int
	Superuser bool
}

// maxOwnerRoles bounds the owner roles the guide lists; the rest are only
// counted.
const maxOwnerRoles = 20

// tableOwnersSQL groups the user tables (tablesOwnedSQL's) that the role
// cannot act on as owner by their owner, most tables first. The window
// count is the number of owner roles before the LIMIT.
const tableOwnersSQL = `/* pg_sage onboarding */ SELECT o.rolname::text, o.rolsuper,
  count(*)::int, (count(*) OVER ())::int
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_roles o ON o.oid = c.relowner
WHERE ` + userTables + `
  AND NOT pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE')
GROUP BY o.oid, o.rolname, o.rolsuper
ORDER BY count(*) DESC, o.rolname
LIMIT $1`

// readTableOwners lists the owner roles of the tables the connected role
// does not own (at most maxOwnerRoles) and how many such roles there are.
func readTableOwners(ctx context.Context, pool *pgxpool.Pool) ([]TableOwner, int, error) {
	rows, err := pool.Query(ctx, tableOwnersSQL, maxOwnerRoles)
	if err != nil {
		return nil, 0, fmt.Errorf("read table owner roles: %w", err)
	}
	defer rows.Close()
	var out []TableOwner
	total := 0
	for rows.Next() {
		var o TableOwner
		if err := rows.Scan(&o.Role, &o.Superuser, &o.Tables, &total); err != nil {
			return nil, 0, fmt.Errorf("read table owner roles: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read table owner roles: %w", err)
	}
	return out, total, nil
}

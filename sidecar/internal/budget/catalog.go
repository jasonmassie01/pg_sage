package budget

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Catalog reads roles on the monitored database through bounded,
// read-only catalog statements (statement_timeout and lock_timeout from
// the safety settings).
type Catalog struct {
	reader catalogread.Reader
}

// NewCatalog reads through db with timeouts t.
func NewCatalog(db catalogread.Beginner, t catalogread.Timeouts) *Catalog {
	return &Catalog{reader: catalogread.New(db, t)}
}

// SelfRole is the role pg_sage connects as.
func (c *Catalog) SelfRole(ctx context.Context) (string, error) {
	var role string
	err := c.reader.QueryRow(ctx, "/* pg_sage budget */ SELECT session_user::text").Scan(&role)
	return role, err
}

// CurrentDatabase is the monitored database's datname.
func (c *Catalog) CurrentDatabase(ctx context.Context) (string, error) {
	var db string
	err := c.reader.QueryRow(ctx, "/* pg_sage budget */ SELECT current_database()::text").
		Scan(&db)
	return db, err
}

// roleFactsSQL reads at most 200 named roles: superuser, connection
// limit and the role's settings in this database.
const roleFactsSQL = `/* pg_sage budget */
SELECT r.rolname, r.rolsuper, r.rolconnlimit,
       COALESCE((SELECT s.setconfig FROM pg_db_role_setting s
                  WHERE s.setrole = r.oid
                    AND s.setdatabase = (SELECT oid FROM pg_database
                                          WHERE datname = current_database())), '{}')
  FROM pg_roles r
 WHERE r.rolname = ANY($1)
 ORDER BY r.rolname
 LIMIT 200`

// RoleFacts reads the named roles; a missing role is absent.
func (c *Catalog) RoleFacts(ctx context.Context, roles []string) (map[string]RoleFact, error) {
	rows, err := c.reader.Query(ctx, roleFactsSQL, roles)
	if err != nil {
		return nil, fmt.Errorf("read roles: %w", err)
	}
	defer rows.Close()
	out := map[string]RoleFact{}
	for rows.Next() {
		var name string
		var f RoleFact
		var config []string
		if err := rows.Scan(&name, &f.Superuser, &f.ConnLimit, &config); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		f.Settings = map[string]string{}
		for _, kv := range config {
			if k, v, ok := strings.Cut(kv, "="); ok {
				f.Settings[k] = v
			}
		}
		out[name] = f
	}
	return out, rows.Err()
}

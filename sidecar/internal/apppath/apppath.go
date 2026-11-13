// Package apppath sets the search_path pg_sage plans an application's
// statements on (workload EXPLAIN, HypoPG what-ifs): the application's
// own, read from pg_db_role_setting, never pg_sage's session path, which
// is pinned (pg_catalog, pg_temp) or set for pg_sage's own role.
package apppath

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the transaction the path is set in: a pooled connection inside
// BEGIN, or a pgx.Tx.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// applicationPathSQL reads the search_path every role of this database
// starts with: ALTER DATABASE (or ALTER ROLE ALL IN DATABASE), then ALTER
// ROLE ALL, then the server's value. reset_val is the server's value only
// while no setting names pg_sage's own login role (RESET would restore
// that one), so it is NULL then and PostgreSQL's default applies. A
// startup option in pg_sage's DSN would show in reset_val as well.
const applicationPathSQL = `
SELECT COALESCE(
  (SELECT pg_catalog.substr(c.setting, 13)
   FROM pg_catalog.pg_db_role_setting s,
        LATERAL pg_catalog.unnest(s.setconfig) AS c(setting)
   WHERE s.setrole = 0
     AND s.setdatabase IN (0, (SELECT d.oid FROM pg_catalog.pg_database d
                               WHERE d.datname = pg_catalog.current_database()))
     AND pg_catalog.starts_with(c.setting, 'search_path=')
   ORDER BY (s.setdatabase <> 0) DESC
   LIMIT 1),
  (SELECT p.reset_val FROM pg_catalog.pg_settings p
   WHERE p.name = 'search_path'
     AND NOT EXISTS (
       SELECT 1
       FROM pg_catalog.pg_db_role_setting s,
            LATERAL pg_catalog.unnest(s.setconfig) AS c(setting)
       WHERE s.setrole = (SELECT r.oid FROM pg_catalog.pg_roles r
                          WHERE r.rolname = SESSION_USER)
         AND pg_catalog.starts_with(c.setting, 'search_path='))))`

// setPathSQL sets the path for the current transaction only: the session
// keeps its own once the transaction ends.
const setPathSQL = "SELECT pg_catalog.set_config('search_path', $1, true)"

// defaultPath is PostgreSQL's built-in search_path.
const defaultPath = `"$user", public`

// Read returns the schemas of the application's search_path, without
// "$user": pg_sage does not know which application role a statement
// belongs to, and "$user" in pg_sage's session would be pg_sage's role.
func Read(ctx context.Context, db DB) ([]string, error) {
	if db == nil {
		return nil, errors.New("read the application search_path: no session")
	}
	var value *string
	if err := db.QueryRow(ctx, applicationPathSQL).Scan(&value); err != nil {
		return nil, fmt.Errorf("read the application search_path: %w", err)
	}
	setting := defaultPath
	if value != nil {
		setting = *value
	}
	path, err := parsePath(setting)
	if err != nil {
		return nil, fmt.Errorf("read the application search_path %q: %w", setting, err)
	}
	return path, nil
}

// Pin sets the application's search_path (Read) for db's current
// transaction. Every schema is quoted, so the names resolve exactly.
func Pin(ctx context.Context, db DB) error {
	path, err := Read(ctx, db)
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, setPathSQL, render(path)); err != nil {
		return fmt.Errorf("set the application search_path: %w", err)
	}
	return nil
}

// render quotes each schema and joins them as a search_path value.
func render(path []string) string {
	quoted := make([]string, len(path))
	for i, schema := range path {
		quoted[i] = pgx.Identifier{schema}.Sanitize()
	}
	return strings.Join(quoted, ", ")
}

// parsePath splits a search_path value as PostgreSQL does: elements
// separated by commas, a double-quoted one exact (a doubled quote is
// one), an unquoted one folded to lower case (A-Z only). "$user" is left out.
func parsePath(value string) ([]string, error) {
	var out []string
	rest := strings.TrimSpace(value)
	for rest != "" {
		elem, remainder, err := nextElement(rest)
		if err != nil {
			return nil, err
		}
		if elem != "$user" {
			out = append(out, elem)
		}
		rest = remainder
	}
	return out, nil
}

// nextElement reads the first element of s (no leading space) and
// returns it with the text after its separating comma.
func nextElement(s string) (elem, rest string, err error) {
	if strings.HasPrefix(s, `"`) {
		elem, rest, err = quotedElement(s)
	} else {
		end := strings.IndexAny(s, ",")
		if end < 0 {
			end = len(s)
		}
		elem, rest = strings.TrimSpace(s[:end]), s[end:]
		if strings.ContainsAny(elem, "\" \t\r\n") {
			return "", "", fmt.Errorf("unexpected character in element %q", elem)
		}
		elem = asciiLower(elem)
	}
	if err != nil {
		return "", "", err
	}
	if elem == "" {
		return "", "", errors.New("empty element")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return elem, "", nil
	}
	if !strings.HasPrefix(rest, ",") {
		return "", "", fmt.Errorf("expected a comma after %q", elem)
	}
	rest = strings.TrimSpace(rest[1:])
	if rest == "" {
		return "", "", errors.New("trailing comma")
	}
	return elem, rest, nil
}

// asciiLower folds A-Z only, as PostgreSQL folds an unquoted identifier
// in a multibyte encoding.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// quotedElement reads the double-quoted element s starts with.
func quotedElement(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		return b.String(), s[i+1:], nil
	}
	return "", "", errors.New("unterminated quoted element")
}

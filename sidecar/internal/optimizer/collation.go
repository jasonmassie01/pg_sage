package optimizer

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// collationSQL reads the database collation from pg_database: the
// lc_collate GUC was removed in PostgreSQL 16. The locale provider and
// ICU/builtin locale columns differ by version, so they are read through
// to_jsonb (absent columns are NULL instead of an error).
const collationSQL = `/* pg_sage */ SELECT datcollate,
       COALESCE(to_jsonb(d)->>'datlocprovider', 'c'),
       COALESCE(to_jsonb(d)->>'datlocale', to_jsonb(d)->>'daticulocale', '')
  FROM pg_catalog.pg_database d WHERE datname = current_database()`

// fetchCollation returns the database collation: datcollate for libc,
// "C" for the builtin provider (code-point order), "ICU <locale>" for ICU.
func fetchCollation(ctx context.Context, pool catalogread.Querier) (string, error) {
	var collate, provider, locale string
	err := pool.QueryRow(ctx, collationSQL).Scan(&collate, &provider, &locale)
	if err != nil {
		return "", fmt.Errorf("read pg_database collation: %w", err)
	}
	switch provider {
	case "b":
		return "C", nil
	case "i":
		return "ICU " + locale, nil
	}
	return collate, nil
}

package optimizer

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// collationSQL reads the database collation from pg_database: the
// lc_collate GUC was removed in PostgreSQL 16. The locale provider and
// ICU/builtin locale columns differ by version, so they are read through
// to_jsonb (absent columns are NULL instead of an error). The same read
// carries the server's major version (market-15b), so the version costs
// no extra round trip.
const collationSQL = `/* pg_sage optimizer */ SELECT datcollate,
       COALESCE(to_jsonb(d)->>'datlocprovider', 'c'),
       COALESCE(to_jsonb(d)->>'datlocale', to_jsonb(d)->>'daticulocale', ''),
       current_setting('server_version_num')::int / 10000
  FROM pg_catalog.pg_database d WHERE datname = current_database()`

// fetchCollation returns the database collation: datcollate for libc,
// "C" for the builtin provider (code-point order), "ICU <locale>" for ICU.
func fetchCollation(ctx context.Context, pool catalogread.Querier) (string, error) {
	collation, _, err := fetchCollationAndMajor(ctx, pool)
	return collation, err
}

// fetchCollationAndMajor returns the database collation and the server's
// major version (0 with the error when the read fails).
func fetchCollationAndMajor(ctx context.Context, pool catalogread.Querier,
) (string, int, error) {
	var collate, provider, locale string
	var major int
	err := pool.QueryRow(ctx, collationSQL).Scan(&collate, &provider, &locale, &major)
	if err != nil {
		return "", 0, fmt.Errorf("read pg_database collation and server version: %w", err)
	}
	switch provider {
	case "b":
		return "C", major, nil
	case "i":
		return "ICU " + locale, major, nil
	}
	return collate, major, nil
}

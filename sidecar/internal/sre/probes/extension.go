package probes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// errExtensionMissing is a probe whose extension is not installed in
// this database: the probe is unsupported, never "no rows".
var errExtensionMissing = errors.New("extension not installed")

const extensionSchemaSQL = `SELECT n.nspname::text FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname = $1`

// resolveExtension replaces ExtSchemaToken in sql with the quoted schema
// the extension is installed in, read inside the probe's own read-only
// transaction. The schema name comes from the catalog and is quoted as
// an identifier; nothing else of the SQL changes.
func resolveExtension(ctx context.Context, tx pgx.Tx, extension, sql string) (string,
	error) {
	if extension == "" {
		return sql, nil
	}
	var schema string
	err := tx.QueryRow(ctx, extensionSchemaSQL, extension).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", errExtensionMissing, extension)
	}
	if err != nil {
		return "", fmt.Errorf("resolve extension %s: %w", extension, err)
	}
	return strings.ReplaceAll(sql, ExtSchemaToken, pgx.Identifier{schema}.Sanitize()), nil
}

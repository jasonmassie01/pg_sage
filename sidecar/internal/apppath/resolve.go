package apppath

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// resolveSQL names the relation $1 resolves to on the transaction's
// search_path, schema-qualified and quoted (NULL when there is none). It
// runs on the application's path, which an owner may order with
// pg_catalog last, so every function and operator is named with its
// schema.
const resolveSQL = `SELECT (SELECT pg_catalog.quote_ident(n.nspname)
	OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c.relname)
	FROM pg_catalog.pg_class c, pg_catalog.pg_namespace n
	WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)::pg_catalog.oid
	  AND n.oid OPERATOR(pg_catalog.=) c.relnamespace)`

// Resolve names the relation that name, as an application's SQL writes it
// ([schema.]relation, quoted or not), resolves to on the application's
// search_path: schema-qualified and quoted, so it resolves to the same
// relation on pg_sage's pinned path (security-01c-n). It is "" when no
// such relation is on the application's path. It reads in one bounded
// read-only transaction of db, always rolled back.
func Resolve(ctx context.Context, db catalogread.Beginner, t catalogread.Timeouts,
	name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("resolve a relation on the application search_path: no name")
	}
	tx, err := catalogread.Begin(ctx, db, t)
	if err != nil {
		return "", fmt.Errorf("resolve %s on the application search_path: %w", name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := Pin(ctx, tx); err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	var qualified *string
	if err := tx.QueryRow(ctx, resolveSQL, name).Scan(&qualified); err != nil {
		return "", fmt.Errorf("resolve %s on the application search_path: %w", name, err)
	}
	if qualified == nil {
		return "", nil
	}
	return *qualified, nil
}

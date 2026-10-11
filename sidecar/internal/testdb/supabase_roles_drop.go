package testdb

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DropSupabaseRoles drops anon and authenticated if they exist, after
// dropping what they own and hold in dsn's database.
func DropSupabaseRoles(ctx context.Context, dsn string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, role := range SupabaseRoles {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles "+
			"WHERE rolname = $1)", role).Scan(&exists); err != nil {
			return fmt.Errorf("look up role %s: %w", role, err)
		}
		if !exists {
			continue
		}
		id := pgx.Identifier{role}.Sanitize()
		for _, stmt := range []string{"DROP OWNED BY " + id, "DROP ROLE " + id} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	return nil
}

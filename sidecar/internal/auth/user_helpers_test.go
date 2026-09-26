package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Test-only lookups. The production GetUserByID, CountAdmins,
// DeleteUser and UpdateUserRole had no callers (G6-D06); handlers use
// the *PreservingAdmin variants. These helpers keep the invariant
// tests readable without shipping dead exported API.

func lookupUserForTest(
	ctx context.Context, pool *pgxpool.Pool, id int,
) (*User, error) {
	var u User
	err := pool.QueryRow(ctx,
		"SELECT id, email, role, created_at, last_login "+
			"FROM sage.users WHERE id = $1", id,
	).Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &u.LastLogin)
	if err != nil {
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return &u, nil
}

func countAdminsForTest(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.users WHERE role = $1", RoleAdmin).Scan(&n)
	return n, err
}

func deleteUserForTest(ctx context.Context, pool *pgxpool.Pool, id int) error {
	_, err := pool.Exec(ctx, "DELETE FROM sage.users WHERE id = $1", id)
	return err
}

func setRoleForTest(
	ctx context.Context, pool *pgxpool.Pool, id int, role string,
) error {
	_, err := pool.Exec(ctx,
		"UPDATE sage.users SET role = $1 WHERE id = $2", role, id)
	return err
}

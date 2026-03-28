//go:build integration

package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	// Ensure schema and tables exist.
	ctx := context.Background()
	_, err = pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS sage")
	if err != nil {
		t.Fatalf("creating schema: %v", err)
	}
	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sage.users (
			id SERIAL PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			password TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'viewer',
			created_at TIMESTAMPTZ DEFAULT now(),
			last_login TIMESTAMPTZ
		)`)
	if err != nil {
		t.Fatalf("creating users table: %v", err)
	}
	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sage.sessions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id INT REFERENCES sage.users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ DEFAULT now()
		)`)
	if err != nil {
		t.Fatalf("creating sessions table: %v", err)
	}

	// Clean up before test.
	_, _ = pool.Exec(ctx, "DELETE FROM sage.sessions")
	_, _ = pool.Exec(ctx, "DELETE FROM sage.users")

	return pool
}

func TestCreateUser_Valid(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"test@example.com", "password123", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}
}

func TestCreateUser_DuplicateEmail(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool,
		"dup@example.com", "pass1", RoleViewer)
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}

	_, err = CreateUser(ctx, pool,
		"dup@example.com", "pass2", RoleAdmin)
	if err == nil {
		t.Fatal("expected error for duplicate email")
	}
}

func TestCreateUser_InvalidRole(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool,
		"bad@example.com", "pass", "superadmin")
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
}

func TestAuthenticate_Valid(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool,
		"auth@example.com", "secret", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := Authenticate(ctx, pool,
		"auth@example.com", "secret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.Email != "auth@example.com" {
		t.Errorf("email: %q", user.Email)
	}
	if user.Role != RoleAdmin {
		t.Errorf("role: %q", user.Role)
	}
}

func TestAuthenticate_WrongPassword(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool,
		"wrong@example.com", "correct", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = Authenticate(ctx, pool,
		"wrong@example.com", "incorrect")
	if err == nil {
		t.Fatal("expected error for wrong password")
	}
}

func TestAuthenticate_NonexistentUser(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := Authenticate(ctx, pool,
		"nobody@example.com", "pass")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
}

func TestCreateSession_And_Validate(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"sess@example.com", "pass", RoleOperator)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	sessionID, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("session ID is empty")
	}

	user, err := ValidateSession(ctx, pool, sessionID)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if user.Email != "sess@example.com" {
		t.Errorf("email: %q", user.Email)
	}
}

func TestValidateSession_Expired(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"exp@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Insert an already-expired session directly.
	var sessionID string
	err = pool.QueryRow(ctx,
		"INSERT INTO sage.sessions (user_id, expires_at) "+
			"VALUES ($1, $2) RETURNING id",
		id, time.Now().Add(-1*time.Hour),
	).Scan(&sessionID)
	if err != nil {
		t.Fatalf("insert expired session: %v", err)
	}

	_, err = ValidateSession(ctx, pool, sessionID)
	if err == nil {
		t.Fatal("expected error for expired session")
	}
}

func TestValidateSession_Nonexistent(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := ValidateSession(ctx, pool,
		"00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestDeleteSession(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"del@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	sessionID, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err = DeleteSession(ctx, pool, sessionID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	_, err = ValidateSession(ctx, pool, sessionID)
	if err == nil {
		t.Fatal("expected error after session deletion")
	}
}

func TestCleanExpiredSessions(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"clean@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Insert expired session.
	_, err = pool.Exec(ctx,
		"INSERT INTO sage.sessions (user_id, expires_at) "+
			"VALUES ($1, $2)",
		id, time.Now().Add(-1*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert expired session: %v", err)
	}

	// Insert valid session.
	validID, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err = CleanExpiredSessions(ctx, pool)
	if err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}

	// Valid session should still exist.
	_, err = ValidateSession(ctx, pool, validID)
	if err != nil {
		t.Errorf("valid session was cleaned: %v", err)
	}
}

func TestListUsers(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool,
		"list1@example.com", "pass", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err = CreateUser(ctx, pool,
		"list2@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
}

func TestDeleteUser(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"deluser@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = DeleteUser(ctx, pool, id)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// Verify deleted.
	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("expected 0 users after delete, got %d",
			len(users))
	}
}

func TestUpdateUserRole_Valid(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"role@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = UpdateUserRole(ctx, pool, id, RoleAdmin)
	if err != nil {
		t.Fatalf("UpdateUserRole: %v", err)
	}

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if users[0].Role != RoleAdmin {
		t.Errorf("role: %q, want %q",
			users[0].Role, RoleAdmin)
	}
}

func TestUpdateUserRole_InvalidRole(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"badrole@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = UpdateUserRole(ctx, pool, id, "superadmin")
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
}

func TestBootstrapAdmin_CreatesFirst(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	err := BootstrapAdmin(ctx, pool,
		"admin@example.com", "admin123")
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	if users[0].Role != RoleAdmin {
		t.Errorf("role: %q, want admin", users[0].Role)
	}
}

func TestBootstrapAdmin_RejectsSecond(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	err := BootstrapAdmin(ctx, pool,
		"first@example.com", "pass1")
	if err != nil {
		t.Fatalf("first BootstrapAdmin: %v", err)
	}

	err = BootstrapAdmin(ctx, pool,
		"second@example.com", "pass2")
	if err == nil {
		t.Fatal("expected error for second bootstrap")
	}
}

func TestUserCount(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	count, err := UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	_, err = CreateUser(ctx, pool,
		"count@example.com", "pass", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	count, err = UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}
}

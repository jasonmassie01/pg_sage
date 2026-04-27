package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setupPhase2Pool connects to a local PostgreSQL, bootstraps the sage
// schema with users and sessions tables, truncates them, and returns
// the pool.  Tests are skipped when the database is unreachable.
func setupPhase2Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	ctx := context.Background()

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Skipf("skipping: cannot parse DSN: %v", err)
	}
	// Single connection so advisory lock stays on the same session.
	poolCfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("skipping: cannot create pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: database unavailable: %v", err)
	}

	// Hold the pg_sage advisory lock for the entire test to prevent
	// schema tests from dropping the sage schema mid-test.
	_, err = pool.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext('pg_sage'))")
	if err != nil {
		pool.Close()
		t.Fatalf("acquiring advisory lock: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"SELECT pg_advisory_unlock(hashtext('pg_sage'))")
		pool.Close()
	})

	// Bootstrap schema and tables.
	_, err = pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS sage;

		CREATE TABLE IF NOT EXISTS sage.users (
			id          SERIAL PRIMARY KEY,
			email       TEXT UNIQUE NOT NULL,
			password    TEXT NOT NULL,
			role        TEXT NOT NULL DEFAULT 'viewer',
			created_at  TIMESTAMPTZ DEFAULT now(),
			last_login  TIMESTAMPTZ
		);

		CREATE TABLE IF NOT EXISTS sage.sessions (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id     INT REFERENCES sage.users(id) ON DELETE CASCADE,
			expires_at  TIMESTAMPTZ NOT NULL,
			created_at  TIMESTAMPTZ DEFAULT now()
		);

		ALTER TABLE sage.users
			ADD COLUMN IF NOT EXISTS oauth_provider TEXT DEFAULT '';
		ALTER TABLE sage.users
			ALTER COLUMN password DROP NOT NULL;
	`)
	if err != nil {
		t.Fatalf("bootstrap DDL failed: %v", err)
	}

	// Clean slate for each test.
	if _, err := pool.Exec(ctx, "TRUNCATE sage.users CASCADE"); err != nil {
		t.Fatalf("truncating users: %v", err)
	}

	return pool
}

// -------------------------------------------------------------------------
// CreateUser
// -------------------------------------------------------------------------

func TestPhase2_CreateUser_HappyPath(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool, "alice@example.com", "password123", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}

	// Verify the row exists with correct fields.
	var email, role string
	err = pool.QueryRow(ctx,
		"SELECT email, role FROM sage.users WHERE id = $1", id,
	).Scan(&email, &role)
	if err != nil {
		t.Fatalf("querying created user: %v", err)
	}
	if email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", email)
	}
	if role != RoleAdmin {
		t.Errorf("role = %q, want %q", role, RoleAdmin)
	}
}

func TestPhase2_CreateUser_AllRoles(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	roles := []string{RoleAdmin, RoleOperator, RoleViewer}
	for i, role := range roles {
		email := strings.Replace("roleN@example.com", "N",
			strings.Repeat("x", i+1), 1)
		id, err := CreateUser(ctx, pool, email, "password", role)
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", role, err)
		}
		if id <= 0 {
			t.Errorf("role %s: expected positive ID, got %d", role, id)
		}
	}
}

func TestPhase2_CreateUser_InvalidRole(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool, "bad@example.com", "password", "superadmin")
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
	if !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("error = %q, want it to contain 'invalid role'", err)
	}
}

func TestPhase2_CreateUser_DuplicateEmail(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool, "dup@example.com", "password1", RoleViewer)
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err = CreateUser(ctx, pool, "dup@example.com", "password2", RoleAdmin)
	if err == nil {
		t.Fatal("expected error for duplicate email")
	}
	if !strings.Contains(err.Error(), "creating user") {
		t.Errorf("error = %q, want it to contain 'creating user'", err)
	}
}

func TestPhase2_CreateUser_EmptyEmail(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Empty email violates NOT NULL / unique constraint.
	_, err := CreateUser(ctx, pool, "", "password", RoleViewer)
	// This should succeed at the Go level (no input validation on email),
	// but a second insert with "" would fail due to UNIQUE.
	if err != nil {
		// Some DBs reject empty strings on UNIQUE TEXT — either way is fine.
		return
	}
	// If first succeeded, second should fail.
	_, err = CreateUser(ctx, pool, "", "password", RoleViewer)
	if err == nil {
		t.Fatal("expected error for duplicate empty email")
	}
}

func TestPhase2_CreateUser_EmptyPassword(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool, "empty-pw@example.com", "", RoleViewer)
	if err == nil {
		t.Fatal("expected error for empty password")
	}
	if !strings.Contains(err.Error(), "at least 8 characters") {
		t.Errorf("error = %q, want password length error", err)
	}
}

// -------------------------------------------------------------------------
// Authenticate
// -------------------------------------------------------------------------

func TestPhase2_Authenticate_HappyPath(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool, "auth@example.com", "secret88", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := Authenticate(ctx, pool, "auth@example.com", "secret88")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user == nil {
		t.Fatal("Authenticate returned nil user")
	}
	if user.Email != "auth@example.com" {
		t.Errorf("Email = %q, want auth@example.com", user.Email)
	}
	if user.Role != RoleAdmin {
		t.Errorf("Role = %q, want %q", user.Role, RoleAdmin)
	}
	if user.ID <= 0 {
		t.Errorf("ID = %d, want positive", user.ID)
	}
}

func TestPhase2_Authenticate_UpdatesLastLogin(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool, "login@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Before auth, last_login should be NULL.
	var lastLogin *time.Time
	err = pool.QueryRow(ctx,
		"SELECT last_login FROM sage.users WHERE id = $1", id,
	).Scan(&lastLogin)
	if err != nil {
		t.Fatalf("querying last_login before auth: %v", err)
	}
	if lastLogin != nil {
		t.Errorf("last_login before auth = %v, want nil", lastLogin)
	}

	_, err = Authenticate(ctx, pool, "login@example.com", "password")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// After auth, last_login should be set.
	err = pool.QueryRow(ctx,
		"SELECT last_login FROM sage.users WHERE id = $1", id,
	).Scan(&lastLogin)
	if err != nil {
		t.Fatalf("querying last_login after auth: %v", err)
	}
	if lastLogin == nil {
		t.Error("last_login after auth is nil, want non-nil")
	}
}

func TestPhase2_Authenticate_WrongPassword(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool, "wrong@example.com", "correct8", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = Authenticate(ctx, pool, "wrong@example.com", "incorrect8")
	if err == nil {
		t.Fatal("expected error for wrong password")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %q, want 'invalid credentials'", err)
	}
}

func TestPhase2_Authenticate_NonexistentUser(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := Authenticate(ctx, pool, "nobody@example.com", "password")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %q, want 'invalid credentials'", err)
	}
}

func TestPhase2_Authenticate_OAuthUserNoPassword(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Create an OAuth user (password is NULL).
	_, err := FindOrCreateOAuthUser(ctx, pool,
		"oauth-only@example.com", "github", RoleViewer)
	if err != nil {
		t.Fatalf("FindOrCreateOAuthUser: %v", err)
	}

	// Authenticate with any password should fail (hash is NULL).
	_, err = Authenticate(ctx, pool, "oauth-only@example.com", "anything")
	if err == nil {
		t.Fatal("expected error authenticating OAuth user with password")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %q, want 'invalid credentials'", err)
	}
}

// -------------------------------------------------------------------------
// CreateSession / ValidateSession / DeleteSession lifecycle
// -------------------------------------------------------------------------

func TestPhase2_SessionLifecycle(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	userID, err := CreateUser(ctx, pool,
		"session@example.com", "password", RoleOperator)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create session.
	sessionID, err := CreateSession(ctx, pool, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("session ID is empty")
	}

	// Validate session.
	user, err := ValidateSession(ctx, pool, sessionID)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if user == nil {
		t.Fatal("ValidateSession returned nil user")
	}
	if user.Email != "session@example.com" {
		t.Errorf("Email = %q, want session@example.com", user.Email)
	}
	if user.Role != RoleOperator {
		t.Errorf("Role = %q, want %q", user.Role, RoleOperator)
	}
	if user.ID != userID {
		t.Errorf("ID = %d, want %d", user.ID, userID)
	}

	// Delete session.
	err = DeleteSession(ctx, pool, sessionID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Validate after delete should fail.
	_, err = ValidateSession(ctx, pool, sessionID)
	if err == nil {
		t.Fatal("expected error validating deleted session")
	}
	if !strings.Contains(err.Error(), "session invalid or expired") {
		t.Errorf("error = %q, want 'session invalid or expired'", err)
	}
}

func TestPhase2_ValidateSession_Nonexistent(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := ValidateSession(ctx, pool,
		"00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
	if !strings.Contains(err.Error(), "session invalid or expired") {
		t.Errorf("error = %q, want 'session invalid or expired'", err)
	}
}

func TestPhase2_ValidateSession_Expired(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	userID, err := CreateUser(ctx, pool,
		"expired@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Insert an already-expired session directly.
	var sessionID string
	err = pool.QueryRow(ctx,
		"INSERT INTO sage.sessions (user_id, expires_at) "+
			"VALUES ($1, $2) RETURNING id",
		userID, time.Now().Add(-1*time.Hour),
	).Scan(&sessionID)
	if err != nil {
		t.Fatalf("inserting expired session: %v", err)
	}

	_, err = ValidateSession(ctx, pool, sessionID)
	if err == nil {
		t.Fatal("expected error for expired session")
	}
}

func TestPhase2_ValidateSession_InvalidUUID(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := ValidateSession(ctx, pool, "not-a-uuid")
	if err == nil {
		t.Fatal("expected error for invalid UUID")
	}
}

func TestPhase2_DeleteSession_Nonexistent(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Deleting a nonexistent session should not error (DELETE WHERE
	// affects 0 rows, which is not treated as an error).
	err := DeleteSession(ctx, pool,
		"00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("DeleteSession nonexistent: %v", err)
	}
}

func TestPhase2_CreateSession_MultipleSessions(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	userID, err := CreateUser(ctx, pool,
		"multi@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	s1, err := CreateSession(ctx, pool, userID)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	s2, err := CreateSession(ctx, pool, userID)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	if s1 == s2 {
		t.Error("two sessions should have different IDs")
	}

	// Both should be valid.
	if _, err := ValidateSession(ctx, pool, s1); err != nil {
		t.Errorf("session 1 invalid: %v", err)
	}
	if _, err := ValidateSession(ctx, pool, s2); err != nil {
		t.Errorf("session 2 invalid: %v", err)
	}

	// Delete one; the other should remain valid.
	if err := DeleteSession(ctx, pool, s1); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := ValidateSession(ctx, pool, s1); err == nil {
		t.Error("deleted session 1 should be invalid")
	}
	if _, err := ValidateSession(ctx, pool, s2); err != nil {
		t.Errorf("session 2 should still be valid: %v", err)
	}
}

// -------------------------------------------------------------------------
// CleanExpiredSessions
// -------------------------------------------------------------------------

func TestPhase2_CleanExpiredSessions(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	userID, err := CreateUser(ctx, pool,
		"clean@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Insert expired session directly.
	_, err = pool.Exec(ctx,
		"INSERT INTO sage.sessions (user_id, expires_at) "+
			"VALUES ($1, $2)",
		userID, time.Now().Add(-1*time.Hour),
	)
	if err != nil {
		t.Fatalf("inserting expired session: %v", err)
	}

	// Insert valid session.
	validID, err := CreateSession(ctx, pool, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err = CleanExpiredSessions(ctx, pool)
	if err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}

	// Valid session should survive.
	_, err = ValidateSession(ctx, pool, validID)
	if err != nil {
		t.Errorf("valid session was cleaned: %v", err)
	}

	// Count remaining sessions: should be exactly 1.
	var count int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.sessions WHERE user_id = $1",
		userID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 session after cleanup, got %d", count)
	}
}

func TestPhase2_CleanExpiredSessions_NoneExpired(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	userID, err := CreateUser(ctx, pool,
		"noexpire@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = CreateSession(ctx, pool, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Cleaning when nothing is expired should not error.
	err = CleanExpiredSessions(ctx, pool)
	if err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}

	var count int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.sessions",
	).Scan(&count)
	if err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 session (none cleaned), got %d", count)
	}
}

func TestPhase2_CleanExpiredSessions_Empty(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// No sessions at all — should not error.
	err := CleanExpiredSessions(ctx, pool)
	if err != nil {
		t.Fatalf("CleanExpiredSessions on empty table: %v", err)
	}
}

// -------------------------------------------------------------------------
// ListUsers
// -------------------------------------------------------------------------

func TestPhase2_ListUsers_Empty(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("expected 0 users, got %d", len(users))
	}
}

func TestPhase2_ListUsers_MultipleUsers(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	emails := []string{"a@example.com", "b@example.com", "c@example.com"}
	for _, email := range emails {
		_, err := CreateUser(ctx, pool, email, "password", RoleViewer)
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
	}

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}

	// Verify ordering by ID (ascending).
	for i := 1; i < len(users); i++ {
		if users[i].ID <= users[i-1].ID {
			t.Errorf("users not ordered by ID: %d <= %d",
				users[i].ID, users[i-1].ID)
		}
	}

	// Verify emails match.
	for i, email := range emails {
		if users[i].Email != email {
			t.Errorf("users[%d].Email = %q, want %q",
				i, users[i].Email, email)
		}
	}
}

func TestPhase2_ListUsers_DoesNotExposePassword(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := CreateUser(ctx, pool,
		"nopw@example.com", "supersecret", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	// The User struct has no Password field — this is a structural check.
	// We can only verify the struct doesn't contain the password string
	// by checking the type has no such field (compile-time guarantee).
	// Runtime check: the user returned has the correct non-password fields.
	if users[0].Email != "nopw@example.com" {
		t.Errorf("Email = %q", users[0].Email)
	}
	if users[0].Role != RoleViewer {
		t.Errorf("Role = %q", users[0].Role)
	}
}

// -------------------------------------------------------------------------
// DeleteUser
// -------------------------------------------------------------------------

func TestPhase2_DeleteUser_HappyPath(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"delete-me@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = DeleteUser(ctx, pool, id)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("expected 0 users, got %d", len(users))
	}
}

func TestPhase2_DeleteUser_Nonexistent(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	err := DeleteUser(ctx, pool, 99999)
	if err == nil {
		t.Fatal("expected error deleting nonexistent user")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("error = %q, want 'user not found'", err)
	}
}

func TestPhase2_DeleteUser_CascadesSessions(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"cascade@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	sessionID, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err = DeleteUser(ctx, pool, id)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// Session should be gone due to ON DELETE CASCADE.
	_, err = ValidateSession(ctx, pool, sessionID)
	if err == nil {
		t.Fatal("expected session to be cascaded on user delete")
	}
}

// -------------------------------------------------------------------------
// UpdateUserRole
// -------------------------------------------------------------------------

func TestPhase2_UpdateUserRole_HappyPath(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"role-change@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = UpdateUserRole(ctx, pool, id, RoleAdmin)
	if err != nil {
		t.Fatalf("UpdateUserRole: %v", err)
	}

	// Verify role changed.
	var role string
	err = pool.QueryRow(ctx,
		"SELECT role FROM sage.users WHERE id = $1", id,
	).Scan(&role)
	if err != nil {
		t.Fatalf("querying role: %v", err)
	}
	if role != RoleAdmin {
		t.Errorf("role = %q, want %q", role, RoleAdmin)
	}
}

func TestPhase2_UpdateUserRole_AllTransitions(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"transitions@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	transitions := []string{
		RoleOperator, RoleAdmin, RoleViewer,
		RoleAdmin, RoleOperator, RoleViewer,
	}
	for _, newRole := range transitions {
		err = UpdateUserRole(ctx, pool, id, newRole)
		if err != nil {
			t.Fatalf("UpdateUserRole to %q: %v", newRole, err)
		}
		var got string
		err = pool.QueryRow(ctx,
			"SELECT role FROM sage.users WHERE id = $1", id,
		).Scan(&got)
		if err != nil {
			t.Fatalf("querying role: %v", err)
		}
		if got != newRole {
			t.Errorf("after transition: role = %q, want %q", got, newRole)
		}
	}
}

func TestPhase2_UpdateUserRole_InvalidRole(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"badrole2@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = UpdateUserRole(ctx, pool, id, "superadmin")
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
	if !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("error = %q, want 'invalid role'", err)
	}

	// Verify role was NOT changed.
	var role string
	err = pool.QueryRow(ctx,
		"SELECT role FROM sage.users WHERE id = $1", id,
	).Scan(&role)
	if err != nil {
		t.Fatalf("querying role: %v", err)
	}
	if role != RoleViewer {
		t.Errorf("role should still be %q, got %q", RoleViewer, role)
	}
}

func TestPhase2_UpdateUserRole_NonexistentUser(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	err := UpdateUserRole(ctx, pool, 99999, RoleAdmin)
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("error = %q, want 'user not found'", err)
	}
}

// -------------------------------------------------------------------------
// GetUserByID
// -------------------------------------------------------------------------

func TestPhase2_GetUserByID_Found(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"lookup@example.com", "password", RoleOperator)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := GetUserByID(ctx, pool, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user == nil {
		t.Fatal("GetUserByID returned nil user")
	}
	if user.ID != id {
		t.Errorf("ID = %d, want %d", user.ID, id)
	}
	if user.Email != "lookup@example.com" {
		t.Errorf("Email = %q, want lookup@example.com", user.Email)
	}
	if user.Role != RoleOperator {
		t.Errorf("Role = %q, want %q", user.Role, RoleOperator)
	}
	if user.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if user.LastLogin != nil {
		t.Errorf("LastLogin = %v, want nil before login", user.LastLogin)
	}
}

func TestPhase2_GetUserByID_NotFound(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	user, err := GetUserByID(ctx, pool, 99999)
	if err == nil {
		t.Fatal("expected error for missing user")
	}
	if user != nil {
		t.Fatalf("user = %#v, want nil on error", user)
	}
	if !strings.Contains(err.Error(), "getting user") {
		t.Errorf("error = %q, want getting user context", err)
	}
}

func TestPhase2_GetUserByID_CancelledContext(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	user, err := GetUserByID(ctx, pool, 1)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if user != nil {
		t.Fatalf("user = %#v, want nil on error", user)
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %q, want context canceled", err)
	}
}

// -------------------------------------------------------------------------
// UpdateUserRolePreservingAdmin
// -------------------------------------------------------------------------

func TestPhase2_UpdateUserRolePreservingAdmin_RejectsOnlyAdminDemotion(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	adminID, err := CreateUser(ctx, pool,
		"only-admin@example.com", "password", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}

	err = UpdateUserRolePreservingAdmin(ctx, pool, adminID, RoleViewer)
	if err == nil {
		t.Fatal("expected ErrLastAdmin")
	}
	if err != ErrLastAdmin {
		t.Fatalf("error = %v, want %v", err, ErrLastAdmin)
	}

	user, err := GetUserByID(ctx, pool, adminID)
	if err != nil {
		t.Fatalf("GetUserByID after failed demotion: %v", err)
	}
	if user.Role != RoleAdmin {
		t.Errorf("role after ErrLastAdmin = %q, want %q", user.Role, RoleAdmin)
	}
}

func TestPhase2_UpdateUserRolePreservingAdmin_AllowsOneOfTwoAdminsDemotion(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	firstID, err := CreateUser(ctx, pool,
		"first-admin@example.com", "password", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser first admin: %v", err)
	}
	if _, err := CreateUser(ctx, pool,
		"second-admin@example.com", "password", RoleAdmin); err != nil {
		t.Fatalf("CreateUser second admin: %v", err)
	}

	err = UpdateUserRolePreservingAdmin(ctx, pool, firstID, RoleOperator)
	if err != nil {
		t.Fatalf("UpdateUserRolePreservingAdmin: %v", err)
	}

	user, err := GetUserByID(ctx, pool, firstID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.Role != RoleOperator {
		t.Errorf("role = %q, want %q", user.Role, RoleOperator)
	}
	count, err := CountAdmins(ctx, pool)
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if count != 1 {
		t.Errorf("admin count = %d, want 1", count)
	}
}

func TestPhase2_UpdateUserRolePreservingAdmin_MissingUser(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	err := UpdateUserRolePreservingAdmin(ctx, pool, 99999, RoleAdmin)
	if err == nil {
		t.Fatal("expected ErrUserNotFound")
	}
	if err != ErrUserNotFound {
		t.Fatalf("error = %v, want %v", err, ErrUserNotFound)
	}
}

func TestPhase2_UpdateUserRolePreservingAdmin_InvalidRolePreservesRole(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"invalid-preserve@example.com", "password", RoleOperator)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = UpdateUserRolePreservingAdmin(ctx, pool, id, "superadmin")
	if err == nil {
		t.Fatal("expected invalid role error")
	}
	if !strings.Contains(err.Error(), ErrInvalidRole.Error()) {
		t.Fatalf("error = %v, want %v", err, ErrInvalidRole)
	}

	user, err := GetUserByID(ctx, pool, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.Role != RoleOperator {
		t.Errorf("role after invalid update = %q, want %q",
			user.Role, RoleOperator)
	}
}

// -------------------------------------------------------------------------
// FindOrCreateOAuthUser
// -------------------------------------------------------------------------

func TestPhase2_FindOrCreateOAuthUser_CreatesNew(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	user, err := FindOrCreateOAuthUser(ctx, pool,
		"new-oauth@example.com", "github", RoleViewer)
	if err != nil {
		t.Fatalf("FindOrCreateOAuthUser: %v", err)
	}
	if user == nil {
		t.Fatal("returned nil user")
	}
	if user.Email != "new-oauth@example.com" {
		t.Errorf("Email = %q, want new-oauth@example.com", user.Email)
	}
	if user.Role != RoleViewer {
		t.Errorf("Role = %q, want %q", user.Role, RoleViewer)
	}
	if user.ID <= 0 {
		t.Errorf("ID = %d, want positive", user.ID)
	}

	// Verify oauth_provider stored.
	var provider string
	err = pool.QueryRow(ctx,
		"SELECT oauth_provider FROM sage.users WHERE id = $1",
		user.ID,
	).Scan(&provider)
	if err != nil {
		t.Fatalf("querying oauth_provider: %v", err)
	}
	if provider != "github" {
		t.Errorf("oauth_provider = %q, want github", provider)
	}
}

func TestPhase2_FindOrCreateOAuthUser_FindsExisting(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Create via OAuth first.
	u1, err := FindOrCreateOAuthUser(ctx, pool,
		"existing-oauth@example.com", "github", RoleViewer)
	if err != nil {
		t.Fatalf("first FindOrCreateOAuthUser: %v", err)
	}

	// Find the same user.
	u2, err := FindOrCreateOAuthUser(ctx, pool,
		"existing-oauth@example.com", "github", RoleAdmin)
	if err != nil {
		t.Fatalf("second FindOrCreateOAuthUser: %v", err)
	}
	if u2.ID != u1.ID {
		t.Errorf("expected same user ID %d, got %d", u1.ID, u2.ID)
	}
	// Role should NOT change on find (stays at original).
	if u2.Role != RoleViewer {
		t.Errorf("Role = %q, want %q (should not change on find)",
			u2.Role, RoleViewer)
	}
}

func TestPhase2_FindOrCreateOAuthUser_DefaultRoleEmpty(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Empty defaultRole should default to "viewer".
	user, err := FindOrCreateOAuthUser(ctx, pool,
		"default-role@example.com", "google", "")
	if err != nil {
		t.Fatalf("FindOrCreateOAuthUser: %v", err)
	}
	if user.Role != RoleViewer {
		t.Errorf("Role = %q, want %q (default)", user.Role, RoleViewer)
	}
}

func TestPhase2_FindOrCreateOAuthUser_InvalidRole(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	_, err := FindOrCreateOAuthUser(ctx, pool,
		"invalid-role-oauth@example.com", "github", "superadmin")
	if err == nil {
		t.Fatal("expected error for invalid default role")
	}
	if !strings.Contains(err.Error(), "invalid default role") {
		t.Errorf("error = %q, want 'invalid default role'", err)
	}
}

func TestPhase2_FindOrCreateOAuthUser_FindsPasswordUser(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Create a regular password user.
	_, err := CreateUser(ctx, pool,
		"pw-user@example.com", "password", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// FindOrCreate with same email should return the existing user.
	user, err := FindOrCreateOAuthUser(ctx, pool,
		"pw-user@example.com", "google", RoleViewer)
	if err != nil {
		t.Fatalf("FindOrCreateOAuthUser: %v", err)
	}
	if user.Role != RoleAdmin {
		t.Errorf("Role = %q, want %q (original role preserved)",
			user.Role, RoleAdmin)
	}
}

// -------------------------------------------------------------------------
// UserCount
// -------------------------------------------------------------------------

func TestPhase2_UserCount_Zero(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	count, err := UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestPhase2_UserCount_AfterCreates(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		email := strings.Replace("countN@example.com", "N",
			strings.Repeat("x", i+1), 1)
		_, err := CreateUser(ctx, pool, email, "password", RoleViewer)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}

	count, err := UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5, got %d", count)
	}
}

func TestPhase2_UserCount_AfterDelete(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	id, err := CreateUser(ctx, pool,
		"count-del@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	count, err := UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	err = DeleteUser(ctx, pool, id)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	count, err = UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount after delete: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 after delete, got %d", count)
	}
}

// -------------------------------------------------------------------------
// BootstrapAdmin
// -------------------------------------------------------------------------

func TestPhase2_BootstrapAdmin_CreatesFirstAdmin(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	err := BootstrapAdmin(ctx, pool, "admin@example.com", "admin123")
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
	if users[0].Email != "admin@example.com" {
		t.Errorf("Email = %q, want admin@example.com", users[0].Email)
	}
	if users[0].Role != RoleAdmin {
		t.Errorf("Role = %q, want %q", users[0].Role, RoleAdmin)
	}

	// Should be able to authenticate with the password.
	user, err := Authenticate(ctx, pool, "admin@example.com", "admin123")
	if err != nil {
		t.Fatalf("Authenticate bootstrapped admin: %v", err)
	}
	if user.Role != RoleAdmin {
		t.Errorf("authenticated role = %q, want admin", user.Role)
	}
}

func TestPhase2_BootstrapAdmin_RejectsWhenUsersExist(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Create a user first.
	_, err := CreateUser(ctx, pool,
		"existing@example.com", "password", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = BootstrapAdmin(ctx, pool, "admin@example.com", "admin123")
	if err == nil {
		t.Fatal("expected error when users already exist")
	}
	if !strings.Contains(err.Error(), "users already exist") {
		t.Errorf("error = %q, want 'users already exist'", err)
	}
}

func TestPhase2_BootstrapAdmin_RejectsSecondCall(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// Passwords must be at least 8 characters per the post-hardening
	// auth policy. Use distinct 12-char passwords for both calls.
	err := BootstrapAdmin(ctx, pool, "first@example.com", "firstpass123")
	if err != nil {
		t.Fatalf("first BootstrapAdmin: %v", err)
	}

	err = BootstrapAdmin(ctx, pool, "second@example.com", "secondpass12")
	if err == nil {
		t.Fatal("expected error for second bootstrap call")
	}
	if !strings.Contains(err.Error(), "users already exist") {
		t.Errorf("error = %q, want 'users already exist'", err)
	}

	// Verify only the first user exists.
	count, err := UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 user, got %d", count)
	}
}

// -------------------------------------------------------------------------
// Cross-function integration scenarios
// -------------------------------------------------------------------------

func TestPhase2_FullWorkflow_BootstrapAuthSession(t *testing.T) {
	pool := setupPhase2Pool(t)
	ctx := context.Background()

	// 1. Bootstrap admin (re-truncate to guard against running sidecar).
	_, _ = pool.Exec(ctx, "TRUNCATE sage.users CASCADE")
	// Password must be ≥8 chars per post-hardening auth policy.
	err := BootstrapAdmin(ctx, pool, "admin@corp.com", "s3cretpass8")
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}

	// 2. Authenticate.
	user, err := Authenticate(ctx, pool, "admin@corp.com", "s3cretpass8")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// 3. Create session.
	sessionID, err := CreateSession(ctx, pool, user.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 4. Validate session.
	sessUser, err := ValidateSession(ctx, pool, sessionID)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if sessUser.Email != "admin@corp.com" {
		t.Errorf("session user email = %q", sessUser.Email)
	}

	// 5. Create second user via OAuth.
	oauthUser, err := FindOrCreateOAuthUser(ctx, pool,
		"dev@corp.com", "google", RoleOperator)
	if err != nil {
		t.Fatalf("FindOrCreateOAuthUser: %v", err)
	}
	if oauthUser.Role != RoleOperator {
		t.Errorf("oauth user role = %q, want operator", oauthUser.Role)
	}

	// 6. List users.
	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	// 7. Update OAuth user role.
	err = UpdateUserRole(ctx, pool, oauthUser.ID, RoleAdmin)
	if err != nil {
		t.Fatalf("UpdateUserRole: %v", err)
	}

	// 8. Verify count.
	count, err := UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 users, got %d", count)
	}

	// 9. Delete OAuth user.
	err = DeleteUser(ctx, pool, oauthUser.ID)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// 10. Logout admin.
	err = DeleteSession(ctx, pool, sessionID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// 11. Clean expired sessions (no-op but exercises the path).
	err = CleanExpiredSessions(ctx, pool)
	if err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}

	// 12. Final count should be 1.
	count, err = UserCount(ctx, pool)
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 user, got %d", count)
	}
}

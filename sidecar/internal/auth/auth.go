package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt hash of the password.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword(
		[]byte(password), BcryptCost,
	)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(bytes), nil
}

// CheckPassword returns true if password matches the hash.
func CheckPassword(hash, password string) bool {
	err := bcrypt.CompareHashAndPassword(
		[]byte(hash), []byte(password),
	)
	return err == nil
}

// CreateUser inserts a new user and returns the user ID.
func CreateUser(
	ctx context.Context, pool *pgxpool.Pool,
	email, password, role string,
) (int, error) {
	if len(password) < 8 {
		return 0, fmt.Errorf(
			"password must be at least 8 characters")
	}
	if !IsValidRole(role) {
		return 0, fmt.Errorf("invalid role: %q", role)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	var id int
	err = pool.QueryRow(ctx,
		"INSERT INTO sage.users (email, password, role) "+
			"VALUES ($1, $2, $3) RETURNING id",
		email, hash, role,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("creating user: %w", err)
	}
	return id, nil
}

// Authenticate verifies credentials and returns the user.
func Authenticate(
	ctx context.Context, pool *pgxpool.Pool,
	email, password string,
) (*User, error) {
	var u User
	var hash *string
	err := pool.QueryRow(ctx,
		"SELECT id, email, password, role, created_at, last_login "+
			"FROM sage.users WHERE email = $1",
		email,
	).Scan(&u.ID, &u.Email, &hash, &u.Role,
		&u.CreatedAt, &u.LastLogin)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("invalid credentials")
		}
		return nil, fmt.Errorf("querying user: %w", err)
	}
	if hash == nil || !CheckPassword(*hash, password) {
		return nil, fmt.Errorf("invalid credentials")
	}
	_, err = pool.Exec(ctx,
		"UPDATE sage.users SET last_login = now() WHERE id = $1",
		u.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("updating last_login: %w", err)
	}
	return &u, nil
}

// CreateSession inserts a session row, returns the UUID.
func CreateSession(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) (string, error) {
	var sessionID string
	err := pool.QueryRow(ctx,
		"INSERT INTO sage.sessions (user_id, expires_at) "+
			"VALUES ($1, $2) RETURNING id",
		userID, time.Now().Add(SessionDuration),
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}
	return sessionID, nil
}

// ValidateSession checks that a session exists and is not expired.
func ValidateSession(
	ctx context.Context, pool *pgxpool.Pool, sessionID string,
) (*User, error) {
	var u User
	err := pool.QueryRow(ctx,
		"SELECT u.id, u.email, u.role, u.created_at, u.last_login "+
			"FROM sage.sessions s "+
			"JOIN sage.users u ON u.id = s.user_id "+
			"WHERE s.id = $1 AND s.expires_at > now()",
		sessionID,
	).Scan(&u.ID, &u.Email, &u.Role,
		&u.CreatedAt, &u.LastLogin)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("session invalid or expired")
		}
		return nil, fmt.Errorf("validating session: %w", err)
	}
	return &u, nil
}

// DeleteSession removes a session (logout).
func DeleteSession(
	ctx context.Context, pool *pgxpool.Pool, sessionID string,
) error {
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.sessions WHERE id = $1", sessionID,
	)
	if err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// CleanExpiredSessions deletes all expired sessions.
func CleanExpiredSessions(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.sessions WHERE expires_at <= now()",
	)
	if err != nil {
		return fmt.Errorf("cleaning expired sessions: %w", err)
	}
	return nil
}

// ListUsers returns all users without password hashes.
func ListUsers(
	ctx context.Context, pool *pgxpool.Pool,
) ([]User, error) {
	rows, err := pool.Query(ctx,
		"SELECT id, email, role, created_at, last_login "+
			"FROM sage.users ORDER BY id",
	)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(
			&u.ID, &u.Email, &u.Role,
			&u.CreatedAt, &u.LastLogin,
		); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating users: %w", err)
	}
	return users, nil
}

// DeleteUser removes a user and all their sessions.
func DeleteUser(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) error {
	tag, err := pool.Exec(ctx,
		"DELETE FROM sage.users WHERE id = $1", userID,
	)
	if err != nil {
		return fmt.Errorf("deleting user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

// UpdateUserRole changes a user's role.
func UpdateUserRole(
	ctx context.Context, pool *pgxpool.Pool,
	userID int, role string,
) error {
	if !IsValidRole(role) {
		return fmt.Errorf("invalid role: %q", role)
	}
	tag, err := pool.Exec(ctx,
		"UPDATE sage.users SET role = $1 WHERE id = $2",
		role, userID,
	)
	if err != nil {
		return fmt.Errorf("updating user role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

// FindOrCreateOAuthUser looks up a user by email. If not found,
// creates one with the given provider and default role.
func FindOrCreateOAuthUser(
	ctx context.Context, pool *pgxpool.Pool,
	email, provider, defaultRole string,
) (*User, error) {
	if defaultRole == "" {
		defaultRole = RoleViewer
	}
	if !IsValidRole(defaultRole) {
		return nil, fmt.Errorf("invalid default role: %q", defaultRole)
	}

	var u User
	err := pool.QueryRow(ctx,
		"SELECT id, email, role, created_at, last_login "+
			"FROM sage.users WHERE email = $1",
		email,
	).Scan(&u.ID, &u.Email, &u.Role,
		&u.CreatedAt, &u.LastLogin)
	if err == nil {
		_, _ = pool.Exec(ctx,
			"UPDATE sage.users SET last_login = now() WHERE id = $1",
			u.ID,
		)
		return &u, nil
	}
	if err != pgx.ErrNoRows {
		return nil, fmt.Errorf("querying oauth user: %w", err)
	}

	err = pool.QueryRow(ctx,
		"INSERT INTO sage.users (email, role, oauth_provider) "+
			"VALUES ($1, $2, $3) RETURNING id, created_at",
		email, defaultRole, provider,
	).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("creating oauth user: %w", err)
	}
	u.Email = email
	u.Role = defaultRole
	return &u, nil
}

// GetUserByID returns a user by ID (no password).
func GetUserByID(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) (*User, error) {
	var u User
	err := pool.QueryRow(ctx,
		"SELECT id, email, role, created_at, last_login "+
			"FROM sage.users WHERE id = $1",
		userID,
	).Scan(&u.ID, &u.Email, &u.Role,
		&u.CreatedAt, &u.LastLogin)
	if err != nil {
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return &u, nil
}

// CountAdmins returns the number of users with admin role.
func CountAdmins(
	ctx context.Context, pool *pgxpool.Pool,
) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.users "+
			"WHERE role = $1",
		RoleAdmin,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting admins: %w", err)
	}
	return count, nil
}

// UserCount returns the total number of users.
func UserCount(
	ctx context.Context, pool *pgxpool.Pool,
) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.users",
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return count, nil
}

// BootstrapAdmin creates the first admin if no users exist.
func BootstrapAdmin(
	ctx context.Context, pool *pgxpool.Pool,
	email, password string,
) error {
	count, err := UserCount(ctx, pool)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("admin bootstrap rejected: users already exist")
	}
	_, err = CreateUser(ctx, pool, email, password, RoleAdmin)
	return err
}

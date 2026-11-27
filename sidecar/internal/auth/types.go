package auth

import "time"

// User represents an authenticated user (password never exposed).
type User struct {
	ID        int
	Email     string
	Role      string // "admin", "operator", "viewer"
	CreatedAt time.Time
	LastLogin *time.Time
	// HasPassword is false for an SSO-only account (no password hash).
	HasPassword bool
	// MustChangePassword holds the session to the password change form
	// (an admin reset or a generated bootstrap password, apiauth-03).
	MustChangePassword bool
}

// Session represents an active login session.
type Session struct {
	ID        string
	UserID    int
	ExpiresAt time.Time
}

const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"

	SessionDuration = 24 * time.Hour
	BcryptCost      = 12
)

// ValidRoles is the set of allowed role values.
var ValidRoles = map[string]bool{
	RoleAdmin:    true,
	RoleOperator: true,
	RoleViewer:   true,
}

// IsValidRole returns true if role is one of admin, operator, viewer.
func IsValidRole(role string) bool {
	return ValidRoles[role]
}

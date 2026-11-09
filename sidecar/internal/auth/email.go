package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An email is one identity whatever its case (apiauth-05): every boundary
// that takes one (account creation, password login, SSO identities, link
// grants, break-glass, the login limiter and audit) normalizes it with
// NormalizeEmail, and every lookup compares lower(email). Installs whose
// sage.users held case-variant duplicates before the migration keep them
// until an operator resolves them; such an email matches more than one
// account and fails closed.

var (
	// ErrInvalidEmail is an email that is not a single bare address.
	ErrInvalidEmail = errors.New("invalid email address")
	// ErrEmailAmbiguous means the email matches several accounts that
	// differ only in case; sign-in with it is refused until an operator
	// removes or renames the duplicates.
	ErrEmailAmbiguous = errors.New(
		"email matches more than one account (case-variant duplicates)")
	// ErrEmailInUse means an account with this email, in any case, exists.
	ErrEmailInUse = errors.New("an account with this email already exists (in any case)")
)

// newEmailOnlySQL guards an INSERT ... SELECT whose first parameter is the
// email: no row is inserted when any account has it in any case. It holds
// even where users_email_lower is missing (duplicates found at migration).
const newEmailOnlySQL = "WHERE NOT EXISTS (SELECT 1 FROM sage.users " +
	"WHERE lower(email) = lower($1::text))"

// NormalizeEmail returns the email trimmed of surrounding whitespace and in
// lower case: the form sage.users stores and lookups compare.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail returns the normalized email when it is a single bare
// address (no display name, no list), or ErrInvalidEmail.
func ValidateEmail(email string) (string, error) {
	normalized := NormalizeEmail(email)
	addr, err := mail.ParseAddress(normalized)
	if err != nil || addr.Name != "" || addr.Address != normalized {
		return "", fmt.Errorf("%w: want one address like name@example.com",
			ErrInvalidEmail)
	}
	return normalized, nil
}

// usersWithEmail returns the ids (at most two) of the accounts whose email
// matches case-insensitively.
func usersWithEmail(ctx context.Context, pool *pgxpool.Pool, email string) ([]int, error) {
	rows, err := pool.Query(ctx,
		"/* pg_sage auth_user_lookup v2 */ SELECT id FROM sage.users "+
			"WHERE lower(email) = lower($1) ORDER BY id LIMIT 2",
		NormalizeEmail(email))
	if err != nil {
		return nil, fmt.Errorf("looking up user by email: %w", err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("looking up user by email: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("looking up user by email: %w", err)
	}
	return ids, nil
}

// logAmbiguousEmail tells the operator which accounts collide; the email
// itself is not logged.
func logAmbiguousEmail(path string, ids []int) {
	slog.Error("sign-in refused: the email matches several accounts that differ "+
		"only in case; delete or rename the duplicates in sage.users",
		"path", path, "user_ids", ids)
}

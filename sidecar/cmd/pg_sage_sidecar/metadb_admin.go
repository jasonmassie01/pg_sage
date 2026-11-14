package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
)

const (
	adminEmail   = "admin@pg-sage.local"
	adminPassLen = 16
)

// bootstrapAdminUser creates the first admin user when the
// sage.users table is empty. Only generated credentials are printed to stderr.
func bootstrapAdminUser(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	appAuth, err := auth.LoadDatabricksAppConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if appAuth != nil {
		return auth.BootstrapDatabricksAppAdmin(ctx, pool, *appAuth)
	}
	count, err := auth.UserCount(ctx, pool)
	if err != nil {
		return fmt.Errorf("checking user count: %w", err)
	}
	if count > 0 {
		return nil
	}

	credentials, err := loadBootstrapAdminCredentials()
	if err != nil {
		return fmt.Errorf("loading admin bootstrap credentials: %w", err)
	}

	if err := createBootstrapAdmin(ctx, pool, credentials); err != nil {
		return fmt.Errorf("creating admin: %w", err)
	}

	if credentials.supplied {
		fmt.Fprintf(os.Stderr,
			"First admin created: %s (configured bootstrap secret; password not logged)\n",
			credentials.email)
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"First admin created: %s\nInitial password: %s\n"+
			"You must change this password at first sign-in.\n",
		credentials.email, credentials.password,
	)
	return nil
}

// generateRandomPassword returns a hex-encoded random string of
// the given length.
func generateRandomPassword(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(buf)[:length], nil
}

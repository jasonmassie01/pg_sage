package main

import (
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"

	"github.com/pg-sage/sidecar/internal/appmode"
)

type bootstrapAdminCredentials struct {
	email    string
	password string
	supplied bool
}

// loadBootstrapAdminCredentials is called only for an empty users table.
// A configured secret seeds the first account; it never resets an account.
func loadBootstrapAdminCredentials() (bootstrapAdminCredentials, error) {
	email := os.Getenv("SAGE_BOOTSTRAP_ADMIN_EMAIL")
	password := os.Getenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD")
	file := os.Getenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE")
	if email == "" && password == "" && file == "" {
		// A generated password is printed to stderr, which Apps keeps in its
		// logs; the App auth loader refuses this first, so this is a backstop.
		if appmode.InApps(os.Getenv) {
			return bootstrapAdminCredentials{}, fmt.Errorf(
				"refusing to generate and print an admin password inside Databricks Apps; "+
					"start pg_sage with %s=%s", appmode.AuthModeEnv, appmode.AuthMode)
		}
		password, err := generateRandomPassword(adminPassLen)
		return bootstrapAdminCredentials{email: adminEmail, password: password}, err
	}
	invalid := bootstrapAdminCredentials{}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || address.Name != "" || strings.ContainsAny(email, "\r\n") {
		return invalid, fmt.Errorf("SAGE_BOOTSTRAP_ADMIN_EMAIL must be a plain email address")
	}
	if (password == "") == (file == "") {
		return invalid, fmt.Errorf("set exactly one of SAGE_BOOTSTRAP_ADMIN_PASSWORD or SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE")
	}
	if file != "" {
		password, err = readBootstrapAdminPassword(file)
		if err != nil {
			return invalid, err
		}
	}
	// bcrypt accepts at most 72 bytes. Requiring 12 for an explicit bootstrap
	// secret also catches accidental short/empty secret resource values.
	if len(password) < 12 || len(password) > 72 {
		return invalid, fmt.Errorf("bootstrap admin password must contain 12 to 72 bytes")
	}
	return bootstrapAdminCredentials{email: email, password: password, supplied: true}, nil
}

func readBootstrapAdminPassword(path string) (string, error) {
	return readOwnerOnlyFile(path, "SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE")
}

// maxOwnerOnlyFileBytes bounds a secret file read at startup.
const maxOwnerOnlyFileBytes = 4096

// readOwnerOnlyFile reads a secret file that must be a regular file only
// its owner can access (mode 600 or 400) and at most 4 KiB. label names
// the file in errors; the content never appears in them.
func readOwnerOnlyFile(path, label string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot open %s", label)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must be a regular file accessible only to its owner "+
			"(mode 600 or 400)", label)
	}
	value, err := io.ReadAll(io.LimitReader(f, maxOwnerOnlyFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %s", label)
	}
	if len(value) > maxOwnerOnlyFileBytes {
		return "", fmt.Errorf("%s exceeds maximum size", label)
	}
	return strings.TrimRight(string(value), "\r\n"), nil
}

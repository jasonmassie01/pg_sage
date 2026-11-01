package main

import (
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"
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
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot open SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE must be a regular file accessible only to its owner (mode 600 or 400)")
	}
	value, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", fmt.Errorf("cannot read SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE")
	}
	if len(value) > 4096 {
		return "", fmt.Errorf("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE exceeds maximum size")
	}
	return strings.TrimRight(string(value), "\r\n"), nil
}

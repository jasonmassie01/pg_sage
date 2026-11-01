package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DatabricksAppMode = "databricks_app"

// DatabricksAppConfig is an explicit trust in Databricks Apps' authenticated
// ingress, never a generic reverse-proxy authentication option. The platform
// authenticates and asserts X-Forwarded-Email before forwarding requests.
// Deployments must keep the application port behind that ingress.
type DatabricksAppConfig struct {
	Workspace  string
	AdminEmail string
}

// LoadDatabricksAppConfigFromEnv returns nil for ordinary session auth.
// Selecting proxy identity outside the declared Apps runtime fails closed.
func LoadDatabricksAppConfigFromEnv(getenv func(string) string) (*DatabricksAppConfig, error) {
	mode := getenv("SAGE_AUTH_MODE")
	if mode == "" || mode == "session" {
		return nil, nil
	}
	if mode != DatabricksAppMode {
		return nil, fmt.Errorf("SAGE_AUTH_MODE must be session or databricks_app")
	}
	port, err := strconv.Atoi(getenv("DATABRICKS_APP_PORT"))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("databricks_app auth requires the Databricks Apps runtime port")
	}
	// Apps may inject a bare workspace hostname. Match unified auth's HTTPS
	// default while keeping the identity issuer a validated origin.
	workspaceHost := getenv("DATABRICKS_HOST")
	if workspaceHost != "" && !strings.Contains(workspaceHost, "://") {
		workspaceHost = "https://" + workspaceHost
	}
	u, err := url.Parse(workspaceHost)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("databricks_app auth requires an HTTPS Databricks workspace origin")
	}
	email, err := DatabricksAppEmail(getenv("SAGE_DATABRICKS_ADMIN_EMAIL"))
	if err != nil {
		return nil, fmt.Errorf("databricks_app auth requires a plain SAGE_DATABRICKS_ADMIN_EMAIL")
	}
	return &DatabricksAppConfig{Workspace: strings.TrimRight(u.String(), "/"), AdminEmail: email}, nil
}

// DatabricksAppEmail rejects lists, display names and whitespace. Identity is
// keyed by the platform-asserted email in this workspace, not a body field or
// arbitrary forwarded user/role header. Email is not claimed to be immutable.
func DatabricksAppEmail(raw string) (string, error) {
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Name != "" || a.Address != raw || len(raw) > 254 ||
		strings.ContainsAny(raw, ",;\r\n\t ") {
		return "", fmt.Errorf("invalid Databricks email identity")
	}
	return strings.ToLower(a.Address), nil
}

func (c DatabricksAppConfig) Identity(email string) Identity {
	return Identity{Issuer: c.Workspace + "/databricks-apps", Subject: email,
		Email: email, EmailVerified: true}
}

// ResolveDatabricksAppUser reads the durable role on every request. It does
// not grant administrator privileges from request headers or cache stale roles.
func ResolveDatabricksAppUser(ctx context.Context, pool *pgxpool.Pool, c DatabricksAppConfig, email string) (*User, error) {
	if pool == nil {
		return nil, fmt.Errorf("databricks account store is unavailable")
	}
	identity := c.Identity(email)
	u, err := userByOAuthIdentity(ctx, pool, identity)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return FindOrCreateOAuthUser(ctx, pool, identity, "databricks_apps", RoleViewer)
}

// BootstrapDatabricksAppAdmin explicitly binds the configured owner, preserving
// its durable ID and approval/audit references. This is not email-based linking
// of arbitrary users: only the operator-configured owner may migrate a local
// account. An account bound to a different external identity is refused.
func BootstrapDatabricksAppAdmin(ctx context.Context, pool *pgxpool.Pool, c DatabricksAppConfig) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Databricks admin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE sage.users IN EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock Databricks admin: %w", err)
	}
	identity := c.Identity(c.AdminEmail)
	var matches int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM sage.users WHERE lower(email) = $1", c.AdminEmail).Scan(&matches); err != nil {
		return fmt.Errorf("count configured Databricks admin: %w", err)
	}
	if matches > 1 {
		return fmt.Errorf("configured Databricks admin has ambiguous local accounts")
	}
	var userID int
	var issuer, subject *string
	err = tx.QueryRow(ctx, `SELECT id, oauth_issuer, oauth_subject FROM sage.users
		WHERE lower(email) = $1 FOR UPDATE`, c.AdminEmail).Scan(&userID, &issuer, &subject)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `INSERT INTO sage.users
			(email, password, role, oauth_provider, oauth_issuer, oauth_subject)
			VALUES ($1, NULL, 'admin', 'databricks_apps', $2, $1) RETURNING id`,
			c.AdminEmail, identity.Issuer).Scan(&userID)
	case err != nil:
		return fmt.Errorf("read Databricks admin: %w", err)
	case (issuer != nil || subject != nil) && (issuer == nil || *issuer != identity.Issuer || subject == nil || *subject != identity.Subject):
		return fmt.Errorf("configured Databricks admin is already linked to another identity")
	default:
		_, err = tx.Exec(ctx, `UPDATE sage.users SET password = NULL, role = 'admin',
			oauth_provider = 'databricks_apps', oauth_issuer = $2, oauth_subject = $3
			WHERE id = $1`, userID, identity.Issuer, identity.Subject)
	}
	if err != nil {
		return fmt.Errorf("bind Databricks admin: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sage.sessions WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("revoke previous admin sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Databricks admin bootstrap: %w", err)
	}
	return nil
}

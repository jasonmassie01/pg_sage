package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// setupOIDCPool starts from the legacy users table used by the other
// auth tests and applies the production bootstrap on top of it, which
// proves the issuer/subject migration is additive.
func setupOIDCPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := setupPhase2Pool(t)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func testIdentity(subject, email string) Identity {
	return Identity{
		Issuer: "https://idp.example.com", Subject: subject,
		Email: email, EmailVerified: true,
	}
}

func TestFindOrCreateOAuthUser_CreatesWithIssuerSubject(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-1", "new@example.com"), "oidc", RoleViewer)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var issuer, subject string
	if err := pool.QueryRow(ctx, `SELECT oauth_issuer, oauth_subject
		FROM sage.users WHERE id = $1`, u.ID).Scan(&issuer, &subject); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if issuer != "https://idp.example.com" || subject != "sub-1" {
		t.Fatalf("stored identity = (%q, %q)", issuer, subject)
	}
	if u.Role != RoleViewer || u.Email != "new@example.com" {
		t.Fatalf("user = %+v", u)
	}
}

func TestFindOrCreateOAuthUser_MatchesOnSubjectNotEmail(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	first, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-2", "old@example.com"), "oidc", RoleViewer)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	again, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-2", "renamed@example.com"), "oidc", RoleAdmin)
	if err != nil {
		t.Fatalf("re-login after email change: %v", err)
	}
	if again.ID != first.ID || again.Role != RoleViewer {
		t.Fatalf("re-login user = %+v, want id %d role viewer",
			again, first.ID)
	}
}

func TestFindOrCreateOAuthUser_RefusesToAutoLinkPasswordAccount(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin, err := CreateUser(ctx, pool, "admin@example.com",
		"correct-horse-battery", RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	_, err = FindOrCreateOAuthUser(ctx, pool,
		testIdentity("attacker-sub", "admin@example.com"), "oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("err = %v, want %v", err, ErrOAuthLinkRequired)
	}
	var issuer *string
	if err := pool.QueryRow(ctx, `SELECT oauth_issuer FROM sage.users
		WHERE id = $1`, admin).Scan(&issuer); err != nil {
		t.Fatalf("read admin: %v", err)
	}
	if issuer != nil {
		t.Fatalf("password account was linked to issuer %q", *issuer)
	}
}

func TestFindOrCreateOAuthUser_SubjectChangeCannotInherit(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if _, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-a", "shared@example.com"), "oidc",
		RoleViewer); err != nil {
		t.Fatalf("create: %v", err)
	}
	other := testIdentity("sub-b", "shared@example.com")
	if _, err := FindOrCreateOAuthUser(ctx, pool, other, "oidc",
		RoleViewer); !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("subject change err = %v, want %v",
			err, ErrOAuthLinkRequired)
	}
	otherIssuer := testIdentity("sub-a", "shared@example.com")
	otherIssuer.Issuer = "https://evil.example.com"
	if _, err := FindOrCreateOAuthUser(ctx, pool, otherIssuer, "oidc",
		RoleViewer); !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("issuer change err = %v, want %v",
			err, ErrOAuthLinkRequired)
	}
}

func TestFindOrCreateOAuthUser_RejectsUnverifiedIdentity(t *testing.T) {
	pool := setupOIDCPool(t)
	id := testIdentity("sub-u", "u@example.com")
	id.EmailVerified = false
	_, err := FindOrCreateOAuthUser(context.Background(), pool, id,
		"oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthEmailUnverified) {
		t.Fatalf("err = %v, want %v", err, ErrOAuthEmailUnverified)
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.users").Scan(&n)
	if n != 0 {
		t.Fatalf("users = %d, want 0", n)
	}
}

// Users created by OAuth before issuer/subject existed (no password,
// same provider, no issuer) are linked on their first verified login.
func TestFindOrCreateOAuthUser_LinksLegacyOAuthUser(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	var legacyID int
	err := pool.QueryRow(ctx, `INSERT INTO sage.users
		(email, role, oauth_provider) VALUES ('legacy@example.com',
		'operator', 'oidc') RETURNING id`).Scan(&legacyID)
	if err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-l", "legacy@example.com"), "oidc", RoleViewer)
	if err != nil {
		t.Fatalf("legacy login: %v", err)
	}
	if u.ID != legacyID || u.Role != RoleOperator {
		t.Fatalf("user = %+v, want legacy id %d operator", u, legacyID)
	}
}

func TestFindOrCreateOAuthUser_ConcurrentFirstLogin(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := testIdentity("sub-c", "race@example.com")
	var wg sync.WaitGroup
	ids := make([]int, 6)
	errs := make([]error, 6)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := FindOrCreateOAuthUser(ctx, pool, id, "oidc", RoleViewer)
			errs[i] = err
			if u != nil {
				ids[i] = u.ID
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("login %d got user %d, want %d", i, ids[i], ids[0])
		}
	}
	var n int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM sage.users").Scan(&n)
	if n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

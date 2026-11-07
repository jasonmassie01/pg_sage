package auth

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-01: deactivating a user is the offboarding step. It ends every
// session, revokes the user's operator MCP tokens, and refuses password,
// SSO and link-grant sign-in until an admin re-enables the account. The
// last enabled admin cannot be deactivated.

func statusIdentity(subject, email string) Identity {
	return Identity{Issuer: "https://idp.status.test", Subject: subject,
		Email: email, EmailVerified: true}
}

func insertOperatorToken(t *testing.T, pool *pgxpool.Pool, owner int, hash string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.mcp_tokens
		(name, kind, scopes, databases, token_hash, prefix, owner_user_id, created_by,
		 expires_at)
		VALUES ('op', 'operator', ARRAY['read','approve'], ARRAY['*'], $1, 'sage_op',
		 $2, 'test', now() + interval '1 day')`, hash, owner)
	if err != nil {
		t.Fatalf("inserting operator token: %v", err)
	}
}

func tokenRevoked(t *testing.T, pool *pgxpool.Pool, hash string) bool {
	t.Helper()
	var revoked bool
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM sage.mcp_tokens WHERE token_hash = $1`,
		hash).Scan(&revoked); err != nil {
		t.Fatalf("reading token: %v", err)
	}
	return revoked
}

func TestSetUserDisabled_StateTransitions(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	linkTestUser(t, pool, "status-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "status-op@example.com", RoleOperator)
	grant, _, err := IssueLinkGrant(ctx, pool, target, 0)
	if err != nil {
		t.Fatalf("IssueLinkGrant: %v", err)
	}
	id := statusIdentity("status-sub", "status-op@example.com")
	if _, err := pool.Exec(ctx, `UPDATE sage.users SET oauth_issuer = $1,
		oauth_subject = $2 WHERE id = $3`, id.Issuer, id.Subject, target); err != nil {
		t.Fatalf("binding identity: %v", err)
	}
	cookie, err := CreateSession(ctx, pool, target)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tokenHash := "ab01ab01ab01ab01ab01ab01ab01ab01ab01ab01"
	insertOperatorToken(t, pool, target, tokenHash)

	res, err := SetUserDisabled(ctx, pool, target, true)
	if err != nil {
		t.Fatalf("SetUserDisabled(true): %v", err)
	}
	if !res.Changed || res.SessionsEnded != 1 || res.TokensRevoked != 1 {
		t.Fatalf("disable result = %+v, want changed, 1 session, 1 token", res)
	}
	requireDisabledRefusals(t, pool, cookie, grant, id)
	if !tokenRevoked(t, pool, tokenHash) {
		t.Fatal("operator MCP token still live after deactivation")
	}

	requireReEnabled(t, pool, target, cookie, tokenHash, id)
}

// requireReEnabled re-enables target and checks sign-in works again while
// the old session and the revoked token stay dead.
func requireReEnabled(
	t *testing.T, pool *pgxpool.Pool, target int, cookie, tokenHash string, id Identity,
) {
	t.Helper()
	ctx := context.Background()
	res, err := SetUserDisabled(ctx, pool, target, false)
	if err != nil || !res.Changed || res.SessionsEnded != 0 || res.TokensRevoked != 0 {
		t.Fatalf("SetUserDisabled(false) = (%+v, %v), want changed only", res, err)
	}
	if u, err := Authenticate(ctx, pool, "status-op@example.com",
		"local-password-1"); err != nil || u.ID != target {
		t.Fatalf("password login after re-enable = (%+v, %v)", u, err)
	}
	if u, err := FindOrCreateOAuthUser(ctx, pool, id, "oidc", RoleViewer); err != nil ||
		u.ID != target {
		t.Fatalf("SSO login after re-enable = (%+v, %v)", u, err)
	}
	if _, err := ValidateSession(ctx, pool, cookie); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("old session after re-enable: err = %v, want ErrSessionInvalid", err)
	}
	if !tokenRevoked(t, pool, tokenHash) {
		t.Fatal("re-enabling the user revived a revoked MCP token")
	}
}

func requireDisabledRefusals(
	t *testing.T, pool *pgxpool.Pool, cookie, grant string, id Identity,
) {
	t.Helper()
	ctx := context.Background()
	if _, err := ValidateSession(ctx, pool, cookie); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("old session: err = %v, want ErrSessionInvalid", err)
	}
	fresh, err := CreateSession(ctx, pool, id2(t, pool, id))
	if err != nil {
		t.Fatalf("CreateSession for a disabled user: %v", err)
	}
	if _, err := ValidateSession(ctx, pool, fresh); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("new session of a disabled user: err = %v, want ErrSessionInvalid", err)
	}
	if _, err := Authenticate(ctx, pool, id.Email,
		"local-password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("password login: err = %v, want ErrInvalidCredentials", err)
	}
	if _, err := FindOrCreateOAuthUser(ctx, pool, id, "oidc",
		RoleViewer); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("SSO login: err = %v, want ErrUserDisabled", err)
	}
	if _, err := RedeemLinkGrant(ctx, pool, grant); !errors.Is(err, ErrLinkGrantInvalid) {
		t.Fatalf("link-grant redeem: err = %v, want ErrLinkGrantInvalid", err)
	}
	if _, err := EndUserSessions(ctx, pool, id2(t, pool, id), ""); err != nil {
		t.Fatalf("cleaning the probe session: %v", err)
	}
}

// id2 returns the id of the user bound to identity id.
func id2(t *testing.T, pool *pgxpool.Pool, id Identity) int {
	t.Helper()
	var uid int
	if err := pool.QueryRow(context.Background(), `SELECT id FROM sage.users
		WHERE oauth_issuer = $1 AND oauth_subject = $2`, id.Issuer, id.Subject).
		Scan(&uid); err != nil {
		t.Fatalf("reading bound user: %v", err)
	}
	return uid
}

func TestSetUserDisabled_IsIdempotent(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	linkTestUser(t, pool, "idem-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "idem-viewer@example.com", RoleViewer)
	if res, err := SetUserDisabled(ctx, pool, target, true); err != nil || !res.Changed {
		t.Fatalf("first disable = (%+v, %v)", res, err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, `SELECT disabled_at FROM sage.users WHERE id = $1`,
		target).Scan(&first); err != nil {
		t.Fatalf("reading disabled_at: %v", err)
	}
	res, err := SetUserDisabled(ctx, pool, target, true)
	if err != nil || res.Changed {
		t.Fatalf("second disable = (%+v, %v), want unchanged", res, err)
	}
	var again time.Time
	if err := pool.QueryRow(ctx, `SELECT disabled_at FROM sage.users WHERE id = $1`,
		target).Scan(&again); err != nil {
		t.Fatalf("reading disabled_at: %v", err)
	}
	if !again.Equal(first) {
		t.Fatalf("disabled_at moved from %v to %v on a repeat", first, again)
	}
	if res, err := SetUserDisabled(ctx, pool, target, false); err != nil || !res.Changed {
		t.Fatalf("enable = (%+v, %v)", res, err)
	}
	if res, err := SetUserDisabled(ctx, pool, target, false); err != nil || res.Changed {
		t.Fatalf("repeat enable = (%+v, %v), want unchanged", res, err)
	}
}

func TestSetUserDisabled_LastEnabledAdmin(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	a := linkTestUser(t, pool, "last-a@example.com", RoleAdmin)
	b := linkTestUser(t, pool, "last-b@example.com", RoleAdmin)
	if _, err := SetUserDisabled(ctx, pool, b, true); err != nil {
		t.Fatalf("disabling the second admin: %v", err)
	}
	// b is disabled, so a is the only enabled admin.
	if _, err := SetUserDisabled(ctx, pool, a, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the last enabled admin: err = %v, want ErrLastAdmin", err)
	}
	if err := UpdateUserRolePreservingAdmin(ctx, pool, a,
		RoleViewer); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last enabled admin: err = %v, want ErrLastAdmin", err)
	}
	if err := DeleteUserPreservingAdmin(ctx, pool, a); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("deleting the last enabled admin: err = %v, want ErrLastAdmin", err)
	}
	// A disabled admin holds no admin power, so it may be demoted or deleted.
	if err := UpdateUserRolePreservingAdmin(ctx, pool, b, RoleOperator); err != nil {
		t.Fatalf("demoting a disabled admin: %v", err)
	}
	if err := DeleteUserPreservingAdmin(ctx, pool, b); err != nil {
		t.Fatalf("deleting a disabled admin: %v", err)
	}
	var enabled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.users
		WHERE role = 'admin' AND disabled_at IS NULL`).Scan(&enabled); err != nil {
		t.Fatalf("counting admins: %v", err)
	}
	if enabled != 1 {
		t.Fatalf("enabled admins = %d, want 1", enabled)
	}
}

func TestSetUserDisabled_ReEnableNeedsNoAdminCheck(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	linkTestUser(t, pool, "re-a@example.com", RoleAdmin)
	b := linkTestUser(t, pool, "re-b@example.com", RoleAdmin)
	if _, err := SetUserDisabled(ctx, pool, b, true); err != nil {
		t.Fatalf("disable b: %v", err)
	}
	if res, err := SetUserDisabled(ctx, pool, b, false); err != nil || !res.Changed {
		t.Fatalf("re-enable b = (%+v, %v)", res, err)
	}
	if u, err := Authenticate(ctx, pool, "re-b@example.com", "local-password-1"); err != nil ||
		u.ID != b || u.Role != RoleAdmin {
		t.Fatalf("re-enabled admin login = (%+v, %v)", u, err)
	}
}

func TestSetUserDisabled_UnknownUser(t *testing.T) {
	pool := setupOIDCPool(t)
	if _, err := SetUserDisabled(context.Background(), pool, 987654,
		true); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user: err = %v, want ErrUserNotFound", err)
	}
}

func TestSetUserDisabled_InvalidInputNeverQueries(t *testing.T) {
	for _, id := range []int{0, -1} {
		res, err := SetUserDisabled(context.Background(), nil, id, true)
		if !errors.Is(err, ErrUserNotFound) || res.Changed {
			t.Fatalf("id %d: (%+v, %v), want ErrUserNotFound", id, res, err)
		}
	}
	if _, err := SetUserDisabled(context.Background(), nil, 1, true); err == nil {
		t.Fatal("nil pool: want an error")
	}
}

func TestSetUserDisabled_PropagatesStoreError(t *testing.T) {
	pool := closedPool(t)
	_, err := SetUserDisabled(context.Background(), pool, 1, true)
	if err == nil || errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrLastAdmin) {
		t.Fatalf("closed pool: err = %v, want a wrapped store error", err)
	}
}

// Two admins deactivating each other at once must leave one enabled admin:
// the users-table lock serializes the admin count.
func TestSetUserDisabled_ConcurrentMutualDisable(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	a := linkTestUser(t, pool, "race-a@example.com", RoleAdmin)
	b := linkTestUser(t, pool, "race-b@example.com", RoleAdmin)
	wide, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(wide.Close)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, target := range []int{a, b} {
		wg.Add(1)
		go func(i, target int) {
			defer wg.Done()
			<-start
			_, errs[i] = SetUserDisabled(ctx, wide, target, true)
		}(i, target)
	}
	close(start)
	wg.Wait()
	refused := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrLastAdmin):
			refused++
		case err != nil:
			t.Fatalf("concurrent disable: %v", err)
		}
	}
	var enabled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.users
		WHERE role = 'admin' AND disabled_at IS NULL`).Scan(&enabled); err != nil {
		t.Fatalf("counting admins: %v", err)
	}
	if refused != 1 || enabled != 1 {
		t.Fatalf("refused = %d, enabled admins = %d; want 1 and 1", refused, enabled)
	}
}

func TestListUsers_ReportsDisabled(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	linkTestUser(t, pool, "list-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "list-off@example.com", RoleViewer)
	if _, err := SetUserDisabled(ctx, pool, target, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	users, err := ListUsers(ctx, pool)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range users {
		seen[u.Email] = u.DisabledAt != nil
	}
	if !seen["list-off@example.com"] || seen["list-admin@example.com"] {
		t.Fatalf("disabled flags = %v, want only list-off disabled", seen)
	}
}

// Break-glass is the lockout recovery path: signing in with it restores the
// break-glass account even if an admin had deactivated it.
func TestBreakGlassLogin_ReEnablesItsAccount(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	linkTestUser(t, pool, "bg-admin@example.com", RoleAdmin)
	cfg := breakGlassConfig()
	u, err := BreakGlassLogin(ctx, pool, cfg, fixtureBreakGlassPassword)
	if err != nil {
		t.Fatalf("first break-glass login: %v", err)
	}
	if _, err := SetUserDisabled(ctx, pool, u.ID, true); err != nil {
		t.Fatalf("disabling break-glass account: %v", err)
	}
	again, err := BreakGlassLogin(ctx, pool, cfg, fixtureBreakGlassPassword)
	if err != nil || again.ID != u.ID {
		t.Fatalf("break-glass login after deactivation = (%+v, %v)", again, err)
	}
	cookie, err := CreateSession(ctx, pool, again.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got, err := ValidateSession(ctx, pool, cookie); err != nil || got.ID != u.ID {
		t.Fatalf("break-glass session = (%+v, %v), want valid", got, err)
	}
}

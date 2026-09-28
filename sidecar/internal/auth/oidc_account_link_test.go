package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// D7: an existing account is bound to an SSO identity only by an explicit
// act (a signed-in link or an admin-issued one-time grant), never by email.

func linkTestUser(t *testing.T, pool *pgxpool.Pool, email, role string) int {
	t.Helper()
	id, err := CreateUser(context.Background(), pool, email, "local-password-1", role)
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return id
}

func storedIdentity(t *testing.T, pool *pgxpool.Pool, userID int) (*string, *string) {
	t.Helper()
	var issuer, subject *string
	if err := pool.QueryRow(context.Background(), `SELECT oauth_issuer, oauth_subject
		FROM sage.users WHERE id = $1`, userID).Scan(&issuer, &subject); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	return issuer, subject
}

func TestLinkOAuthIdentity_BindsToPasswordUserPreservingIDAndRole(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "Ops@Example.com", RoleOperator)
	if err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-link-1", "ops@example.com"), "oidc"); err != nil {
		t.Fatalf("link: %v", err)
	}
	issuer, subject := storedIdentity(t, pool, id)
	if issuer == nil || *issuer != "https://idp.example.com" ||
		subject == nil || *subject != "sub-link-1" {
		t.Fatalf("identity not bound: %v/%v", issuer, subject)
	}
	var role string
	var hash *string
	if err := pool.QueryRow(ctx, `SELECT role, password FROM sage.users WHERE id=$1`,
		id).Scan(&role, &hash); err != nil {
		t.Fatal(err)
	}
	if role != RoleOperator || hash == nil || *hash == "" {
		t.Fatalf("link changed role %q or dropped password", role)
	}
	if _, err := Authenticate(ctx, pool, "Ops@Example.com", "local-password-1"); err != nil {
		t.Fatalf("password login after link: %v", err)
	}
}

func TestLinkOAuthIdentity_RefusesIdentityBoundToAnotherUser(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	first := linkTestUser(t, pool, "first@example.com", RoleViewer)
	second := linkTestUser(t, pool, "second@example.com", RoleAdmin)
	if err := LinkOAuthIdentity(ctx, pool, first,
		testIdentity("sub-shared", "first@example.com"), "oidc"); err != nil {
		t.Fatalf("first link: %v", err)
	}
	err := LinkOAuthIdentity(ctx, pool, second,
		testIdentity("sub-shared", "second@example.com"), "oidc")
	if !errors.Is(err, ErrOAuthLinkConflict) {
		t.Fatalf("second link err = %v, want ErrOAuthLinkConflict", err)
	}
	if issuer, _ := storedIdentity(t, pool, second); issuer != nil {
		t.Fatalf("second user bound to %q", *issuer)
	}
	_, subject := storedIdentity(t, pool, first)
	if subject == nil || *subject != "sub-shared" {
		t.Fatalf("first user's identity changed: %v", subject)
	}
}

func TestLinkOAuthIdentity_RefusesUserAlreadyLinked(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "linked@example.com", RoleViewer)
	if err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-a", "linked@example.com"), "oidc"); err != nil {
		t.Fatalf("link: %v", err)
	}
	err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-b", "linked@example.com"), "oidc")
	if !errors.Is(err, ErrOAuthLinkConflict) {
		t.Fatalf("relink err = %v, want ErrOAuthLinkConflict", err)
	}
	if _, subject := storedIdentity(t, pool, id); subject == nil || *subject != "sub-a" {
		t.Fatalf("relink replaced identity: %v", subject)
	}
}

func TestLinkOAuthIdentity_RefusesEmailMismatch(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "owner@example.com", RoleViewer)
	err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-other", "someone-else@example.com"), "oidc")
	if !errors.Is(err, ErrOAuthLinkConflict) {
		t.Fatalf("mismatched email err = %v, want ErrOAuthLinkConflict", err)
	}
	if issuer, _ := storedIdentity(t, pool, id); issuer != nil {
		t.Fatalf("mismatched email bound %q", *issuer)
	}
}

func TestLinkOAuthIdentity_RefusesUnverifiedOrIncompleteIdentity(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "unverified@example.com", RoleViewer)
	unverified := testIdentity("sub-u", "unverified@example.com")
	unverified.EmailVerified = false
	if err := LinkOAuthIdentity(ctx, pool, id, unverified, "oidc"); !errors.Is(err,
		ErrOAuthEmailUnverified) {
		t.Fatalf("unverified err = %v, want ErrOAuthEmailUnverified", err)
	}
	noIssuer := testIdentity("sub-u", "unverified@example.com")
	noIssuer.Issuer = ""
	if err := LinkOAuthIdentity(ctx, pool, id, noIssuer, "oidc"); err == nil {
		t.Fatal("identity without issuer was linked")
	}
	if issuer, _ := storedIdentity(t, pool, id); issuer != nil {
		t.Fatalf("rejected identity bound %q", *issuer)
	}
}

func TestLinkOAuthIdentity_UnknownUser(t *testing.T) {
	pool := setupOIDCPool(t)
	err := LinkOAuthIdentity(context.Background(), pool, 999999,
		testIdentity("sub-x", "ghost@example.com"), "oidc")
	if !errors.Is(err, ErrOAuthLinkConflict) && !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user err = %v", err)
	}
}

func TestLinkOAuthIdentity_ConcurrentLinksOfOneIdentity(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	a := linkTestUser(t, pool, "race@example.com", RoleViewer)
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- LinkOAuthIdentity(ctx, pool, a,
				testIdentity("sub-race", "race@example.com"), "oidc")
		}()
	}
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrOAuthLinkConflict) {
			t.Errorf("racer err = %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("successful links = %d, want 1", wins)
	}
}

func TestFindOrCreateOAuthUser_AfterLinkReturnsSameUser(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "after@example.com", RoleAdmin)
	if err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-after", "after@example.com"), "oidc"); err != nil {
		t.Fatalf("link: %v", err)
	}
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-after", "after@example.com"), "oidc", RoleViewer)
	if err != nil {
		t.Fatalf("login after link: %v", err)
	}
	if u.ID != id || u.Role != RoleAdmin {
		t.Fatalf("login resolved to %+v, want id %d as admin", u, id)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("login after link created a new row: %d users", n)
	}
}

func TestFindOrCreateOAuthUser_WrongIssuerDoesNotResolveLinkedUser(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "issuer@example.com", RoleAdmin)
	if err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-iss", "issuer@example.com"), "oidc"); err != nil {
		t.Fatalf("link: %v", err)
	}
	other := testIdentity("sub-iss", "issuer@example.com")
	other.Issuer = "https://other-idp.example.net"
	u, err := FindOrCreateOAuthUser(ctx, pool, other, "oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("other issuer resolved to %+v err=%v, want ErrOAuthLinkRequired", u, err)
	}
}

func TestPlainLoginNeverLinksPasswordAccount(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "plain@example.com", RoleAdmin)
	_, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-plain", "plain@example.com"), "oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("plain login err = %v, want ErrOAuthLinkRequired", err)
	}
	if issuer, _ := storedIdentity(t, pool, id); issuer != nil {
		t.Fatalf("plain login linked the account to %q", *issuer)
	}
}

func TestUnlinkOAuthIdentity_ClearsIdentityAndSessions(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "unlink@example.com", RoleOperator)
	if err := LinkOAuthIdentity(ctx, pool, id,
		testIdentity("sub-unlink", "unlink@example.com"), "oidc"); err != nil {
		t.Fatalf("link: %v", err)
	}
	session, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := UnlinkOAuthIdentity(ctx, pool, id); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if issuer, subject := storedIdentity(t, pool, id); issuer != nil || subject != nil {
		t.Fatalf("identity remains after unlink: %v/%v", issuer, subject)
	}
	if _, err := ValidateSession(ctx, pool, session); err == nil {
		t.Fatal("session survived unlink")
	}
	if err := UnlinkOAuthIdentity(ctx, pool, id); !errors.Is(err, ErrOAuthNotLinked) {
		t.Fatalf("second unlink err = %v, want ErrOAuthNotLinked", err)
	}
	if err := UnlinkOAuthIdentity(ctx, pool, 999999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user unlink err = %v, want ErrUserNotFound", err)
	}
}

func TestCreateSSOOnlyUser_HasNoPasswordAndIsNeverAutoLinked(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id, err := CreateSSOOnlyUser(ctx, pool, "sso-only@example.com", RoleOperator)
	if err != nil {
		t.Fatalf("CreateSSOOnlyUser: %v", err)
	}
	var hash *string
	var role string
	if err := pool.QueryRow(ctx, `SELECT password, role FROM sage.users WHERE id=$1`,
		id).Scan(&hash, &role); err != nil {
		t.Fatal(err)
	}
	if hash != nil || role != RoleOperator {
		t.Fatalf("sso-only user has password=%v role=%q", hash != nil, role)
	}
	if _, err := Authenticate(ctx, pool, "sso-only@example.com", ""); err == nil {
		t.Fatal("password login succeeded for an SSO-only user")
	}
	_, err = FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-sso-only", "sso-only@example.com"), "oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("email match auto-linked an SSO-only user: %v", err)
	}
	if _, err := CreateSSOOnlyUser(ctx, pool, "sso-only@example.com",
		RoleViewer); err == nil {
		t.Fatal("duplicate SSO-only email accepted")
	}
	if _, err := CreateSSOOnlyUser(ctx, pool, "bad-role@example.com", "root"); err == nil {
		t.Fatal("invalid role accepted")
	}
}

func TestLinkGrant_StoresOnlyHashAndLinksOnce(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "grant-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "grant-target@example.com", RoleOperator)
	token, expires, err := IssueLinkGrant(ctx, pool, target, admin)
	if err != nil {
		t.Fatalf("IssueLinkGrant: %v", err)
	}
	if len(token) < 32 || time.Until(expires) > 16*time.Minute ||
		time.Until(expires) < 14*time.Minute {
		t.Fatalf("grant token len=%d expires in %v", len(token), time.Until(expires))
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM sage.user_oidc_link_grants
		WHERE user_id=$1`, target).Scan(&stored); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	sum := sha256.Sum256([]byte(token))
	if stored == token || stored != hex.EncodeToString(sum[:]) {
		t.Fatal("grant is not stored as its SHA-256 hash")
	}
	got, err := RedeemLinkGrant(ctx, pool, token)
	if err != nil || got != target {
		t.Fatalf("redeem = %d, %v; want %d", got, err, target)
	}
	if _, err := RedeemLinkGrant(ctx, pool, token); !errors.Is(err, ErrLinkGrantInvalid) {
		t.Fatalf("replayed grant err = %v, want ErrLinkGrantInvalid", err)
	}
}

func TestLinkGrant_ExpiredUnknownAndReplacedRejected(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "exp-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "exp-target@example.com", RoleViewer)
	old, _, err := IssueLinkGrant(ctx, pool, target, admin)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, err := IssueLinkGrant(ctx, pool, target, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RedeemLinkGrant(ctx, pool, old); !errors.Is(err, ErrLinkGrantInvalid) {
		t.Fatalf("superseded grant err = %v, want ErrLinkGrantInvalid", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.user_oidc_link_grants
		SET expires_at = now() - interval '1 second' WHERE user_id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := RedeemLinkGrant(ctx, pool, fresh); !errors.Is(err, ErrLinkGrantInvalid) {
		t.Fatalf("expired grant err = %v, want ErrLinkGrantInvalid", err)
	}
	for _, token := range []string{"", "not-a-real-grant"} {
		if _, err := RedeemLinkGrant(ctx, pool, token); !errors.Is(err,
			ErrLinkGrantInvalid) {
			t.Fatalf("grant %q err = %v, want ErrLinkGrantInvalid", token, err)
		}
	}
}

func TestLinkGrant_RefusesLinkedOrUnknownUser(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "ref-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "ref-target@example.com", RoleViewer)
	if err := LinkOAuthIdentity(ctx, pool, target,
		testIdentity("sub-ref", "ref-target@example.com"), "oidc"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IssueLinkGrant(ctx, pool, target, admin); !errors.Is(err,
		ErrOAuthLinkConflict) {
		t.Fatalf("grant for linked user err = %v, want ErrOAuthLinkConflict", err)
	}
	if _, _, err := IssueLinkGrant(ctx, pool, 999999, admin); !errors.Is(err,
		ErrUserNotFound) {
		t.Fatalf("grant for unknown user err = %v, want ErrUserNotFound", err)
	}
}

func TestLinkGrant_ConcurrentRedeemExactlyOnce(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "cc-admin@example.com", RoleAdmin)
	target := linkTestUser(t, pool, "cc-target@example.com", RoleViewer)
	token, _, err := IssueLinkGrant(ctx, pool, target, admin)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := RedeemLinkGrant(ctx, pool, token)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrLinkGrantInvalid) {
			t.Errorf("racer err = %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("grant redeemed %d times, want 1", wins)
	}
}

func TestRecordAuthAudit_PersistsEvent(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.auth_audit`); err != nil {
		t.Fatal(err)
	}
	err := RecordAuthAudit(ctx, pool, AuthAuditEvent{Event: AuditOIDCUnlinked,
		ActorUserID: 7, TargetUserID: 9, Detail: map[string]any{"issuer": "i"}})
	if err != nil {
		t.Fatalf("RecordAuthAudit: %v", err)
	}
	var event string
	var actor, target int
	if err := pool.QueryRow(ctx, `SELECT event, actor_user_id, target_user_id
		FROM sage.auth_audit`).Scan(&event, &actor, &target); err != nil {
		t.Fatal(err)
	}
	if event != "oidc_unlinked" || actor != 7 || target != 9 {
		t.Fatalf("audit row = %s/%d/%d", event, actor, target)
	}
}

package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// apiauth-05: sage.users.email is one identity whatever its case. Every
// lookup is case-insensitive, and an email that matches more than one
// account (case-variant duplicates from before the migration) fails closed.

func countUsersByEmail(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.users WHERE lower(email) = lower($1)`,
		email).Scan(&n); err != nil {
		t.Fatalf("count users %s: %v", email, err)
	}
	return n
}

func storedEmail(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var email string
	if err := pool.QueryRow(context.Background(),
		`SELECT email FROM sage.users WHERE id = $1`, id).Scan(&email); err != nil {
		t.Fatalf("read email of %d: %v", id, err)
	}
	return email
}

// withCaseDuplicates drops the case-insensitive index (as on an install
// whose migration found duplicates) and inserts two password accounts whose
// emails differ only in case. Cleanup deletes them and re-bootstraps, which
// restores the index for the tests that follow.
func withCaseDuplicates(t *testing.T, pool *pgxpool.Pool, a, b string) (int, int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS sage.users_email_lower`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	hash, err := HashPassword("local-password-1")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 2)
	for i, email := range []string{a, b} {
		if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
			VALUES ($1, $2, 'admin') RETURNING id`, email, hash).Scan(&ids[i]); err != nil {
			t.Fatalf("insert %s: %v", email, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.users WHERE id = ANY($1)`, ids)
		if err := schema.Bootstrap(bg, pool); err != nil {
			t.Errorf("re-bootstrap after duplicates: %v", err)
		}
	})
	return ids[0], ids[1]
}

func TestEmailCase_SSOCaseVariantOfPasswordUserNeedsLink(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "alice@corp.com", RoleViewer)
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-alice-upper", "Alice@Corp.com"), "oidc", RoleAdmin)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("SSO as Alice@Corp.com = (%+v, %v), want ErrOAuthLinkRequired", u, err)
	}
	if n := countUsersByEmail(t, pool, "alice@corp.com"); n != 1 {
		t.Fatalf("users with alice@corp.com = %d, want 1 (no new row)", n)
	}
	if issuer, _ := storedIdentity(t, pool, id); issuer != nil {
		t.Fatalf("case-variant SSO login linked the password account to %q", *issuer)
	}
}

func TestEmailCase_PasswordLoginAnyCase(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "alice@corp.com", RoleOperator)
	for _, email := range []string{"ALICE@corp.com", "  Alice@Corp.Com ", "alice@corp.com"} {
		u, err := Authenticate(ctx, pool, email, "local-password-1")
		if err != nil || u == nil || u.ID != id || u.Role != RoleOperator {
			t.Fatalf("Authenticate(%q) = (%+v, %v), want user %d", email, u, err, id)
		}
	}
	if _, err := Authenticate(ctx, pool, "ALICE@corp.com", "wrong-password"); !errors.Is(
		err, ErrInvalidCredentials) {
		t.Fatalf("wrong password err = %v, want ErrInvalidCredentials", err)
	}
	got, err := UserIDByEmail(ctx, pool, " ALICE@Corp.com")
	if err != nil || got != id {
		t.Fatalf("UserIDByEmail(ALICE) = (%d, %v), want %d", got, err, id)
	}
	if _, err := UserIDByEmail(ctx, pool, "nobody@corp.com"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown email err = %v, want ErrUserNotFound", err)
	}
}

func TestEmailCase_CreateStoresNormalizedAndRefusesVariant(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id, err := CreateUser(ctx, pool, "  Carol@Corp.COM ", "local-password-1", RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if got := storedEmail(t, pool, id); got != "carol@corp.com" {
		t.Fatalf("stored email = %q, want carol@corp.com", got)
	}
	if _, err := CreateUser(ctx, pool, "CAROL@corp.com", "local-password-2",
		RoleAdmin); err == nil {
		t.Fatal("a case variant of an existing email created a second account")
	}
	sso, err := CreateSSOOnlyUser(ctx, pool, " Dave@Corp.com", RoleViewer)
	if err != nil {
		t.Fatalf("CreateSSOOnlyUser: %v", err)
	}
	if got := storedEmail(t, pool, sso); got != "dave@corp.com" {
		t.Fatalf("stored SSO-only email = %q, want dave@corp.com", got)
	}
	if _, err := CreateSSOOnlyUser(ctx, pool, "DAVE@corp.com", RoleViewer); err == nil {
		t.Fatal("a case variant of an SSO-only email created a second account")
	}
	if n := countUsersByEmail(t, pool, "carol@corp.com") + countUsersByEmail(t, pool,
		"dave@corp.com"); n != 2 {
		t.Fatalf("accounts = %d, want 2", n)
	}
}

func TestEmailCase_CreateRejectsEmptyEmail(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	for _, email := range []string{"", "   "} {
		if id, err := CreateUser(ctx, pool, email, "local-password-1",
			RoleViewer); err == nil {
			t.Fatalf("CreateUser(%q) created user %d", email, id)
		}
		if id, err := CreateSSOOnlyUser(ctx, pool, email, RoleViewer); err == nil {
			t.Fatalf("CreateSSOOnlyUser(%q) created user %d", email, id)
		}
	}
}

// While users_email_lower is missing (duplicates found elsewhere), the
// migration has not lower-cased stored emails, so only an explicit
// case-insensitive check stops a new account beside a mixed-case one.
func TestEmailCase_CreateRefusesVariantWithoutIndex(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	withCaseDuplicates(t, pool, "dup@corp.com", "DUP@corp.com")
	var erin int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
		VALUES ('Erin@Corp.com', NULL, 'viewer') RETURNING id`).Scan(&erin); err != nil {
		t.Fatalf("insert mixed-case account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, erin)
	})
	if id, err := CreateUser(ctx, pool, "erin@corp.com", "local-password-1",
		RoleAdmin); !errors.Is(err, ErrEmailInUse) {
		t.Fatalf("CreateUser(erin@corp.com) = (%d, %v), want ErrEmailInUse", id, err)
	}
	if id, err := CreateSSOOnlyUser(ctx, pool, "ERIN@corp.com",
		RoleViewer); !errors.Is(err, ErrEmailInUse) {
		t.Fatalf("CreateSSOOnlyUser(ERIN@corp.com) = (%d, %v), want ErrEmailInUse", id, err)
	}
	if n := countUsersByEmail(t, pool, "erin@corp.com"); n != 1 {
		t.Fatalf("accounts for erin@corp.com = %d, want 1", n)
	}
	if got := storedEmail(t, pool, erin); got != "Erin@Corp.com" {
		t.Fatalf("existing account email changed to %q", got)
	}
}

func TestEmailCase_SSOFirstLoginStoresLowerCase(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-new-person", " New.Person@Corp.COM"), "oidc", RoleViewer)
	if err != nil {
		t.Fatalf("first SSO login: %v", err)
	}
	if u.Email != "new.person@corp.com" || storedEmail(t, pool, u.ID) != "new.person@corp.com" {
		t.Fatalf("user email = %q stored %q, want new.person@corp.com",
			u.Email, storedEmail(t, pool, u.ID))
	}
	again, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-new-person", "NEW.PERSON@corp.com"), "oidc", RoleViewer)
	if err != nil || again.ID != u.ID {
		t.Fatalf("second SSO login = (%+v, %v), want user %d", again, err, u.ID)
	}
}

func TestEmailCase_LegacyOAuthUserLinksAnyCase(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role,
		oauth_provider) VALUES ('legacy@corp.com', NULL, 'operator', 'oidc')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-legacy", "Legacy@Corp.com"), "oidc", RoleViewer)
	if err != nil || u.ID != id || u.Role != RoleOperator {
		t.Fatalf("legacy SSO login = (%+v, %v), want user %d as operator", u, err, id)
	}
	if _, subject := storedIdentity(t, pool, id); subject == nil || *subject != "sub-legacy" {
		t.Fatalf("legacy row not bound: subject %v", subject)
	}
}

func TestEmailCase_AmbiguousEmailFailsClosed(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	a, b := withCaseDuplicates(t, pool, "Twin@Corp.com", "twin@corp.com")
	for _, email := range []string{"twin@corp.com", "Twin@Corp.com"} {
		u, err := Authenticate(ctx, pool, email, "local-password-1")
		if !errors.Is(err, ErrInvalidCredentials) || u != nil {
			t.Fatalf("ambiguous login %q = (%+v, %v), want ErrInvalidCredentials", email, u, err)
		}
	}
	got, err := UserIDByEmail(ctx, pool, "twin@corp.com")
	if !errors.Is(err, ErrEmailAmbiguous) || got != 0 {
		t.Fatalf("UserIDByEmail = (%d, %v), want (0, ErrEmailAmbiguous)", got, err)
	}
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-twin", "TWIN@corp.com"), "oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("ambiguous SSO = (%+v, %v), want ErrOAuthLinkRequired", u, err)
	}
	if n := countUsersByEmail(t, pool, "twin@corp.com"); n != 2 {
		t.Fatalf("users = %d, want the 2 duplicates and no new row", n)
	}
	for _, id := range []int{a, b} {
		if issuer, _ := storedIdentity(t, pool, id); issuer != nil {
			t.Fatalf("ambiguous SSO linked user %d", id)
		}
	}
}

func TestEmailCase_AmbiguousLegacyOAuthRowsAreNotLinked(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS sage.users_email_lower`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.users WHERE lower(email) = 'pair@corp.com'`)
		if err := schema.Bootstrap(bg, pool); err != nil {
			t.Errorf("re-bootstrap: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.users (email, password, role,
		oauth_provider) VALUES ('Pair@corp.com', NULL, 'admin', 'oidc'),
		('pair@corp.com', NULL, 'viewer', 'oidc')`); err != nil {
		t.Fatal(err)
	}
	u, err := FindOrCreateOAuthUser(ctx, pool,
		testIdentity("sub-pair", "pair@corp.com"), "oidc", RoleViewer)
	if !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("ambiguous legacy SSO = (%+v, %v), want ErrOAuthLinkRequired", u, err)
	}
	var bound int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.users
		WHERE oauth_subject = 'sub-pair'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 0 {
		t.Fatalf("%d legacy rows bound to an ambiguous email, want 0", bound)
	}
}

func TestEmailCase_BreakGlassRefusesCaseVariantHolder(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	var holder int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
		VALUES ('Break-Glass@PG-Sage.local', 'x', 'viewer') RETURNING id`).
		Scan(&holder); err != nil {
		t.Fatal(err)
	}
	u, err := BreakGlassLogin(ctx, pool, breakGlassConfig(), fixtureBreakGlassPassword)
	if !errors.Is(err, ErrBreakGlassConflict) || u != nil {
		t.Fatalf("break-glass = (%+v, %v), want ErrBreakGlassConflict", u, err)
	}
	if n := countUsersByEmail(t, pool, BreakGlassEmail); n != 1 {
		t.Fatalf("accounts for the break-glass email = %d, want only the holder", n)
	}
	if role, err := UserRole(ctx, pool, holder); err != nil || role != RoleViewer {
		t.Fatalf("holder role = %q (%v), want viewer unchanged", role, err)
	}
}

// Two first logins at once, from two IdP identities whose emails differ only
// in case: one account, and the other identity must be linked explicitly.
func TestEmailCase_ConcurrentCaseVariantFirstLogins(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	ids := []Identity{testIdentity("sub-race-a", "Race@corp.com"),
		testIdentity("sub-race-b", "race@CORP.com")}
	type result struct {
		u   *User
		err error
	}
	results := make(chan result, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id Identity) {
			defer wg.Done()
			u, err := FindOrCreateOAuthUser(ctx, pool, id, "oidc", RoleViewer)
			results <- result{u, err}
		}(id)
	}
	wg.Wait()
	close(results)
	wins, refused := 0, 0
	for r := range results {
		switch {
		case r.err == nil && r.u != nil && r.u.Email == "race@corp.com":
			wins++
		case errors.Is(r.err, ErrOAuthLinkRequired):
			refused++
		default:
			t.Errorf("racer = (%+v, %v)", r.u, r.err)
		}
	}
	if wins != 1 || refused != 1 {
		t.Fatalf("wins=%d refused=%d, want 1 and 1", wins, refused)
	}
	if n := countUsersByEmail(t, pool, "race@corp.com"); n != 1 {
		t.Fatalf("accounts = %d, want 1", n)
	}
}

// The same identity racing itself with two email spellings resolves to one
// account for both requests.
func TestEmailCase_ConcurrentSameIdentityFirstLogins(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	emails := []string{"Same@corp.com", "same@Corp.com", "SAME@CORP.COM"}
	got := make(chan int, len(emails))
	var wg sync.WaitGroup
	for _, email := range emails {
		wg.Add(1)
		go func(email string) {
			defer wg.Done()
			u, err := FindOrCreateOAuthUser(ctx, pool,
				testIdentity("sub-same", email), "oidc", RoleViewer)
			if err != nil {
				t.Errorf("racer %s: %v", email, err)
				got <- 0
				return
			}
			got <- u.ID
		}(email)
	}
	wg.Wait()
	close(got)
	first := 0
	for id := range got {
		if first == 0 {
			first = id
		}
		if id == 0 || id != first {
			t.Fatalf("racers resolved to different users: %d vs %d", id, first)
		}
	}
	if n := countUsersByEmail(t, pool, "same@corp.com"); n != 1 {
		t.Fatalf("accounts = %d, want 1", n)
	}
}

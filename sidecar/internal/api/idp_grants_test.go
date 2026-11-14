package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// apiauth-08d over the real router, a real database and an in-process
// signing IdP: oauth.database_grant_mapping turns the groups claim into
// per-database grants at each SSO login, in one transaction, audited, and
// never touching an admin's manual grants.

func withGrantMapping(cfg *config.Config) {
	cfg.OAuth.DatabaseGrantMapping = []config.OAuthDatabaseGrant{
		{Group: "team-billing", Database: "billing", Role: auth.RoleOperator},
		{Group: "team-search", Database: "search", Role: auth.RoleViewer},
		{Group: "team-boom", Database: "aaa-idp-ok", Role: auth.RoleViewer},
		{Group: "team-boom", Database: "idp-boom", Role: auth.RoleViewer},
	}
}

// idpLogin signs in through the stub IdP with groups (nil = no claim) and
// returns the callback status.
func (f *linkFixture) idpLogin(t *testing.T, subject string, groups []string) int {
	t.Helper()
	f.resetBrowser()
	claims := map[string]any{"sub": subject, "email": subject + "@fixture.invalid",
		"email_verified": true}
	if groups != nil {
		claims["groups"] = groups
	}
	f.idp.SetClaims(claims)
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	return f.callback(t, state).StatusCode
}

// grantRows returns database → "role/source" for userID.
func (f *linkFixture) grantRows(t *testing.T, userID int) map[string]string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT database_name, role, source
		FROM sage.user_database_grants WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatalf("reading grants: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var db, role, source string
		if err := rows.Scan(&db, &role, &source); err != nil {
			t.Fatal(err)
		}
		out[db] = role + "/" + source
	}
	return out
}

func TestIdPGrant_LoginMapsGroupToDatabaseGrant(t *testing.T) {
	f := newAuthzFixture(t, func(c *config.Config) { withMapping(c); withGrantMapping(c) },
		nil)
	since := f.maxAuditID(t)
	if code := f.idpLogin(t, "sub-idp-happy", []string{"pg-viewers", "team-billing"}); code !=
		http.StatusFound {
		t.Fatalf("login = %d, want 302", code)
	}
	id := f.userID(t, "sub-idp-happy@fixture.invalid")
	if rows := f.grantRows(t, id); len(rows) != 1 || rows["billing"] != "operator/idp" {
		t.Fatalf("grants = %v, want billing operator/idp", rows)
	}
	row := f.oneAudit(t, since, auth.AuditDatabaseGrantSet)
	if row.Target != id || row.Actor != id || row.Detail["database"] != "billing" ||
		row.Detail["role"] != "operator" || row.Detail["source"] != "idp" {
		t.Fatalf("grant audit = %+v", row)
	}
	// The role mapping is unchanged: pg-viewers still makes a viewer.
	if role, _ := auth.UserRole(context.Background(), f.pool, id); role != auth.RoleViewer {
		t.Fatalf("role = %q, want viewer from role_mapping", role)
	}
}

func TestIdPGrant_EmptyGroupsRemoveIdPGrants(t *testing.T) {
	f := newAuthzFixture(t, withGrantMapping, nil)
	if code := f.idpLogin(t, "sub-idp-empty", []string{"team-billing", "team-search"}); code !=
		http.StatusFound {
		t.Fatalf("first login = %d", code)
	}
	id := f.userID(t, "sub-idp-empty@fixture.invalid")
	if rows := f.grantRows(t, id); len(rows) != 2 {
		t.Fatalf("grants after first login = %v, want two", rows)
	}
	since := f.maxAuditID(t)
	if code := f.idpLogin(t, "sub-idp-empty", nil); code != http.StatusFound {
		t.Fatalf("login with no groups claim = %d, want 302", code)
	}
	if rows := f.grantRows(t, id); len(rows) != 0 {
		t.Fatalf("grants after empty groups = %v, want none (global role again)", rows)
	}
	revs := f.auditSince(t, since, auth.AuditDatabaseGrantRevoked)
	if len(revs) != 2 || revs[0].Detail["source"] != "idp" || revs[1].Detail["source"] != "idp" {
		t.Fatalf("revoke audit = %+v, want two idp revokes", revs)
	}
}

func TestIdPGrant_MoveTeamsAcrossLogins(t *testing.T) {
	f := newAuthzFixture(t, withGrantMapping, nil)
	if code := f.idpLogin(t, "sub-idp-move", []string{"team-billing"}); code != http.StatusFound {
		t.Fatalf("first login = %d", code)
	}
	id := f.userID(t, "sub-idp-move@fixture.invalid")
	since := f.maxAuditID(t)
	if code := f.idpLogin(t, "sub-idp-move", []string{"team-search"}); code != http.StatusFound {
		t.Fatalf("second login = %d", code)
	}
	if rows := f.grantRows(t, id); len(rows) != 1 || rows["search"] != "viewer/idp" {
		t.Fatalf("grants after the move = %v, want search viewer/idp only", rows)
	}
	rev := f.oneAudit(t, since, auth.AuditDatabaseGrantRevoked)
	set := f.oneAudit(t, since, auth.AuditDatabaseGrantSet)
	if rev.Detail["database"] != "billing" || set.Detail["database"] != "search" {
		t.Fatalf("audit = revoke %+v, set %+v", rev, set)
	}
}

func TestIdPGrant_ManualGrantWinsAndSurvives(t *testing.T) {
	f := newAuthzFixture(t, withGrantMapping, nil)
	if code := f.idpLogin(t, "sub-idp-manual", []string{"team-search"}); code != http.StatusFound {
		t.Fatalf("first login = %d", code)
	}
	id := f.userID(t, "sub-idp-manual@fixture.invalid")
	admin, _ := f.createUser(t, "admin", auth.RoleAdmin)
	if _, err := auth.NewDatabaseGrantStore(f.pool).SetGrant(context.Background(),
		auth.GrantChange{ActorID: admin, UserID: id, Database: "billing",
			Role: auth.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	since := f.maxAuditID(t)
	if code := f.idpLogin(t, "sub-idp-manual", []string{"team-billing"}); code !=
		http.StatusFound {
		t.Fatalf("second login = %d", code)
	}
	if rows := f.grantRows(t, id); len(rows) != 1 || rows["billing"] != "viewer/manual" {
		t.Fatalf("grants = %v, want the manual billing viewer grant only", rows)
	}
	shadow := f.oneAudit(t, since, auth.AuditIdPGrantShadowed)
	if shadow.Detail["database"] != "billing" || shadow.Detail["idp_role"] != "operator" ||
		shadow.Detail["manual_role"] != "viewer" {
		t.Fatalf("shadow audit = %+v", shadow)
	}
	if code := f.idpLogin(t, "sub-idp-manual", nil); code != http.StatusFound {
		t.Fatalf("third login = %d", code)
	}
	if rows := f.grantRows(t, id); rows["billing"] != "viewer/manual" {
		t.Fatalf("an IdP login with no groups touched a manual grant: %v", rows)
	}
}

func TestIdPGrant_GroupNamesAreCaseSensitive(t *testing.T) {
	f := newAuthzFixture(t, withGrantMapping, nil)
	if code := f.idpLogin(t, "sub-idp-case", []string{"Team-Billing", "TEAM-SEARCH"}); code !=
		http.StatusFound {
		t.Fatalf("login = %d", code)
	}
	if rows := f.grantRows(t, f.userID(t, "sub-idp-case@fixture.invalid")); len(rows) != 0 {
		t.Fatalf("grants = %v, want none: group names match exactly", rows)
	}
}

func TestIdPGrant_NoMappingKeepsTodayBehaviour(t *testing.T) {
	f := newAuthzFixture(t, nil, nil)
	if code := f.idpLogin(t, "sub-idp-off", []string{"team-billing"}); code != http.StatusFound {
		t.Fatalf("first login = %d", code)
	}
	id := f.userID(t, "sub-idp-off@fixture.invalid")
	// A leftover IdP row from a mapping since removed is not touched either.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, source) VALUES ($1, 'search', 'viewer', 'idp')`,
		id); err != nil {
		t.Fatal(err)
	}
	if code := f.idpLogin(t, "sub-idp-off", nil); code != http.StatusFound {
		t.Fatalf("second login = %d", code)
	}
	if rows := f.grantRows(t, id); len(rows) != 1 || rows["search"] != "viewer/idp" {
		t.Fatalf("grants = %v, want only the untouched leftover", rows)
	}
}

// installGrantFailure makes inserting a grant on idp-boom fail, as a
// broken or full control database would.
func installGrantFailure(t *testing.T, f *linkFixture) {
	t.Helper()
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION sage.test_idp_grant_boom()
		RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		RAISE EXCEPTION 'injected grant write failure'; END $$;
		DROP TRIGGER IF EXISTS test_idp_grant_boom ON sage.user_database_grants;
		CREATE TRIGGER test_idp_grant_boom BEFORE INSERT ON sage.user_database_grants
		FOR EACH ROW WHEN (NEW.database_name = 'idp-boom')
		EXECUTE FUNCTION sage.test_idp_grant_boom();`)
	if err != nil {
		t.Fatalf("installing the failing trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS
			test_idp_grant_boom ON sage.user_database_grants;
			DROP FUNCTION IF EXISTS sage.test_idp_grant_boom();`)
	})
}

func TestIdPGrant_WriteFailureFailsLoginWith503(t *testing.T) {
	f := newAuthzFixture(t, withGrantMapping, nil)
	if code := f.idpLogin(t, "sub-idp-boom", []string{"team-billing"}); code != http.StatusFound {
		t.Fatalf("first login = %d", code)
	}
	id := f.userID(t, "sub-idp-boom@fixture.invalid")
	installGrantFailure(t, f)
	since := f.maxAuditID(t)
	if code := f.idpLogin(t, "sub-idp-boom", []string{"team-boom"}); code !=
		http.StatusServiceUnavailable {
		t.Fatalf("login with a failing grant write = %d, want 503", code)
	}
	if f.sessionCookie() != "" {
		t.Fatal("a failed grant sync still started a session")
	}
	if rows := f.grantRows(t, id); len(rows) != 1 || rows["billing"] != "operator/idp" {
		t.Fatalf("grants = %v, want the previous set unchanged (no partial write)", rows)
	}
	if n := len(f.auditSince(t, since, auth.AuditDatabaseGrantSet)) +
		len(f.auditSince(t, since, auth.AuditDatabaseGrantRevoked)); n != 0 {
		t.Fatalf("%d grant audit rows from a rolled-back sync", n)
	}
	failed := f.oneAudit(t, since, auth.AuditLoginFailed)
	if failed.Detail["reason"] != "grant_sync_failed" {
		t.Fatalf("login_failed reason = %v, want grant_sync_failed", failed.Detail["reason"])
	}
	if n := len(f.auditSince(t, since, auth.AuditLoginSucceeded)); n != 0 {
		t.Fatalf("%d login_succeeded rows for a refused login", n)
	}
	f.resetBrowser()
	f.idp.SetClaims(map[string]any{"sub": "sub-idp-boom",
		"email": "sub-idp-boom@fixture.invalid", "email_verified": true,
		"groups": []string{"team-boom"}})
	state := f.authorizeState(t, http.MethodGet, "/api/v1/auth/oauth/authorize", "")
	assertErrorPage(t, f.browserCallback(t, state), "/#/login?sso_error=grants_unavailable")
}

func TestIdPGrantSource_EditorShowsSourceAndRefusesIdPRevoke(t *testing.T) {
	f := newAuthzFixture(t, withGrantMapping, nil)
	if code := f.idpLogin(t, "sub-idp-editor", []string{"team-billing"}); code !=
		http.StatusFound {
		t.Fatalf("SSO login = %d", code)
	}
	id := f.userID(t, "sub-idp-editor@fixture.invalid")
	f.resetBrowser()
	f.login(t, auth.RoleAdmin)
	base := fmt.Sprintf("/api/v1/users/%d/database-grants", id)
	status, body := f.request(t, http.MethodGet, base, "")
	var list struct {
		Grants     []auth.DatabaseGrant `json:"grants"`
		IdPManaged bool                 `json:"idp_managed"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
		t.Fatalf("list = %d %s", status, body)
	}
	if !list.IdPManaged || len(list.Grants) != 1 || list.Grants[0].Source != "idp" {
		t.Fatalf("list = %+v, want one idp grant and idp_managed", list)
	}
	status, body = f.request(t, http.MethodDelete, base+"/billing", "")
	if status != http.StatusConflict || !strings.Contains(body, "idp_grant") {
		t.Fatalf("DELETE of an IdP grant = %d %s, want 409 idp_grant", status, body)
	}
	if rows := f.grantRows(t, id); rows["billing"] != "operator/idp" {
		t.Fatalf("a refused DELETE changed the grant: %v", rows)
	}
}

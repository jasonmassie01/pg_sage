package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// apiauth-08d: oauth.database_grant_mapping turns IdP groups into
// per-database grants at each SSO login. The user's source='idp' rows are
// replaced in one transaction with an audit row per change; manual rows
// are never touched and win over the IdP on the same database.

func idpMappingConfig(entries ...config.OAuthDatabaseGrant) *config.OAuthConfig {
	return &config.OAuthConfig{DatabaseGrantMapping: entries}
}

func TestIdPGrantMappingConfigured(t *testing.T) {
	if DatabaseGrantMappingConfigured(nil) {
		t.Error("nil config reports a mapping")
	}
	if DatabaseGrantMappingConfigured(&config.OAuthConfig{}) {
		t.Error("empty mapping reports a mapping")
	}
	if !DatabaseGrantMappingConfigured(idpMappingConfig(config.OAuthDatabaseGrant{
		Group: "g", Database: "d", Role: RoleViewer})) {
		t.Error("a configured mapping is not reported")
	}
}

func TestResolveIdPGrants(t *testing.T) {
	cfg := idpMappingConfig(
		config.OAuthDatabaseGrant{Group: "team-billing", Database: "billing", Role: RoleOperator},
		config.OAuthDatabaseGrant{Group: "team-billing", Database: "orders", Role: RoleViewer},
		config.OAuthDatabaseGrant{Group: "team-orders", Database: "orders", Role: RoleOperator},
		config.OAuthDatabaseGrant{Group: "team-search", Database: "search", Role: RoleViewer},
	)
	cases := []struct {
		name   string
		groups []string
		want   map[string]string
	}{
		{"one group", []string{"team-billing"},
			map[string]string{"billing": RoleOperator, "orders": RoleViewer}},
		{"highest role per database wins", []string{"team-billing", "team-orders"},
			map[string]string{"billing": RoleOperator, "orders": RoleOperator}},
		{"order of groups does not matter", []string{"team-orders", "team-billing"},
			map[string]string{"billing": RoleOperator, "orders": RoleOperator}},
		{"unmapped groups give nothing", []string{"engineering"}, map[string]string{}},
		{"nil groups give nothing", nil, map[string]string{}},
		{"empty groups give nothing", []string{}, map[string]string{}},
		{"group names are case-sensitive", []string{"Team-Billing", "TEAM-SEARCH"},
			map[string]string{}},
		{"no trimming of group names", []string{" team-search"}, map[string]string{}},
	}
	for _, tc := range cases {
		got := ResolveIdPGrants(cfg, tc.groups)
		if got == nil || len(got) != len(tc.want) {
			t.Errorf("%s: ResolveIdPGrants = %v, want %v", tc.name, got, tc.want)
			continue
		}
		for db, role := range tc.want {
			if got[db] != role {
				t.Errorf("%s: %s = %q, want %q", tc.name, db, got[db], role)
			}
		}
	}
	if got := ResolveIdPGrants(nil, []string{"team-billing"}); got == nil || len(got) != 0 {
		t.Errorf("nil config: ResolveIdPGrants = %v, want an empty map", got)
	}
}

func TestIdPGrantSync_InvalidInputRejectedBeforeSQL(t *testing.T) {
	s := NewDatabaseGrantStore(nil) // any SQL would fail as unavailable
	ctx := context.Background()
	cases := []struct {
		grants map[string]string
		want   error
	}{
		{map[string]string{"billing": RoleAdmin}, ErrInvalidGrantRole},
		{map[string]string{"billing": ""}, ErrInvalidGrantRole},
		{map[string]string{"all": RoleViewer}, ErrInvalidGrantDatabase},
		{map[string]string{"": RoleViewer}, ErrInvalidGrantDatabase},
		{map[string]string{"*": RoleOperator}, ErrInvalidGrantDatabase},
	}
	for _, tc := range cases {
		_, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: 1, Grants: tc.grants})
		if !errors.Is(err, tc.want) {
			t.Errorf("SyncIdPGrants(%v) = %v, want %v", tc.grants, err, tc.want)
		}
	}
	var nilStore *DatabaseGrantStore
	if _, err := nilStore.SyncIdPGrants(ctx, IdPGrantSync{UserID: 1}); !errors.Is(err,
		ErrGrantStoreUnavailable) {
		t.Errorf("nil store: %v, want ErrGrantStoreUnavailable", err)
	}
}

type idpRow struct{ role, source string }

func idpGrantRows(t *testing.T, pool *pgxpool.Pool, userID int) map[string]idpRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT database_name, role, source
		FROM sage.user_database_grants WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatalf("reading grants: %v", err)
	}
	defer rows.Close()
	out := map[string]idpRow{}
	for rows.Next() {
		var db string
		var r idpRow
		if err := rows.Scan(&db, &r.role, &r.source); err != nil {
			t.Fatal(err)
		}
		out[db] = r
	}
	return out
}

func idpAuditDetails(t *testing.T, pool *pgxpool.Pool, event string,
	target int) []map[string]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT detail FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = $2 ORDER BY id`, event, target)
	if err != nil {
		t.Fatalf("reading %s audit: %v", event, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var d map[string]any
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestIdPGrantSync_SetsChangesAndRevokesWithAudit(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	u := linkTestUser(t, pool, "idp-sync@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)

	res, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u, SourceIP: "10.1.2.3",
		Grants: map[string]string{"billing": RoleOperator, "orders": RoleViewer}})
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(res.Set) != 2 || res.Set[0] != "billing" || res.Set[1] != "orders" ||
		len(res.Revoked) != 0 || len(res.Shadowed) != 0 {
		t.Fatalf("first sync result = %+v, want set billing, orders", res)
	}
	rows := idpGrantRows(t, pool, u)
	if len(rows) != 2 || rows["billing"] != (idpRow{RoleOperator, GrantSourceIdP}) ||
		rows["orders"] != (idpRow{RoleViewer, GrantSourceIdP}) {
		t.Fatalf("rows after first sync = %+v", rows)
	}
	sets := idpAuditDetails(t, pool, AuditDatabaseGrantSet, u)
	if len(sets) != 2 || sets[0]["database"] != "billing" || sets[0]["role"] != RoleOperator ||
		sets[0]["source"] != GrantSourceIdP || sets[0]["previous_role"] != "" {
		t.Fatalf("set audit = %+v", sets)
	}

	// Same groups again: nothing changes, nothing is audited.
	res, err = s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
		Grants: map[string]string{"billing": RoleOperator, "orders": RoleViewer}})
	if err != nil || len(res.Set)+len(res.Revoked)+len(res.Shadowed) != 0 {
		t.Fatalf("unchanged sync = (%+v, %v), want no change", res, err)
	}
	if n := len(idpAuditDetails(t, pool, AuditDatabaseGrantSet, u)); n != 2 {
		t.Fatalf("an unchanged sync wrote audit rows: %d set rows", n)
	}

	assertRoleChangeAndRevoke(t, pool, s, u)
}

// assertRoleChangeAndRevoke changes orders to operator and drops billing,
// then checks the rows and the audit of both changes.
func assertRoleChangeAndRevoke(t *testing.T, pool *pgxpool.Pool, s *DatabaseGrantStore,
	u int) {
	t.Helper()
	// Role change on orders, billing dropped.
	res, err := s.SyncIdPGrants(context.Background(), IdPGrantSync{UserID: u,
		Grants: map[string]string{"orders": RoleOperator}})
	if err != nil || len(res.Set) != 1 || res.Set[0] != "orders" || len(res.Revoked) != 1 ||
		res.Revoked[0] != "billing" {
		t.Fatalf("third sync = (%+v, %v)", res, err)
	}
	rows := idpGrantRows(t, pool, u)
	if len(rows) != 1 || rows["orders"] != (idpRow{RoleOperator, GrantSourceIdP}) {
		t.Fatalf("rows after third sync = %+v", rows)
	}
	sets := idpAuditDetails(t, pool, AuditDatabaseGrantSet, u)
	if len(sets) != 3 || sets[2]["previous_role"] != RoleViewer || sets[2]["role"] != RoleOperator {
		t.Fatalf("role change audit = %+v", sets)
	}
	revs := idpAuditDetails(t, pool, AuditDatabaseGrantRevoked, u)
	if len(revs) != 1 || revs[0]["database"] != "billing" ||
		revs[0]["previous_role"] != RoleOperator || revs[0]["source"] != GrantSourceIdP {
		t.Fatalf("revoke audit = %+v", revs)
	}
}

func TestIdPGrantSync_EmptyGroupsRemoveAllIdPGrants(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	u := linkTestUser(t, pool, "idp-empty@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	if _, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
		Grants: map[string]string{"billing": RoleOperator, "search": RoleViewer}}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	res, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u, Grants: nil})
	if err != nil || len(res.Revoked) != 2 || len(res.Set) != 0 {
		t.Fatalf("empty sync = (%+v, %v), want two revokes", res, err)
	}
	grants, err := s.UserDatabaseGrants(ctx, u)
	if err != nil || len(grants) != 0 {
		t.Fatalf("grants after empty sync = (%v, %v), want none (global role again)",
			grants, err)
	}
	if role := EffectiveDatabaseRole(RoleOperator, grants, "payroll"); role != RoleOperator {
		t.Fatalf("effective role with no grants = %q, want the global operator", role)
	}
	if n := len(idpAuditDetails(t, pool, AuditDatabaseGrantRevoked, u)); n != 2 {
		t.Fatalf("%d revoke audit rows, want 2", n)
	}
}

func TestIdPGrantSync_ManualRowsWinAndAreNeverTouched(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "idp-manual-admin@example.com", RoleAdmin)
	u := linkTestUser(t, pool, "idp-manual@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	for db, role := range map[string]string{"billing": RoleViewer, "payroll": RoleOperator} {
		if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: u, Database: db,
			Role: role}); err != nil {
			t.Fatalf("manual grant %s: %v", db, err)
		}
	}
	res, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
		Grants: map[string]string{"billing": RoleOperator, "search": RoleViewer}})
	if err != nil || len(res.Shadowed) != 1 || res.Shadowed[0] != "billing" ||
		len(res.Set) != 1 || res.Set[0] != "search" {
		t.Fatalf("sync over manual rows = (%+v, %v)", res, err)
	}
	rows := idpGrantRows(t, pool, u)
	if rows["billing"] != (idpRow{RoleViewer, GrantSourceManual}) ||
		rows["payroll"] != (idpRow{RoleOperator, GrantSourceManual}) ||
		rows["search"] != (idpRow{RoleViewer, GrantSourceIdP}) || len(rows) != 3 {
		t.Fatalf("rows = %+v, want manual billing and payroll kept", rows)
	}
	shadow := idpAuditDetails(t, pool, AuditIdPGrantShadowed, u)
	if len(shadow) != 1 || shadow[0]["database"] != "billing" ||
		shadow[0]["idp_role"] != RoleOperator || shadow[0]["manual_role"] != RoleViewer {
		t.Fatalf("shadowed audit = %+v", shadow)
	}
	// Empty groups: only the IdP row goes.
	res, err = s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u})
	if err != nil || len(res.Revoked) != 1 || res.Revoked[0] != "search" {
		t.Fatalf("empty sync = (%+v, %v), want only search revoked", res, err)
	}
	if rows := idpGrantRows(t, pool, u); len(rows) != 2 ||
		rows["billing"].source != GrantSourceManual {
		t.Fatalf("rows after empty sync = %+v, want the two manual rows", rows)
	}
}

func TestIdPGrantSync_ManualSetTakesOverAnIdPRow(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "idp-take-admin@example.com", RoleAdmin)
	u := linkTestUser(t, pool, "idp-take@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	if _, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
		Grants: map[string]string{"billing": RoleOperator}}); err != nil {
		t.Fatal(err)
	}
	g, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: u, Database: "billing",
		Role: RoleViewer})
	if err != nil || g.Source != GrantSourceManual {
		t.Fatalf("manual set over an IdP row = (%+v, %v), want source manual", g, err)
	}
	if rows := idpGrantRows(t, pool, u); rows["billing"] != (idpRow{RoleViewer,
		GrantSourceManual}) {
		t.Fatalf("row = %+v, want a manual viewer grant", rows["billing"])
	}
	// The next login no longer owns the row.
	res, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u})
	if err != nil || len(res.Revoked) != 0 {
		t.Fatalf("sync after takeover = (%+v, %v), want nothing revoked", res, err)
	}
}

func TestIdPGrantSource_RevokeOfIdPRowIsRefusedWhenProtected(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "idp-rev-admin@example.com", RoleAdmin)
	u := linkTestUser(t, pool, "idp-rev@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	if _, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
		Grants: map[string]string{"billing": RoleOperator}}); err != nil {
		t.Fatal(err)
	}
	err := s.RevokeGrant(ctx, GrantChange{ActorID: admin, UserID: u, Database: "billing",
		ProtectIdP: true})
	if !errors.Is(err, ErrIdPGrantReadOnly) {
		t.Fatalf("protected revoke of an IdP row = %v, want ErrIdPGrantReadOnly", err)
	}
	if rows := idpGrantRows(t, pool, u); len(rows) != 1 {
		t.Fatalf("a refused revoke removed the row: %+v", rows)
	}
	if n := len(idpAuditDetails(t, pool, AuditDatabaseGrantRevoked, u)); n != 0 {
		t.Fatalf("a refused revoke was audited (%d rows)", n)
	}
	// Without a mapping the row is a leftover an admin may remove.
	if err := s.RevokeGrant(ctx, GrantChange{ActorID: admin, UserID: u,
		Database: "billing"}); err != nil {
		t.Fatalf("unprotected revoke of a leftover IdP row: %v", err)
	}
	list, err := s.ListUserGrants(ctx, u)
	if err != nil || len(list) != 0 {
		t.Fatalf("grants after leftover revoke = (%+v, %v)", list, err)
	}
}

func TestIdPGrantSource_ListCarriesSource(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "idp-list-admin@example.com", RoleAdmin)
	u := linkTestUser(t, pool, "idp-list@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: u, Database: "aaa",
		Role: RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
		Grants: map[string]string{"bbb": RoleOperator}}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUserGrants(ctx, u)
	if err != nil || len(list) != 2 || list[0].Source != GrantSourceManual ||
		list[1].Source != GrantSourceIdP || list[1].GrantedBy != 0 {
		t.Fatalf("ListUserGrants = (%+v, %v), want aaa manual then bbb idp", list, err)
	}
}

func TestIdPGrantSync_UnknownUser(t *testing.T) {
	pool := setupOIDCPool(t)
	_, err := NewDatabaseGrantStore(pool).SyncIdPGrants(context.Background(),
		IdPGrantSync{UserID: 987654, Grants: map[string]string{"billing": RoleViewer}})
	if !errors.Is(err, ErrGrantUserNotFound) {
		t.Fatalf("sync for a missing user = %v, want ErrGrantUserNotFound", err)
	}
}

func TestIdPGrantSync_ConcurrentLoginsSameGroupsAuditOnce(t *testing.T) {
	pool := setupOIDCPool(t)
	u := linkTestUser(t, pool, "idp-race-same@example.com", RoleOperator)
	s := NewDatabaseGrantStore(idpWidePool(t, pool))
	runConcurrentSyncs(t, s, u, func(int) map[string]string {
		return map[string]string{"billing": RoleOperator}
	})
	rows := idpGrantRows(t, pool, u)
	if len(rows) != 1 || rows["billing"] != (idpRow{RoleOperator, GrantSourceIdP}) {
		t.Fatalf("rows = %+v, want one billing operator grant", rows)
	}
	if n := len(idpAuditDetails(t, pool, AuditDatabaseGrantSet, u)); n != 1 {
		t.Fatalf("%d set audit rows for 8 identical logins, want 1", n)
	}
}

func TestIdPGrantSync_ConcurrentLoginsLeaveOneConsistentSet(t *testing.T) {
	pool := setupOIDCPool(t)
	u := linkTestUser(t, pool, "idp-race-mixed@example.com", RoleOperator)
	s := NewDatabaseGrantStore(idpWidePool(t, pool))
	runConcurrentSyncs(t, s, u, func(i int) map[string]string {
		if i%2 == 0 {
			return map[string]string{"billing": RoleOperator}
		}
		return map[string]string{"search": RoleViewer}
	})
	rows := idpGrantRows(t, pool, u)
	billing := len(rows) == 1 && rows["billing"] == idpRow{RoleOperator, GrantSourceIdP}
	search := len(rows) == 1 && rows["search"] == idpRow{RoleViewer, GrantSourceIdP}
	if !billing && !search {
		t.Fatalf("rows = %+v, want exactly one login's grant set", rows)
	}
	sets := len(idpAuditDetails(t, pool, AuditDatabaseGrantSet, u))
	revs := len(idpAuditDetails(t, pool, AuditDatabaseGrantRevoked, u))
	if sets-revs != 1 {
		t.Fatalf("%d sets and %d revokes do not net to the one grant left", sets, revs)
	}
}

// idpWidePool is a second pool on the test database with room for the
// concurrent syncs: the shared test pool keeps one connection for the
// cross-package lock and so would serialize them in the pool instead.
func idpWidePool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.MaxConns = 10
	wide, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening a wider pool: %v", err)
	}
	t.Cleanup(wide.Close)
	return wide
}

func runConcurrentSyncs(t *testing.T, s *DatabaseGrantStore, u int,
	grants func(int) map[string]string) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.SyncIdPGrants(context.Background(), IdPGrantSync{UserID: u,
				Grants: grants(i)})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent sync: %v", err)
		}
	}
}

// A sync in flight holds the user row; a second login's sync waits for it
// rather than reading grants the first is about to change.
func TestIdPGrantSync_WaitsForASyncInFlight(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	u := linkTestUser(t, pool, "idp-wait@example.com", RoleOperator)
	wide := idpWidePool(t, pool)
	s := NewDatabaseGrantStore(wide)
	tx, err := wide.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := lockIdPSync(ctx, tx, u); err != nil {
		t.Fatalf("holding the sync lock: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
			Grants: map[string]string{"billing": RoleViewer}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a second sync ran while the first held the user (err %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sync after the first finished: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting sync never ran")
	}
	if rows := idpGrantRows(t, pool, u); rows["billing"] != (idpRow{RoleViewer,
		GrantSourceIdP}) {
		t.Fatalf("rows = %+v", rows)
	}
}

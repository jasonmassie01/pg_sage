package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// apiauth-01 on real PostgreSQL: an admin who saved a setting can be
// deleted, the sealed audit row keeps their id, and deactivation ends the
// user's sessions with an audit record.

func statusTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	role string) (int, string) {
	t.Helper()
	email := fmt.Sprintf("apiauth01-%s-%d@test.local", role, time.Now().UnixNano())
	id, err := auth.CreateUser(ctx, pool, email, "status-pass-1", role)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, id)
	})
	return id, email
}

func serveUsers(pool *pgxpool.Pool, method, path, body string,
	caller *auth.User) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	registerUserRoutes(mux, pool)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(req, caller))
	return w
}

func TestDeleteUser_AfterConfigWrite(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetWave2GlobalConfig(t, ctx, pool)
	actor := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	target, email := statusTestUser(t, ctx, pool, auth.RoleAdmin)
	base := config.DefaultConfig()
	controller := config.NewConfigController(base, nil, apiTrustOwner{})
	put := configGlobalPutHandler(store.NewConfigStore(pool), controller)
	w := doRequestWithUser(put, http.MethodPut, "/api/v1/config/global",
		`{"trust.level":"advisory","expected_generation":1}`,
		&auth.User{ID: target, Email: email, Role: auth.RoleAdmin})
	if w.Code != http.StatusOK {
		t.Fatalf("config put = %d; body=%s", w.Code, w.Body.String())
	}
	var rowID int64
	var actorEmail *string
	if err := pool.QueryRow(ctx, `SELECT id, changed_by_actor FROM sage.config_audit
		WHERE changed_by = $1 ORDER BY id DESC LIMIT 1`, target).
		Scan(&rowID, &actorEmail); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorEmail == nil || *actorEmail != email {
		t.Fatalf("changed_by_actor = %v, want %s", actorEmail, email)
	}

	del := serveUsers(pool, http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", target),
		"", actor)
	if del.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200; body=%s", del.Code, del.Body.String())
	}
	var changedBy int
	if err := pool.QueryRow(ctx, `SELECT changed_by FROM sage.config_audit WHERE id = $1`,
		rowID).Scan(&changedBy); err != nil {
		t.Fatalf("audit row after delete: %v", err)
	}
	if changedBy != target {
		t.Fatalf("changed_by = %d after delete, want %d", changedBy, target)
	}
	requireAuditRowSealed(t, ctx, pool, rowID)
}

func requireAuditRowSealed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, row int64) {
	t.Helper()
	links, err := auditchain.LinksForRow(ctx, pool, "config_audit", row)
	if err != nil || len(links) != 1 || links[0].Op != "I" {
		t.Fatalf("links of row %d = %+v, %v; want only its insert", row, links, err)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.ConfigAudit,
		auditchain.Window{FromSeq: links[0].Seq, ToSeq: links[0].Seq})
	if err != nil || !rep.OK() || rep.RowsChecked != 1 {
		t.Fatalf("verify config_audit row %d = %+v, %v; want OK", row, rep, err)
	}
}

func TestDeleteUser_UnrelatedForeignKeyIs409(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	actor := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	target, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.apiauth01_fk_probe (
		user_id int REFERENCES sage.users(id))`); err != nil {
		t.Fatalf("creating probe table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.apiauth01_fk_probe`)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO public.apiauth01_fk_probe VALUES ($1)`,
		target); err != nil {
		t.Fatalf("referencing the user: %v", err)
	}
	w := serveUsers(pool, http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", target), "", actor)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if got := errorText(t, w); !strings.Contains(got, "public.apiauth01_fk_probe") {
		t.Fatalf("error = %q, want the referencing table named", got)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.users WHERE id = $1`,
		target).Scan(&n); err != nil || n != 1 {
		t.Fatalf("user rows after refused delete = %d (%v), want 1", n, err)
	}
}

func TestSetUserStatus_EndToEnd(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	actor := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	target, _ := statusTestUser(t, ctx, pool, auth.RoleViewer)
	cookie, err := auth.CreateSession(ctx, pool, target)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	path := fmt.Sprintf("/api/v1/users/%d/status", target)
	w := serveUsers(pool, http.MethodPut, path, `{"disabled":true}`, actor)
	requireStatusBody(t, w, "disabled", true)
	if _, err := auth.ValidateSession(ctx, pool, cookie); err == nil {
		t.Fatal("session survived deactivation")
	}
	requireListedDisabled(t, pool, actor, target, true)
	requireStatusAudit(t, ctx, pool, target, actor.ID, auth.AuditUserDisabled, 1)

	again := serveUsers(pool, http.MethodPut, path, `{"disabled":true}`, actor)
	requireStatusBody(t, again, "disabled", false)
	requireStatusAudit(t, ctx, pool, target, actor.ID, auth.AuditUserDisabled, 1)

	on := serveUsers(pool, http.MethodPut, path, `{"disabled":false}`, actor)
	requireStatusBody(t, on, "enabled", true)
	requireListedDisabled(t, pool, actor, target, false)
	requireStatusAudit(t, ctx, pool, target, actor.ID, auth.AuditUserEnabled, 1)

	missing := serveUsers(pool, http.MethodPut, "/api/v1/users/2147483000/status",
		`{"disabled":true}`, actor)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", missing.Code)
	}
}

func requireStatusBody(t *testing.T, w *httptest.ResponseRecorder, status string,
	changed bool) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status call = %d; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != status || body.Changed != changed {
		t.Fatalf("body = %+v, want status %s changed %v", body, status, changed)
	}
}

func requireListedDisabled(t *testing.T, pool *pgxpool.Pool, actor *auth.User,
	target int, want bool) {
	t.Helper()
	w := serveUsers(pool, http.MethodGet, "/api/v1/users", "", actor)
	var body struct {
		Users []struct {
			ID         int        `json:"id"`
			Disabled   bool       `json:"disabled"`
			DisabledAt *time.Time `json:"disabled_at"`
		} `json:"users"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	for _, u := range body.Users {
		if u.ID == target {
			if u.Disabled != want || (u.DisabledAt != nil) != want {
				t.Fatalf("listed user = %+v, want disabled %v", u, want)
			}
			return
		}
	}
	t.Fatalf("user %d not listed", target)
}

func requireStatusAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	target, actor int, event string, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = $2 AND actor_user_id = $3`,
		event, target, actor).Scan(&n); err != nil {
		t.Fatalf("counting %s audit rows: %v", event, err)
	}
	if n != want {
		t.Fatalf("%s audit rows = %d, want %d", event, n, want)
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// apiauth-04: logout is audited, and "sign out everywhere else" ends every
// other session of the caller while keeping the one it was sent from.

func sessionTestHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerAuthRoutes(mux, pool, nil, &config.Config{})
	return SessionAuthMiddleware(pool)(mux)
}

func sessionLogin(t *testing.T, h http.Handler, email, password string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: status %d, body %s", email, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "sage_session" && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("login %s set no sage_session cookie", email)
	return ""
}

func sessionCall(h http.Handler, method, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sage_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func authAuditRows(
	t *testing.T, pool *pgxpool.Pool, event string, userID int,
) []map[string]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT detail::text
		FROM sage.auth_audit WHERE event = $1 AND target_user_id = $2
		AND actor_user_id = $2 ORDER BY id`, event, userID)
	if err != nil {
		t.Fatalf("reading auth_audit: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scanning auth_audit: %v", err)
		}
		d := map[string]any{"_raw": raw}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("decoding detail %q: %v", raw, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating auth_audit: %v", err)
	}
	return out
}

func TestLogout_EndsSessionAndAuditsOnce(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	uid, err := auth.CreateUser(ctx, pool, "logout-audit@test.com", "password1", "viewer")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := sessionTestHandler(pool)
	tok := sessionLogin(t, h, "logout-audit@test.com", "password1")
	before := len(authAuditRows(t, pool, auth.AuditLogout, uid))

	if w := sessionCall(h, "POST", "/api/v1/auth/logout", tok); w.Code != http.StatusOK {
		t.Fatalf("logout: status %d, body %s", w.Code, w.Body.String())
	}
	if w := sessionCall(h, "GET", "/api/v1/auth/me", tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: status %d, want 401", w.Code)
	}
	rows := authAuditRows(t, pool, auth.AuditLogout, uid)
	if len(rows)-before != 1 {
		t.Fatalf("logout wrote %d audit rows, want 1", len(rows)-before)
	}
	if raw := rows[len(rows)-1]["_raw"].(string); strings.Contains(raw, tok) ||
		strings.Contains(raw, auth.HashSessionToken(tok)) {
		t.Fatalf("logout audit detail %s carries the session secret", raw)
	}
}

func TestSignOutEverywhereElse(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	uid, err := auth.CreateUser(ctx, pool, "everywhere@test.com", "password1", "operator")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err = auth.CreateUser(ctx, pool, "bystander@test.com", "password2", "viewer")
	if err != nil {
		t.Fatalf("create bystander: %v", err)
	}
	h := sessionTestHandler(pool)
	a := sessionLogin(t, h, "everywhere@test.com", "password1")
	b := sessionLogin(t, h, "everywhere@test.com", "password1")
	c := sessionLogin(t, h, "bystander@test.com", "password2")

	w := sessionCall(h, "DELETE", "/api/v1/auth/sessions", a)
	if w.Code != http.StatusOK {
		t.Fatalf("sign out elsewhere: status %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["revoked"] != float64(1) {
		t.Fatalf("response %v, want revoked=1", resp)
	}
	for name, want := range map[string]struct {
		tok  string
		code int
	}{"A": {a, 200}, "B": {b, 401}, "bystander": {c, 200}} {
		if got := sessionCall(h, "GET", "/api/v1/auth/me", want.tok).Code; got != want.code {
			t.Errorf("session %s: me = %d, want %d", name, got, want.code)
		}
	}
	rows := authAuditRows(t, pool, auth.AuditSessionsRevoked, uid)
	if len(rows) != 1 || rows[0]["count"] != float64(1) {
		t.Fatalf("sessions_revoked audit = %v, want one row with count 1", rows)
	}
	for _, secret := range []string{a, b, auth.HashSessionToken(a), auth.HashSessionToken(b)} {
		if strings.Contains(rows[0]["_raw"].(string), secret) {
			t.Fatal("sessions_revoked audit detail carries a session secret")
		}
	}
	// Repeating it is harmless: nothing else is left to end.
	w = sessionCall(h, "DELETE", "/api/v1/auth/sessions", a)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revoked":0`) {
		t.Fatalf("second sign out elsewhere: %d %s", w.Code, w.Body.String())
	}
}

func TestSignOutEverywhereElse_RequiresSession(t *testing.T) {
	// Middleware not applied: the handler must still refuse an anonymous call.
	w := doRequest(signOutOtherSessionsHandler(nil), "DELETE", "/api/v1/auth/sessions", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous sign out elsewhere: status %d, want 401", w.Code)
	}
}

func TestSessionMiddleware_MalformedCookieIs401WithoutQuery(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // any query would fail and surface as 503
	h := sessionTestHandler(pool)
	for name, tok := range map[string]string{
		"non-base64": "not-a-session!", "uuid": "00000000-0000-0000-0000-000000000000",
		"10 KB": strings.Repeat("A", 10*1024),
	} {
		got := sessionCall(h, "GET", "/api/v1/auth/me", tok).Code
		if got != http.StatusUnauthorized {
			t.Errorf("%s cookie: status %d, want 401", name, got)
		}
	}
	if got := sessionCall(h, "GET", "/api/v1/auth/me", "").Code; got != http.StatusUnauthorized {
		t.Errorf("no cookie: status %d, want 401", got)
	}
}

func TestSessionMiddleware_StoreFailureIsNot401(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	h := sessionTestHandler(pool)
	tok := strings.Repeat("A", 43) // well formed: 32 zero-ish bytes, base64url
	w := sessionCall(h, "GET", "/api/v1/auth/me", tok)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed session store: status %d, want 503 (body %s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), tok) {
		t.Fatal("503 body echoes the cookie")
	}
}

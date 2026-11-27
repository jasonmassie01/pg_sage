package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-06: one client address gets loginFailureBudgetLimit failed
// logins per window across all emails; a success neither resets the
// budget nor spends it.

const fixturePassword = "local-disposable-fixture-only"

// freshLoginLimiters isolates a test from the package-level limiters.
func freshLoginLimiters(t *testing.T) {
	t.Helper()
	if loginRateLimitDisabled {
		t.Skip("PG_SAGE_DISABLE_LOGIN_RATE_LIMIT=1 disables the limiters under test")
	}
	oldEmail, oldBudget := loginLimiter, loginFailureBudget
	loginLimiter, loginFailureBudget = newTestLimiter(), newTestFailureBudget()
	t.Cleanup(func() { loginLimiter, loginFailureBudget = oldEmail, oldBudget })
}

func loginBody(email, password string) string {
	data, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return string(data)
}

func TestLoginFailureBudget_TwentyFailuresAcrossEmailsThen429(t *testing.T) {
	freshLoginLimiters(t)
	f := newAuthzFixture(t, nil, nil)
	_, email := f.createUser(t, "real", auth.RoleViewer)
	for i := 1; i <= loginFailureBudgetLimit; i++ {
		spray := fmt.Sprintf("spray%02d@fixture.invalid", i)
		status, body := f.request(t, http.MethodPost, "/api/v1/auth/login",
			loginBody(spray, "wrong-password"))
		if status != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d %s, want 401", i, status, body)
		}
	}
	status, body := f.request(t, http.MethodPost, "/api/v1/auth/login",
		loginBody(email, fixturePassword))
	if status != http.StatusTooManyRequests {
		t.Fatalf("attempt %d with the right password = %d %s, want 429",
			loginFailureBudgetLimit+1, status, body)
	}
	if !strings.Contains(body, "too many login attempts") {
		t.Fatalf("429 body = %s, want the rate-limit message", body)
	}
}

func TestLoginFailureBudget_SuccessDoesNotReset(t *testing.T) {
	freshLoginLimiters(t)
	f := newAuthzFixture(t, nil, nil)
	_, email := f.createUser(t, "real", auth.RoleViewer)
	fail := func(i int) {
		t.Helper()
		status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login",
			loginBody(fmt.Sprintf("miss%02d@fixture.invalid", i), "wrong-password"))
		if status != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i, status)
		}
	}
	for i := 1; i < loginFailureBudgetLimit; i++ {
		fail(i)
	}
	if status, body := f.request(t, http.MethodPost, "/api/v1/auth/login",
		loginBody(email, fixturePassword)); status != http.StatusOK {
		t.Fatalf("login inside the budget = %d %s, want 200", status, body)
	}
	key := ClientLimiterKey("127.0.0.1")
	if got := len(loginFailureBudget.attempts[key]); got != loginFailureBudgetLimit-1 {
		t.Fatalf("budget after success = %d, want %d (not reset, not spent)",
			got, loginFailureBudgetLimit-1)
	}
	fail(loginFailureBudgetLimit)
	if status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login",
		loginBody(email, fixturePassword)); status != http.StatusTooManyRequests {
		t.Fatalf("right password after %d failures = %d, want 429",
			loginFailureBudgetLimit, status)
	}
}

func TestLoginFailureBudget_EmailLockDoesNotSpendBudget(t *testing.T) {
	freshLoginLimiters(t)
	f := newAuthzFixture(t, nil, nil)
	for i := 0; i <= loginMaxAttempts; i++ {
		f.request(t, http.MethodPost, "/api/v1/auth/login",
			loginBody("locked@fixture.invalid", "wrong-password"))
	}
	key := ClientLimiterKey("127.0.0.1")
	if got := len(loginFailureBudget.attempts[key]); got != loginMaxAttempts {
		t.Fatalf("budget spent = %d, want %d (the email-limited refusal is free)",
			got, loginMaxAttempts)
	}
}

func TestLoginFailureBudget_KeysIPv6By64(t *testing.T) {
	freshLoginLimiters(t)
	var addr atomic.Value
	addr.Store("2001:db8::1")
	SetClientIPResolver(func(*http.Request) string { return addr.Load().(string) })
	t.Cleanup(func() { SetClientIPResolver(nil) })
	f := newAuthzFixture(t, nil, nil)
	for i := 1; i <= loginFailureBudgetLimit; i++ {
		addr.Store(fmt.Sprintf("2001:db8::%x", i))
		f.request(t, http.MethodPost, "/api/v1/auth/login",
			loginBody(fmt.Sprintf("v6-%02d@fixture.invalid", i), "wrong-password"))
	}
	addr.Store("2001:db8::ffff")
	if status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login",
		loginBody("v6-next@fixture.invalid", "wrong-password")); status != 429 {
		t.Fatalf("same /64 after the budget = %d, want 429", status)
	}
	addr.Store("2001:db8:0:1::1")
	if status, _ := f.request(t, http.MethodPost, "/api/v1/auth/login",
		loginBody("v6-other@fixture.invalid", "wrong-password")); status != 401 {
		t.Fatalf("another /64 = %d, want 401 (its own budget)", status)
	}
}

// captureSlog routes the default slog logger into a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestLoginHandler_DBErrorReturns401AndLogsCause(t *testing.T) {
	freshLoginLimiters(t)
	logs := captureSlog(t)
	pool, err := pgxpool.New(t.Context(),
		"postgres://sage:unused@127.0.0.1:1/sage?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(loginBody("dberr@fixture.invalid", "s3cret-pw-value")))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { loginHandler(pool)(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("login against an unreachable database did not return")
	}
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(),
		"invalid credentials") {
		t.Fatalf("response = %d %s, want 401 invalid credentials", rec.Code, rec.Body)
	}
	out := logs.String()
	if !strings.Contains(out, "querying user") {
		t.Fatalf("log lacks the cause %q:\n%s", "querying user", out)
	}
	if strings.Contains(out, "s3cret-pw-value") || strings.Contains(out, "dberr@") {
		t.Fatalf("log leaked the password or email:\n%s", out)
	}
	if got := len(loginFailureBudget.attempts["192.0.2.1"]); got != 1 {
		t.Fatalf("budget after a DB error = %d, want 1 (fails closed)", got)
	}
}

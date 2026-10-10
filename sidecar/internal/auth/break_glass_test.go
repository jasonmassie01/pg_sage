package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// fixtureBreakGlassHash is a cost-4 bcrypt hash of the throwaway fixture
// string "fixture-break-glass-pw"; it guards nothing.
const fixtureBreakGlassHash = "$2a$04$C1ODnQFDul1rqzdJ52pN1uFXtmzyOArkugPCm1mrPJqtPc4WJgGey"

const fixtureBreakGlassPassword = "fixture-break-glass-pw"

func breakGlassConfig() config.BreakGlassConfig {
	return config.BreakGlassConfig{Enabled: true, PasswordHash: fixtureBreakGlassHash}
}

func TestBreakGlassLogin_Disabled(t *testing.T) {
	cases := map[string]config.BreakGlassConfig{
		"off":            {},
		"off with hash":  {PasswordHash: fixtureBreakGlassHash},
		"on without key": {Enabled: true},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			u, err := BreakGlassLogin(context.Background(), nil, cfg,
				fixtureBreakGlassPassword)
			if !errors.Is(err, ErrBreakGlassDisabled) || u != nil {
				t.Fatalf("= (%v, %v), want ErrBreakGlassDisabled", u, err)
			}
		})
	}
}

func TestBreakGlassLogin_WrongPasswordDenied(t *testing.T) {
	for _, pw := range []string{"", "wrong", fixtureBreakGlassPassword + " ",
		strings.ToUpper(fixtureBreakGlassPassword)} {
		u, err := BreakGlassLogin(context.Background(), nil, breakGlassConfig(), pw)
		if !errors.Is(err, ErrBreakGlassDenied) || u != nil {
			t.Fatalf("password %q: = (%v, %v), want ErrBreakGlassDenied", pw, u, err)
		}
	}
}

func TestBreakGlassLogin_GrantsAdminOnDedicatedAccount(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	u, err := BreakGlassLogin(ctx, pool, breakGlassConfig(), fixtureBreakGlassPassword)
	if err != nil {
		t.Fatalf("BreakGlassLogin: %v", err)
	}
	if u.Email != BreakGlassEmail || u.Role != RoleAdmin || u.ID <= 0 {
		t.Fatalf("user = %+v, want the break-glass admin", u)
	}
	var hash *string
	var provider string
	if err := pool.QueryRow(ctx, `SELECT password, oauth_provider FROM sage.users
		WHERE id = $1`, u.ID).Scan(&hash, &provider); err != nil {
		t.Fatal(err)
	}
	if hash != nil {
		t.Fatal("break-glass account has a stored password: it must only work via config")
	}
	if provider != BreakGlassProvider {
		t.Fatalf("oauth_provider = %q, want %q", provider, BreakGlassProvider)
	}
	if _, err := Authenticate(ctx, pool, BreakGlassEmail, fixtureBreakGlassPassword); err == nil {
		t.Fatal("break-glass password worked on the normal password login")
	}
}

func TestBreakGlassLogin_ReusesAccountAndRestoresAdmin(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	first, err := BreakGlassLogin(ctx, pool, breakGlassConfig(), fixtureBreakGlassPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.users SET role = 'viewer' WHERE id = $1`,
		first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := BreakGlassLogin(ctx, pool, breakGlassConfig(), fixtureBreakGlassPassword)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Role != RoleAdmin {
		t.Fatalf("second use = %+v, want same id %d restored to admin", second, first.ID)
	}
}

func TestBreakGlassLogin_RefusesToHijackExistingAccount(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.users WHERE email = $1`,
		BreakGlassEmail); err != nil {
		t.Fatal(err)
	}
	linkTestUser(t, pool, BreakGlassEmail, RoleViewer)
	u, err := BreakGlassLogin(ctx, pool, breakGlassConfig(), fixtureBreakGlassPassword)
	if !errors.Is(err, ErrBreakGlassConflict) || u != nil {
		t.Fatalf("= (%v, %v), want ErrBreakGlassConflict", u, err)
	}
	var role string
	_ = pool.QueryRow(ctx, `SELECT role FROM sage.users WHERE email = $1`,
		BreakGlassEmail).Scan(&role)
	if role != RoleViewer {
		t.Fatalf("existing account role changed to %q", role)
	}
}

func TestCreateSessionWithDuration(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "short-session@example.com", RoleViewer)
	sid, err := CreateSessionWithDuration(ctx, pool, id, BreakGlassSessionDuration)
	if err != nil {
		t.Fatal(err)
	}
	var left time.Duration
	var secs float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM expires_at - now())
		FROM sage.sessions WHERE id = $1`, sid).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	left = time.Duration(secs * float64(time.Second))
	if left > BreakGlassSessionDuration || left < BreakGlassSessionDuration-time.Minute {
		t.Fatalf("session lifetime %v, want about %v", left, BreakGlassSessionDuration)
	}
	if BreakGlassSessionDuration >= SessionDuration {
		t.Fatal("break-glass sessions must be shorter than normal sessions")
	}
	if _, err := CreateSessionWithDuration(ctx, pool, id, 0); err == nil {
		t.Fatal("zero-length session accepted")
	}
}

func TestRecordAuthAudit_StoresSourceIPAndDetail(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	err := RecordAuthAudit(ctx, pool, AuthAuditEvent{
		Event: AuditLoginFailed, ActorUserID: 0, TargetUserID: 77,
		SourceIP: "203.0.113.9", Detail: map[string]any{"method": "password"},
	})
	if err != nil {
		t.Fatalf("RecordAuthAudit: %v", err)
	}
	var ip, method string
	if err := pool.QueryRow(ctx, `SELECT source_ip, detail->>'method' FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = 77 ORDER BY id DESC LIMIT 1`,
		AuditLoginFailed).Scan(&ip, &method); err != nil {
		t.Fatal(err)
	}
	if ip != "203.0.113.9" || method != "password" {
		t.Fatalf("row = (%q, %q)", ip, method)
	}
}

func TestRecordAuthAudit_EmptySourceIPAndNilDetail(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if err := RecordAuthAudit(ctx, pool, AuthAuditEvent{
		Event: AuditUserDeleted, TargetUserID: 78,
	}); err != nil {
		t.Fatal(err)
	}
	var ip, detail string
	if err := pool.QueryRow(ctx, `SELECT source_ip, detail::text FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = 78`, AuditUserDeleted).Scan(&ip, &detail); err != nil {
		t.Fatal(err)
	}
	if ip != "" || detail != "{}" {
		t.Fatalf("row = (%q, %q), want empty ip and {}", ip, detail)
	}
}

func TestRecordAuthAudit_RejectsEmptyEvent(t *testing.T) {
	if err := RecordAuthAudit(context.Background(), nil, AuthAuditEvent{}); err == nil {
		t.Fatal("empty event accepted")
	}
}

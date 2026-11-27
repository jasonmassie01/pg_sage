package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// databricks-07 on real PostgreSQL: the first forwarded user value seen for
// an account is pinned; a different value later is audited and, in
// enforce mode, refused.

func pinTestConfig(mode string) DatabricksAppConfig {
	ws := fmt.Sprintf("https://pin-%d.cloud.databricks.com", time.Now().UnixNano())
	return DatabricksAppConfig{Workspace: ws, AdminEmail: "owner@example.com",
		IdentityPin: mode}
}

func pinEmail(name string) string {
	return fmt.Sprintf("pin-%s-%d@example.com", name, time.Now().UnixNano())
}

func storedPin(t *testing.T, pool *pgxpool.Pool, id int) *string {
	t.Helper()
	var pin *string
	if err := pool.QueryRow(context.Background(),
		`SELECT databricks_user_pin FROM sage.users WHERE id = $1`, id).Scan(&pin); err != nil {
		t.Fatalf("reading pin: %v", err)
	}
	return pin
}

type auditRow struct {
	actor, target int
	detail        string
}

func pinAuditRows(t *testing.T, pool *pgxpool.Pool, event string, target int) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT actor_user_id, target_user_id,
		detail::text FROM sage.auth_audit WHERE event = $1 AND target_user_id = $2
		ORDER BY id`, event, target)
	if err != nil {
		t.Fatalf("reading audit: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.target, &r.detail); err != nil {
			t.Fatalf("scanning audit: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestDatabricksAppPinFirstSightThenEnforceMismatch(t *testing.T) {
	pool := setupOIDCPool(t)
	resetPinAudits(t)
	ctx := context.Background()
	cfg := pinTestConfig(DatabricksAppPinEnforce)
	u, err := ResolveDatabricksAppUser(ctx, pool, cfg, pinEmail("enforce"), "idp-1")
	if err != nil || u == nil || u.Role != RoleViewer {
		t.Fatalf("first sight = %+v, %v", u, err)
	}
	if p := storedPin(t, pool, u.ID); p == nil ||
		*p != DatabricksAppUserPin(cfg.Workspace, "idp-1") {
		t.Fatalf("stored pin = %v, want the hash of the first value", p)
	}
	again, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-1")
	if err != nil || again == nil || again.ID != u.ID {
		t.Fatalf("same value again = %+v, %v", again, err)
	}
	if n := len(pinAuditRows(t, pool, AuditDatabricksAppIdentityChanged, u.ID)); n != 0 {
		t.Fatalf("a matching value wrote %d audit rows", n)
	}
	got, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-2")
	if !errors.Is(err, ErrDatabricksAppIdentityChanged) || got != nil {
		t.Fatalf("enforce mismatch = %+v, %v; want ErrDatabricksAppIdentityChanged", got, err)
	}
	rows := pinAuditRows(t, pool, AuditDatabricksAppIdentityChanged, u.ID)
	if len(rows) != 1 || !strings.Contains(rows[0].detail, `"refused": true`) {
		t.Fatalf("audit rows = %+v, want one refused event", rows)
	}
	if strings.Contains(rows[0].detail, "@") || strings.Contains(rows[0].detail, "idp-2") {
		t.Fatalf("audit detail leaks identity values: %s", rows[0].detail)
	}
	if p := storedPin(t, pool, u.ID); *p != DatabricksAppUserPin(cfg.Workspace, "idp-1") {
		t.Fatal("a refused mismatch replaced the pin")
	}
}

func TestDatabricksAppPinLogModeAllowsAndAudits(t *testing.T) {
	pool := setupOIDCPool(t)
	resetPinAudits(t)
	ctx := context.Background()
	cfg := pinTestConfig(DatabricksAppPinLog)
	u, err := ResolveDatabricksAppUser(ctx, pool, cfg, pinEmail("log"), "idp-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sage.users SET role = 'admin' WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-2")
	if err != nil || got == nil || got.ID != u.ID || got.Role != RoleAdmin {
		t.Fatalf("log mismatch = %+v, %v; want the request through", got, err)
	}
	rows := pinAuditRows(t, pool, AuditDatabricksAppIdentityChanged, u.ID)
	if len(rows) != 1 || !strings.Contains(rows[0].detail, `"refused": false`) {
		t.Fatalf("audit rows = %+v, want one allowed event", rows)
	}
	// Repeated requests from the changed identity are not re-audited.
	if _, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-2"); err != nil {
		t.Fatal(err)
	}
	if n := len(pinAuditRows(t, pool, AuditDatabricksAppIdentityChanged, u.ID)); n != 1 {
		t.Fatalf("repeat mismatch wrote %d rows in total, want 1", n)
	}
	if p := storedPin(t, pool, u.ID); *p != DatabricksAppUserPin(cfg.Workspace, "idp-1") {
		t.Fatal("log mode replaced the pin")
	}
}

func TestDatabricksAppPinAbsentAndOffWriteNothing(t *testing.T) {
	pool := setupOIDCPool(t)
	resetPinAudits(t)
	ctx := context.Background()
	cfg := pinTestConfig(DatabricksAppPinEnforce)
	u, err := ResolveDatabricksAppUser(ctx, pool, cfg, pinEmail("absent"), "")
	if err != nil || u == nil {
		t.Fatalf("absent value = %+v, %v", u, err)
	}
	if p := storedPin(t, pool, u.ID); p != nil {
		t.Fatalf("absent value pinned %q", *p)
	}
	off := cfg
	off.IdentityPin = DatabricksAppPinOff
	if _, err := ResolveDatabricksAppUser(ctx, pool, off, u.Email, "idp-1"); err != nil {
		t.Fatal(err)
	}
	if p := storedPin(t, pool, u.ID); p != nil {
		t.Fatalf("off mode pinned %q", *p)
	}
	// A pinned account still answers a request without the header.
	if _, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, ""); err != nil ||
		got.ID != u.ID {
		t.Fatalf("absent value on a pinned account = %+v, %v", got, err)
	}
	if _, err := ResolveDatabricksAppUser(ctx, pool, off, u.Email, "idp-9"); err != nil {
		t.Fatalf("off mode refused a mismatch: %v", err)
	}
	if n := len(pinAuditRows(t, pool, AuditDatabricksAppIdentityChanged, u.ID)); n != 0 {
		t.Fatalf("absent/off wrote %d audit rows", n)
	}
}

func TestDatabricksAppPinResetRelinks(t *testing.T) {
	pool := setupOIDCPool(t)
	resetPinAudits(t)
	ctx := context.Background()
	cfg := pinTestConfig(DatabricksAppPinEnforce)
	u, err := ResolveDatabricksAppUser(ctx, pool, cfg, pinEmail("reset"), "idp-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-2"); !errors.Is(
		err, ErrDatabricksAppIdentityChanged) {
		t.Fatalf("mismatch = %v", err)
	}
	cleared, err := ResetDatabricksAppUserPin(ctx, pool, 77, u.ID, "192.0.2.5")
	if err != nil || !cleared {
		t.Fatalf("reset = %v, %v; want a cleared pin", cleared, err)
	}
	if p := storedPin(t, pool, u.ID); p != nil {
		t.Fatalf("pin after reset = %q", *p)
	}
	rows := pinAuditRows(t, pool, AuditDatabricksAppPinReset, u.ID)
	if len(rows) != 1 || rows[0].actor != 77 ||
		!strings.Contains(rows[0].detail, "previous_pin_prefix") {
		t.Fatalf("reset audit = %+v, want one row by actor 77", rows)
	}
	got, err := ResolveDatabricksAppUser(ctx, pool, cfg, u.Email, "idp-2")
	if err != nil || got.ID != u.ID {
		t.Fatalf("after reset = %+v, %v", got, err)
	}
	if p := storedPin(t, pool, u.ID); p == nil ||
		*p != DatabricksAppUserPin(cfg.Workspace, "idp-2") {
		t.Fatalf("pin after relink = %v, want the new value", p)
	}
	// Resetting an unpinned account succeeds and says nothing was pinned.
	if _, err := ResetDatabricksAppUserPin(ctx, pool, 77, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if cleared, err := ResetDatabricksAppUserPin(ctx, pool, 77, u.ID, ""); err != nil || cleared {
		t.Fatalf("second reset = %v, %v; want nothing cleared", cleared, err)
	}
	if _, err := ResetDatabricksAppUserPin(ctx, pool, 77, 1<<30, ""); !errors.Is(
		err, ErrUserNotFound) {
		t.Fatalf("unknown user reset = %v, want ErrUserNotFound", err)
	}
}

// Two first-sight requests with different values race on one account:
// exactly one value is pinned and the other request is a mismatch.
func TestDatabricksAppPinConcurrentFirstSight(t *testing.T) {
	pool := setupOIDCPool(t)
	resetPinAudits(t)
	ctx := context.Background()
	wide, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wide.Close)
	cfg := pinTestConfig(DatabricksAppPinEnforce)
	for i := range 8 {
		email := pinEmail(fmt.Sprintf("race-%d", i))
		u, err := ResolveDatabricksAppUser(ctx, pool, cfg, email, "")
		if err != nil {
			t.Fatal(err)
		}
		values := []string{"idp-a", "idp-b"}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for j := range values {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[j] = ResolveDatabricksAppUser(ctx, wide, cfg, email, values[j])
			}()
		}
		close(start)
		wg.Wait()
		winners := 0
		for j, e := range errs {
			switch {
			case e == nil:
				winners++
				if p := storedPin(t, pool, u.ID); p == nil ||
					*p != DatabricksAppUserPin(cfg.Workspace, values[j]) {
					t.Fatalf("round %d: stored pin %v is not the winner's", i, p)
				}
			case !errors.Is(e, ErrDatabricksAppIdentityChanged):
				t.Fatalf("round %d: loser got %v, want a mismatch", i, e)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d winners (%v), want exactly 1", i, winners, errs)
		}
	}
}

// waitForLockWaiter blocks until a backend waits on a lock while running a
// statement containing marker, so the test knows the claim is in flight.
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool, marker string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND position($1 in query) > 0`,
			marker).Scan(&n); err != nil {
			t.Fatalf("reading pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the pin claim never waited on the competing transaction")
}

// Deterministic form of the race: another session pins the account after
// this request read NULL and before its claim commits. The claim must not
// overwrite the winner, and the request is a mismatch.
func TestDatabricksAppPinClaimLosesToConcurrentPin(t *testing.T) {
	pool := setupOIDCPool(t)
	resetPinAudits(t)
	ctx := context.Background()
	wide, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wide.Close)
	cfg := pinTestConfig(DatabricksAppPinEnforce)
	u, err := ResolveDatabricksAppUser(ctx, pool, cfg, pinEmail("claim"), "")
	if err != nil {
		t.Fatal(err)
	}
	winner := DatabricksAppUserPin(cfg.Workspace, "idp-a")
	tx, err := wide.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sage.users SET databricks_user_pin = $2
		WHERE id = $1`, u.ID, winner); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ResolveDatabricksAppUser(ctx, wide, cfg, u.Email, "idp-b")
		done <- err
	}()
	waitForLockWaiter(t, wide, "databricks_user_pin IS NULL")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrDatabricksAppIdentityChanged) {
		t.Fatalf("losing claim = %v, want ErrDatabricksAppIdentityChanged", err)
	}
	if p := storedPin(t, pool, u.ID); p == nil || *p != winner {
		t.Fatalf("stored pin = %v, want the winner's", p)
	}
}

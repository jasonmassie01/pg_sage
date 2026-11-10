package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// databricks-07: the App identity is keyed on a mutable email, so the
// resolver pins the IdP user value the Apps proxy forwards beside it.

func TestDatabricksAppIdentityPinDefaultsToLog(t *testing.T) {
	cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
		"DATABRICKS_APP_PORT": "8000", "SAGE_AUTH_MODE": "databricks_app",
	}))
	if err != nil || cfg == nil {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if cfg.IdentityPin != DatabricksAppPinLog {
		t.Fatalf("IdentityPin = %q, want %q with no env set", cfg.IdentityPin, DatabricksAppPinLog)
	}
}

func TestDatabricksAppIdentityPinModesFromEnv(t *testing.T) {
	for _, mode := range []string{"off", "log", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
				"DATABRICKS_APP_PORT": "8000", "SAGE_AUTH_MODE": "databricks_app",
				"SAGE_DATABRICKS_APP_IDENTITY_PIN": mode,
			}))
			if err != nil || cfg == nil || cfg.IdentityPin != mode {
				t.Fatalf("config = %+v, %v; want IdentityPin %q", cfg, err, mode)
			}
		})
	}
}

func TestDatabricksAppIdentityPinRejectsUnknownMode(t *testing.T) {
	for _, raw := range []string{"ENFORCE", "on", " log", "enforce,log", "true"} {
		t.Run(raw, func(t *testing.T) {
			cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
				"DATABRICKS_APP_PORT": "8000", "SAGE_AUTH_MODE": "databricks_app",
				"SAGE_DATABRICKS_APP_IDENTITY_PIN": raw,
			}))
			if cfg != nil || err == nil ||
				!strings.Contains(err.Error(), "SAGE_DATABRICKS_APP_IDENTITY_PIN") {
				t.Fatalf("mode %q = %+v, %v; want a startup error naming the variable",
					raw, cfg, err)
			}
		})
	}
}

var hexPin = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestDatabricksAppUserPinIsWorkspaceScopedHash(t *testing.T) {
	const ws = "https://workspace.cloud.databricks.com"
	pin := DatabricksAppUserPin(ws, "4242424242")
	if !hexPin.MatchString(pin) {
		t.Fatalf("pin %q is not hex SHA-256", pin)
	}
	// sha256("https://workspace.cloud.databricks.com\x004242424242")
	if strings.Contains(pin, "4242424242") {
		t.Fatal("pin carries the raw forwarded user value")
	}
	if pin != DatabricksAppUserPin(ws, "4242424242") {
		t.Fatal("pin is not deterministic")
	}
	if pin == DatabricksAppUserPin("https://other.cloud.databricks.com", "4242424242") {
		t.Fatal("pin does not depend on the workspace")
	}
	if pin == DatabricksAppUserPin(ws, "4242424243") {
		t.Fatal("pin does not depend on the user value")
	}
	// The separator keeps workspace and user from sliding into each other.
	if DatabricksAppUserPin("ab", "c") == DatabricksAppUserPin("a", "bc") {
		t.Fatal("pin input is ambiguous without a separator")
	}
}

// failingPinDB fails the test on any database access.
type failingPinDB struct{ t *testing.T }

func (f failingPinDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.t.Fatal("pin check wrote to the database")
	return pgconn.CommandTag{}, nil
}

func (f failingPinDB) QueryRow(context.Context, string, ...any) pgx.Row {
	f.t.Fatal("pin check read the database")
	return nil
}

func TestDatabricksAppPinOffNeverReadsTheColumn(t *testing.T) {
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinOff}
	u := &User{ID: 7, Role: RoleAdmin}
	got, err := cfg.checkIdentityPin(context.Background(), failingPinDB{t}, u, "user-1")
	if err != nil || got != u {
		t.Fatalf("off mode = %+v, %v; want the user unchanged", got, err)
	}
}

func TestDatabricksAppPinAbsentUserValueKeepsTodaysBehavior(t *testing.T) {
	for _, mode := range []string{DatabricksAppPinOff, DatabricksAppPinLog,
		DatabricksAppPinEnforce, ""} {
		t.Run(mode, func(t *testing.T) {
			cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
				IdentityPin: mode}
			u := &User{ID: 9, Role: RoleOperator}
			got, err := cfg.checkIdentityPin(context.Background(), failingPinDB{t}, u, "")
			if err != nil || got != u || got.Role != RoleOperator {
				t.Fatalf("absent user value = %+v, %v; want the user unchanged", got, err)
			}
		})
	}
}

// scriptedPinDB returns a stored pin and counts writes.
type scriptedPinDB struct {
	stored   *string
	readErr  error
	claimErr error
	claimed  int64
	audits   []string
	auditErr error
}

type pinRow struct {
	val *string
	err error
}

func (r pinRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(**string)) = r.val
	return nil
}

func (s *scriptedPinDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return pinRow{val: s.stored, err: s.readErr}
}

func (s *scriptedPinDB) Exec(_ context.Context, sql string, args ...any) (
	pgconn.CommandTag, error) {
	if strings.Contains(sql, "auth_audit") {
		s.audits = append(s.audits, args[0].(string)+" "+args[3].(string))
		return pgconn.NewCommandTag("INSERT 0 1"), s.auditErr
	}
	if s.claimErr != nil {
		return pgconn.CommandTag{}, s.claimErr
	}
	atomic.AddInt64(&s.claimed, 1)
	v := args[1].(string)
	s.stored = &v
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func TestDatabricksAppPinMismatchRefusedInEnforce(t *testing.T) {
	resetPinAudits(t)
	old := DatabricksAppUserPin("https://w.cloud.databricks.com", "user-old")
	db := &scriptedPinDB{stored: &old}
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinEnforce}
	got, err := cfg.checkIdentityPin(context.Background(), db, &User{ID: 31}, "user-new")
	if !errors.Is(err, ErrDatabricksAppIdentityChanged) || got != nil {
		t.Fatalf("enforce mismatch = %+v, %v; want ErrDatabricksAppIdentityChanged", got, err)
	}
	if len(db.audits) != 1 || !strings.HasPrefix(db.audits[0], AuditDatabricksAppIdentityChanged) {
		t.Fatalf("audits = %v, want one identity-changed event", db.audits)
	}
	if strings.Contains(db.audits[0], "user-new") || strings.Contains(db.audits[0], "@") ||
		strings.Contains(db.audits[0], old) {
		t.Fatalf("audit detail leaks an identity value or a whole pin: %s", db.audits[0])
	}
	if *db.stored != old {
		t.Fatal("a refused mismatch overwrote the stored pin")
	}
}

func TestDatabricksAppPinMismatchAllowedInLog(t *testing.T) {
	resetPinAudits(t)
	old := DatabricksAppUserPin("https://w.cloud.databricks.com", "user-old")
	db := &scriptedPinDB{stored: &old}
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinLog}
	u := &User{ID: 32, Role: RoleAdmin}
	got, err := cfg.checkIdentityPin(context.Background(), db, u, "user-new")
	if err != nil || got != u {
		t.Fatalf("log mismatch = %+v, %v; want the request through", got, err)
	}
	if len(db.audits) != 1 || !strings.Contains(db.audits[0], `"refused":false`) {
		t.Fatalf("audits = %v, want one allowed identity-changed event", db.audits)
	}
	// A failed audit write in log mode still lets the request through.
	resetPinAudits(t)
	db.auditErr = errors.New("audit table unavailable")
	got, err = cfg.checkIdentityPin(context.Background(), db, u, "user-new")
	if err != nil || got != u {
		t.Fatalf("log mismatch with failed audit = %+v, %v", got, err)
	}
}

// A failed audit write must not use up the de-duplication window: the next
// request with the same changed identity retries the audit.
func TestDatabricksAppPinFailedAuditIsRetried(t *testing.T) {
	resetPinAudits(t)
	old := DatabricksAppUserPin("https://w.cloud.databricks.com", "user-old")
	db := &scriptedPinDB{stored: &old, auditErr: errors.New("audit table unavailable")}
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinEnforce}
	u := &User{ID: 36}
	for i := range 2 {
		if _, err := cfg.checkIdentityPin(context.Background(), db, u, "user-new"); !errors.Is(
			err, ErrDatabricksAppIdentityChanged) {
			t.Fatalf("attempt %d with failed audit: err = %v, want refusal", i, err)
		}
	}
	if len(db.audits) != 2 {
		t.Fatalf("audit attempts = %d, want 2 (a failed write is retried)", len(db.audits))
	}
	db.auditErr = nil
	_, _ = cfg.checkIdentityPin(context.Background(), db, u, "user-new")
	_, _ = cfg.checkIdentityPin(context.Background(), db, u, "user-new")
	if len(db.audits) != 3 {
		t.Fatalf("audit attempts = %d, want 3 (one success, then de-duplicated)", len(db.audits))
	}
}

// A zero-valued mode is the documented default, never "off".
func TestDatabricksAppPinEmptyModeBehavesAsLog(t *testing.T) {
	resetPinAudits(t)
	old := DatabricksAppUserPin("https://w.cloud.databricks.com", "user-old")
	db := &scriptedPinDB{stored: &old}
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com"}
	_, err := cfg.checkIdentityPin(context.Background(), db, &User{ID: 33}, "user-new")
	if err != nil {
		t.Fatalf("empty mode refused: %v", err)
	}
	if len(db.audits) != 1 {
		t.Fatalf("empty mode audits = %v, want one", db.audits)
	}
}

func TestDatabricksAppPinMatchAndFirstSight(t *testing.T) {
	resetPinAudits(t)
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinEnforce}
	db := &scriptedPinDB{}
	u := &User{ID: 34}
	got, err := cfg.checkIdentityPin(context.Background(), db, u, "user-1")
	if err != nil || got != u {
		t.Fatalf("first sight = %+v, %v", got, err)
	}
	if db.claimed != 1 || db.stored == nil ||
		*db.stored != DatabricksAppUserPin(cfg.Workspace, "user-1") {
		t.Fatalf("first sight claimed=%d stored=%v; want the hashed pin", db.claimed, db.stored)
	}
	got, err = cfg.checkIdentityPin(context.Background(), db, u, "user-1")
	if err != nil || got != u {
		t.Fatalf("matching pin = %+v, %v", got, err)
	}
	if db.claimed != 1 || len(db.audits) != 0 {
		t.Fatalf("matching pin wrote claimed=%d audits=%v; want read-only", db.claimed, db.audits)
	}
}

func TestDatabricksAppPinStoreErrorsPropagate(t *testing.T) {
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinLog}
	for name, db := range map[string]*scriptedPinDB{
		"read":  {readErr: errors.New("connection reset by peer")},
		"claim": {claimErr: errors.New("connection reset by peer")},
		"gone":  {readErr: pgx.ErrNoRows},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := cfg.checkIdentityPin(context.Background(), db, &User{ID: 35}, "user-1")
			if err == nil || got != nil {
				t.Fatalf("store error = %+v, %v; want an error and no user", got, err)
			}
			if errors.Is(err, ErrDatabricksAppIdentityChanged) {
				t.Fatalf("store error reported as an identity change: %v", err)
			}
			if !strings.Contains(err.Error(), "identity pin") {
				t.Fatalf("error %q does not say what failed", err)
			}
		})
	}
}

func resetPinAudits(t *testing.T) {
	t.Helper()
	databricksPinAudits.reset()
	t.Cleanup(databricksPinAudits.reset)
}

func TestPinAuditLimiterWindowBoundary(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	l := newPinAuditLimiter(func() time.Time { return now })
	k := pinAuditKey{userID: 1, stored: "a", seen: "b"}
	if !l.allow(k) {
		t.Fatal("first mismatch was not recorded")
	}
	now = now.Add(pinAuditWindow - time.Second)
	if l.allow(k) {
		t.Fatal("repeat inside the window was recorded again")
	}
	now = now.Add(time.Second)
	if !l.allow(k) {
		t.Fatal("repeat at the window edge was suppressed")
	}
	if !l.allow(pinAuditKey{userID: 1, stored: "a", seen: "c"}) {
		t.Fatal("a different seen value was suppressed")
	}
	l.forget(1)
	if !l.allow(k) {
		t.Fatal("forget(user) did not clear that user's entries")
	}
}

func TestPinAuditLimiterIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	l := newPinAuditLimiter(func() time.Time { return now })
	for i := 0; i < pinAuditMaxKeys+10; i++ {
		if !l.allow(pinAuditKey{userID: i, stored: "a", seen: "b"}) {
			t.Fatalf("distinct key %d suppressed", i)
		}
	}
	if n := l.size(); n > pinAuditMaxKeys {
		t.Fatalf("limiter holds %d keys, want at most %d", n, pinAuditMaxKeys)
	}
}

func TestPinAuditLimiterConcurrentAllowOnce(t *testing.T) {
	l := newPinAuditLimiter(time.Now)
	k := pinAuditKey{userID: 5, stored: "a", seen: "b"}
	var wg sync.WaitGroup
	var allowed int64
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow(k) {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("%d concurrent mismatches recorded, want 1", allowed)
	}
}

// reset and size are test-only views of the limiter.
func (l *pinAuditLimiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.seen)
}

func (l *pinAuditLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// raceLostPinDB reads NULL, loses the claim (UPDATE 0), then reads the
// winner's pin; after retries it can also keep reading NULL.
type raceLostPinDB struct {
	reads    int
	winner   string
	alwaysNo bool
}

func (r *raceLostPinDB) QueryRow(context.Context, string, ...any) pgx.Row {
	r.reads++
	if r.reads == 1 || r.alwaysNo {
		return pinRow{}
	}
	return pinRow{val: &r.winner}
}

func (r *raceLostPinDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "auth_audit") {
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func TestDatabricksAppPinLostClaimIsMismatch(t *testing.T) {
	resetPinAudits(t)
	cfg := DatabricksAppConfig{Workspace: "https://w.cloud.databricks.com",
		IdentityPin: DatabricksAppPinEnforce}
	db := &raceLostPinDB{winner: DatabricksAppUserPin(cfg.Workspace, "idp-a")}
	got, err := cfg.checkIdentityPin(context.Background(), db, &User{ID: 36}, "idp-b")
	if !errors.Is(err, ErrDatabricksAppIdentityChanged) || got != nil || db.reads != 2 {
		t.Fatalf("lost claim = %+v, %v after %d reads; want a mismatch", got, err, db.reads)
	}
	// A pin that is cleared again between every read and claim is bounded.
	flap := &raceLostPinDB{alwaysNo: true}
	got, err = cfg.checkIdentityPin(context.Background(), flap, &User{ID: 37}, "idp-b")
	if err == nil || got != nil || errors.Is(err, ErrDatabricksAppIdentityChanged) ||
		flap.reads != 2 || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("flapping pin = %+v, %v after %d reads; want a bounded retry error",
			got, err, flap.reads)
	}
}

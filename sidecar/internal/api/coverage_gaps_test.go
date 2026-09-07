package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// ================================================================
// isBlockedHost / checkHost — SSRF protection (pure functions).
// ================================================================

func TestIsBlockedHost_MetadataHostnames(t *testing.T) {
	blocked := []string{
		"169.254.169.254",
		"metadata.google.internal",
	}
	for _, h := range blocked {
		if !isBlockedHost(h) {
			t.Errorf("metadata host %q should be blocked", h)
		}
	}
}

func TestIsBlockedHost_LoopbackIPs(t *testing.T) {
	cases := []string{"127.0.0.1", "127.1.2.3", "::1"}
	for _, h := range cases {
		if !isBlockedHost(h) {
			t.Errorf("loopback %q should be blocked", h)
		}
	}
}

func TestIsBlockedHost_PrivateIPs(t *testing.T) {
	cases := []string{
		"10.0.0.1",
		"10.255.255.254",
		"172.16.0.1",
		"172.31.255.254",
		"192.168.1.1",
		"192.168.255.254",
		"fc00::1",
		"fd00::1",
	}
	for _, h := range cases {
		if !isBlockedHost(h) {
			t.Errorf("private IP %q should be blocked", h)
		}
	}
}

func TestIsBlockedHost_LinkLocalIPs(t *testing.T) {
	cases := []string{
		"169.254.0.1",
		"169.254.1.2",
		"fe80::1",
	}
	for _, h := range cases {
		if !isBlockedHost(h) {
			t.Errorf("link-local %q should be blocked", h)
		}
	}
}

func TestIsBlockedHost_PublicIPs(t *testing.T) {
	cases := []string{
		"8.8.8.8",
		"1.1.1.1",
		"140.82.112.4",
		"2606:4700:4700::1111",
	}
	for _, h := range cases {
		if isBlockedHost(h) {
			t.Errorf("public IP %q should NOT be blocked", h)
		}
	}
}

func TestIsBlockedHost_UnresolvableHost(t *testing.T) {
	// An intentionally invalid TLD — should fail DNS and be blocked.
	h := "this-host-should-never-resolve.invalid"
	if !isBlockedHost(h) {
		t.Errorf("unresolvable %q should be blocked (fail closed)", h)
	}
}

func TestCheckHost_ReturnValues(t *testing.T) {
	// Metadata hostname returns hostBlocked.
	if got := checkHost("169.254.169.254"); got != hostBlocked {
		t.Errorf("metadata IP: got %v, want hostBlocked", got)
	}
	// Public IP returns hostAllowed.
	if got := checkHost("8.8.8.8"); got != hostAllowed {
		t.Errorf("public IP: got %v, want hostAllowed", got)
	}
	// Unresolvable returns hostDNSFailed.
	got := checkHost("this-host-should-never-resolve.invalid")
	if got != hostDNSFailed {
		t.Errorf("unresolvable: got %v, want hostDNSFailed", got)
	}
}

// ================================================================
// loginRateLimiter.purgeExpired — pure function over a mutex.
// ================================================================

func TestPurgeExpired_RemovesExpiredEntries(t *testing.T) {
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
	}
	now := time.Now()
	old := now.Add(-2 * loginWindow) // well outside window
	fresh := now.Add(-1 * time.Second)

	l.attempts["expired@test.com"] = []time.Time{old, old}
	l.attempts["mixed@test.com"] = []time.Time{old, fresh}
	l.attempts["fresh@test.com"] = []time.Time{fresh}

	l.purgeExpired()

	if _, ok := l.attempts["expired@test.com"]; ok {
		t.Error("all-expired entry should be deleted")
	}
	if got := l.attempts["mixed@test.com"]; len(got) != 1 {
		t.Errorf("mixed entry: got %d, want 1", len(got))
	}
	if got := l.attempts["fresh@test.com"]; len(got) != 1 {
		t.Errorf("fresh entry: got %d, want 1", len(got))
	}
}

func TestPurgeExpired_EmptyMap(t *testing.T) {
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
	}
	// Must not panic on empty map.
	l.purgeExpired()
	if len(l.attempts) != 0 {
		t.Errorf("empty map should stay empty, got %d entries",
			len(l.attempts))
	}
}

func TestPurgeExpired_ConcurrentSafety(t *testing.T) {
	// Guard against regressions in mutex protection.
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
	}
	for i := 0; i < 100; i++ {
		email := "user" + strconv.Itoa(i) + "@test.com"
		l.attempts[email] = []time.Time{time.Now()}
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.purgeExpired()
			l.allow("concurrent@test.com")
			l.record("concurrent@test.com")
		}()
	}
	wg.Wait()
}

func TestLoginRateLimiter_AllowRecordReset(t *testing.T) {
	// Covers allow/record/reset behavior in one pass.
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
	}
	email := "ratelimit@test.com"

	if !l.allow(email) {
		t.Fatal("first attempt should be allowed")
	}
	for i := 0; i < loginMaxAttempts; i++ {
		l.record(email)
	}
	if l.allow(email) {
		t.Error("should be rate-limited after max attempts")
	}
	l.reset(email)
	if !l.allow(email) {
		t.Error("should be allowed after reset")
	}
}

// ================================================================
// pendingActionsHandler / pendingCountHandler — live-DB tests.
// ================================================================

func TestPendingActionsHandler_Empty(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	as := store.NewActionStore(pool)
	handler := pendingActionsHandler(as, nil)

	w := doRequest(handler, "GET", "/api/v1/actions/pending", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp struct {
		Pending []map[string]any `json:"pending"`
		Total   int              `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("total: got %d, want 0", resp.Total)
	}
	if len(resp.Pending) != 0 {
		t.Errorf("pending: got %d, want 0", len(resp.Pending))
	}
}

func TestPendingActionsHandler_WithData(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	// Insert a finding so FK passes, then propose an action.
	var findingID int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, title, detail, status)
		 VALUES ('index_health','warning','t','{}','open')
		 RETURNING id`).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	as := store.NewActionStore(pool)
	_, err = as.Propose(ctx, nil, findingID,
		"DROP INDEX x", "", "low")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	handler := pendingActionsHandler(as, nil)
	w := doRequest(handler, "GET", "/api/v1/actions/pending", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp struct {
		Pending []map[string]any `json:"pending"`
		Total   int              `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total: got %d, want 1", resp.Total)
	}
	if len(resp.Pending) != 1 {
		t.Fatalf("pending: got %d, want 1", len(resp.Pending))
	}
	if resp.Pending[0]["proposed_sql"] != "DROP INDEX x" {
		t.Errorf("proposed_sql: got %v", resp.Pending[0]["proposed_sql"])
	}
}

func TestPendingActionsHandler_DatabaseFilter(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	// Insert a finding.
	var findingID int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, title, detail, status)
		 VALUES ('index_health','warning','t','{}','open')
		 RETURNING id`).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	as := store.NewActionStore(pool)
	// Propose with no database_id — should not match filter=999.
	_, err = as.Propose(ctx, nil, findingID,
		"DROP INDEX y", "", "low")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	handler := pendingActionsHandler(as, nil)
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending?database=999", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}

	var resp struct {
		Total int `json:"total"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Total != 0 {
		t.Errorf("filter mismatch should return 0, got %d",
			resp.Total)
	}
}

func TestPendingActionsHandler_InvalidDatabaseID(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	as := store.NewActionStore(pool)
	handler := pendingActionsHandler(as, nil)

	// database=notanumber should be silently ignored (dbID stays nil).
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending?database=notanumber", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
}

func TestPendingCountHandler_Empty(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	as := store.NewActionStore(pool)
	handler := pendingCountHandler(as)

	w := doRequest(handler, "GET", "/api/v1/actions/pending/count", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	var resp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("count: got %d, want 0", resp.Count)
	}
}

func TestPendingCountHandler_WithData(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	var findingID int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, title, detail, status)
		 VALUES ('index_health','warning','t','{}','open')
		 RETURNING id`).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	as := store.NewActionStore(pool)
	for i := 0; i < 3; i++ {
		_, err := as.Propose(ctx, nil, findingID,
			"DROP INDEX z"+strconv.Itoa(i), "", "low")
		if err != nil {
			t.Fatalf("propose: %v", err)
		}
	}

	handler := pendingCountHandler(as)
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending/count", "")

	var resp struct {
		Count int `json:"count"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Count != 3 {
		t.Errorf("count: got %d, want 3", resp.Count)
	}
}

// ================================================================
// testFromConnString / queryPGVersion / queryExtensions —
// exercise via handlers-adjacent code path; requires live DB.
// ================================================================

func TestTestFromConnString_Success(t *testing.T) {
	_, ctx := phase2RequireDB(t)

	result := testFromConnString(ctx, phase2DSN())
	if result.Status != "ok" {
		t.Errorf("status: got %q, want ok (err=%q)",
			result.Status, result.Error)
	}
	if result.PGVersion == "" {
		t.Error("PGVersion should be populated")
	}
	if !strings.Contains(
		strings.ToLower(result.PGVersion), "postgresql") {
		t.Errorf("PGVersion should mention PostgreSQL, got %q",
			result.PGVersion)
	}
	// Extensions is always at least [] (never nil in response).
	if result.Extensions == nil {
		t.Error("Extensions slice should not be nil")
	}
}

func TestTestFromConnString_ConnectionFails(t *testing.T) {
	// Port 1 is reserved; connection attempts should fail fast.
	bad := "postgres://nouser:nopass@127.0.0.1:1/db" +
		"?sslmode=disable&connect_timeout=1"

	result := testFromConnString(
		httptest.NewRequest("GET", "/", nil).Context(), bad)
	if result.Status != "error" {
		t.Errorf("status: got %q, want error", result.Status)
	}
	if result.Error == "" {
		t.Error("Error message should be populated")
	}
}

func TestTestFromConnString_InvalidConnString(t *testing.T) {
	result := testFromConnString(
		httptest.NewRequest("GET", "/", nil).Context(),
		"not-a-valid-conn-string://garbage")
	if result.Status != "error" {
		t.Errorf("status: got %q, want error", result.Status)
	}
}

// ================================================================
// v09 query helpers — incidents + growth forecasts.
// ================================================================

func TestQueryIncidents_OpenVsResolved(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.incidents")

	// Two open + one resolved.
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.incidents
		 (severity, root_cause, source, database_name)
		 VALUES ('warning','slow queries','deterministic','db1'),
		        ('critical','locks','deterministic','db1')`)
	if err != nil {
		t.Fatalf("insert open: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO sage.incidents
		 (severity, root_cause, source, database_name, resolved_at)
		 VALUES ('info','old thing','deterministic','db1', now())`)
	if err != nil {
		t.Fatalf("insert resolved: %v", err)
	}

	// Default (open).
	open, err := queryIncidents(ctx, pool, "", "", "")
	if err != nil {
		t.Fatalf("queryIncidents open: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("open count: got %d, want 2", len(open))
	}

	// Resolved only.
	resolved, err := queryIncidents(ctx, pool, "", "", "resolved")
	if err != nil {
		t.Fatalf("queryIncidents resolved: %v", err)
	}
	if len(resolved) != 1 {
		t.Errorf("resolved count: got %d, want 1", len(resolved))
	}

	// Severity filter.
	crit, err := queryIncidents(ctx, pool, "", "critical", "")
	if err != nil {
		t.Fatalf("queryIncidents critical: %v", err)
	}
	if len(crit) != 1 {
		t.Errorf("critical count: got %d, want 1", len(crit))
	}
	if crit[0]["severity"] != "critical" {
		t.Errorf("severity: got %v", crit[0]["severity"])
	}

	// Database filter (nonexistent).
	none, err := queryIncidents(ctx, pool, "nonexistent", "", "")
	if err != nil {
		t.Fatalf("queryIncidents nonexistent: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("nonexistent db: got %d, want 0", len(none))
	}
}

func TestQueryActiveIncidents(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.incidents")

	_, err := pool.Exec(ctx,
		`INSERT INTO sage.incidents
		 (severity, root_cause, source, database_name)
		 VALUES ('warning','w','deterministic','dbA'),
		        ('critical','c','deterministic','dbA'),
		        ('info','i','deterministic','dbB')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// All active.
	all, err := queryActiveIncidents(ctx, pool, "")
	if err != nil {
		t.Fatalf("queryActiveIncidents: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all active: got %d, want 3", len(all))
	}
	// Critical must be first due to ORDER BY CASE severity.
	if all[0]["severity"] != "critical" {
		t.Errorf("first severity: got %v, want critical",
			all[0]["severity"])
	}

	// Per-db filter.
	onlyA, err := queryActiveIncidents(ctx, pool, "dbA")
	if err != nil {
		t.Fatalf("queryActiveIncidents dbA: %v", err)
	}
	if len(onlyA) != 2 {
		t.Errorf("dbA count: got %d, want 2", len(onlyA))
	}
}

func TestQueryIncidentByID_AndResolve(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.incidents")

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.incidents
		 (severity, root_cause, source)
		 VALUES ('warning','rc','deterministic')
		 RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	row, err := queryIncidentByID(ctx, pool, id)
	if err != nil {
		t.Fatalf("queryIncidentByID: %v", err)
	}
	if row["root_cause"] != "rc" {
		t.Errorf("root_cause: got %v, want rc", row["root_cause"])
	}
	// resolved_at is stored as *time.Time; a nil pointer in a map
	// shows up as a typed nil, so compare through the known type.
	if rp, ok := row["resolved_at"].(*time.Time); ok && rp != nil {
		t.Errorf("resolved_at should be nil before resolve, got %v", rp)
	}

	// Resolve it.
	if err := resolveIncident(ctx, pool, id, "fixed"); err != nil {
		t.Fatalf("resolveIncident: %v", err)
	}

	// Verify it's resolved.
	row2, err := queryIncidentByID(ctx, pool, id)
	if err != nil {
		t.Fatalf("queryIncidentByID after resolve: %v", err)
	}
	rp, ok := row2["resolved_at"].(*time.Time)
	if !ok || rp == nil {
		t.Errorf("resolved_at should be non-nil *time.Time after resolve, got %T %v",
			row2["resolved_at"], row2["resolved_at"])
	}

	// Second resolve should error (already resolved).
	if err := resolveIncident(ctx, pool, id, "again"); err == nil {
		t.Error("second resolve should fail")
	}
}

func TestQueryIncidentByID_NotFound(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	// Well-formed UUID that doesn't exist.
	_, err := queryIncidentByID(ctx, pool,
		"00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Error("expected error for missing incident")
	}
}

func TestResolveIncident_NotFound(t *testing.T) {
	pool, ctx := phase2RequireDB(t)

	err := resolveIncident(ctx, pool,
		"00000000-0000-0000-0000-000000000000", "reason")
	if err == nil {
		t.Error("expected error resolving nonexistent incident")
	}
	if !strings.Contains(err.Error(),
		"not found or already resolved") {
		t.Errorf("error message: got %v", err)
	}
}

func TestQueryGrowthForecasts(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.size_history")

	// Seed two points for one object (needed for HAVING COUNT >= 2).
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.size_history
		 (collected_at, metric_type, object_name, size_bytes, database_name)
		 VALUES (now() - interval '2 days', 'table', 'users', 1000, 'db1'),
		        (now() - interval '1 day',  'table', 'users', 2000, 'db1'),
		        (now() - interval '1 hour', 'table', 'solo',  500,  'db1')`)
	if err != nil {
		t.Fatalf("seed size_history: %v", err)
	}

	forecasts, err := queryGrowthForecasts(ctx, pool, 7)
	if err != nil {
		t.Fatalf("queryGrowthForecasts: %v", err)
	}
	// "solo" has only 1 point → excluded by HAVING.
	if len(forecasts) != 1 {
		t.Fatalf("forecast count: got %d, want 1", len(forecasts))
	}
	if forecasts[0]["object_name"] != "users" {
		t.Errorf("object_name: got %v", forecasts[0]["object_name"])
	}
}

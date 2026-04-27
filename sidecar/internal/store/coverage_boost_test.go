package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
)

// ---------------------------------------------------------------------------
// DB setup helper — connects to local Postgres and skips if unavailable.
// ---------------------------------------------------------------------------

const coverageDSN = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

var (
	cbPool     *pgxpool.Pool
	cbPoolOnce sync.Once
	cbPoolErr  error
	cbKey      = crypto.DeriveKey("coverage-boost-test-key",
		[]byte("cov-test-salt-16"))
)

func coverageDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	cbPoolOnce.Do(func() {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		cfg, err := pgxpool.ParseConfig(coverageDSN)
		if err != nil {
			cbPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		// MaxConns=1 is required for session-scoped advisory locks
		// (serializeAcrossPackages) to protect the test's own queries.
		cfg.MaxConns = 1
		cbPool, cbPoolErr = pgxpool.NewWithConfig(qctx, cfg)
		if cbPoolErr != nil {
			return
		}
		if err := cbPool.Ping(qctx); err != nil {
			cbPoolErr = fmt.Errorf("ping: %w", err)
			cbPool.Close()
			cbPool = nil
			return
		}
		if err := schema.Bootstrap(qctx, cbPool); err != nil {
			cbPoolErr = fmt.Errorf("bootstrap: %w", err)
			cbPool.Close()
			cbPool = nil
			return
		}
		schema.ReleaseAdvisoryLock(qctx, cbPool)
		if err := schema.EnsureDatabasesTable(qctx, cbPool); err != nil {
			cbPoolErr = fmt.Errorf("ensure databases: %w", err)
			return
		}
		if err := schema.MigrateConfigSchema(qctx, cbPool); err != nil {
			cbPoolErr = fmt.Errorf("migrate config: %w", err)
			return
		}
		// Ensure test user exists for FK constraints.
		_, _ = cbPool.Exec(qctx, `
			INSERT INTO sage.users (id, email, password, role)
			VALUES (99, 'coverage@test.com', 'hashed', 'admin')
			ON CONFLICT (id) DO NOTHING`)
	})
	if cbPoolErr != nil {
		t.Skipf("database unavailable: %v", cbPoolErr)
	}
	serializeAcrossPackages(t, ctx, cbPool)
	return cbPool, ctx
}

// ---------------------------------------------------------------------------
// Constructor tests (pure unit, no DB)
// ---------------------------------------------------------------------------

func TestCoverage_NewActionStore(t *testing.T) {
	s := NewActionStore(nil)
	if s == nil {
		t.Fatal("NewActionStore(nil) returned nil")
	}
	if s.pool != nil {
		t.Error("expected nil pool, got non-nil")
	}
}

func TestCoverage_NewConfigStore(t *testing.T) {
	s := NewConfigStore(nil)
	if s == nil {
		t.Fatal("NewConfigStore(nil) returned nil")
	}
	if s.pool != nil {
		t.Error("expected nil pool, got non-nil")
	}
}

func TestCoverage_NewDatabaseStore(t *testing.T) {
	key := crypto.DeriveKey("test", []byte("unit-test-salt16"))
	s := NewDatabaseStore(nil, key)
	if s == nil {
		t.Fatal("NewDatabaseStore(nil, key) returned nil")
	}
	if s.pool != nil {
		t.Error("expected nil pool, got non-nil")
	}
	if len(s.encryptKey) != 32 {
		t.Errorf("encryptKey length = %d, want 32",
			len(s.encryptKey))
	}
}

func TestCoverage_NewNotificationStore(t *testing.T) {
	s := NewNotificationStore(nil, nil)
	if s == nil {
		t.Fatal("NewNotificationStore(nil, nil) returned nil")
	}
	if s.pool != nil {
		t.Error("expected nil pool")
	}
	if s.dispatcher != nil {
		t.Error("expected nil dispatcher")
	}
}

// ---------------------------------------------------------------------------
// Struct zero-value tests
// ---------------------------------------------------------------------------

func TestCoverage_DatabaseRecordZeroValue(t *testing.T) {
	var r DatabaseRecord
	if r.ID != 0 {
		t.Errorf("ID = %d, want 0", r.ID)
	}
	if r.Name != "" {
		t.Errorf("Name = %q, want empty", r.Name)
	}
	if r.Host != "" {
		t.Errorf("Host = %q, want empty", r.Host)
	}
	if r.Port != 0 {
		t.Errorf("Port = %d, want 0", r.Port)
	}
	if r.DatabaseName != "" {
		t.Errorf("DatabaseName = %q, want empty", r.DatabaseName)
	}
	if r.Username != "" {
		t.Errorf("Username = %q, want empty", r.Username)
	}
	if r.SSLMode != "" {
		t.Errorf("SSLMode = %q, want empty", r.SSLMode)
	}
	if r.MaxConnections != 0 {
		t.Errorf("MaxConnections = %d, want 0", r.MaxConnections)
	}
	if r.Enabled {
		t.Error("Enabled = true, want false")
	}
	if r.Tags != nil {
		t.Errorf("Tags = %v, want nil", r.Tags)
	}
	if r.TrustLevel != "" {
		t.Errorf("TrustLevel = %q, want empty", r.TrustLevel)
	}
	if r.ExecutionMode != "" {
		t.Errorf("ExecutionMode = %q, want empty", r.ExecutionMode)
	}
	if !r.CreatedAt.IsZero() {
		t.Error("CreatedAt is not zero")
	}
	if r.CreatedBy != 0 {
		t.Errorf("CreatedBy = %d, want 0", r.CreatedBy)
	}
	if !r.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is not zero")
	}
}

func TestCoverage_DatabaseInputZeroValue(t *testing.T) {
	var d DatabaseInput
	if d.Name != "" {
		t.Errorf("Name = %q, want empty", d.Name)
	}
	if d.Password != "" {
		t.Errorf("Password = %q, want empty", d.Password)
	}
	if d.Port != 0 {
		t.Errorf("Port = %d, want 0", d.Port)
	}
	if d.Tags != nil {
		t.Errorf("Tags = %v, want nil", d.Tags)
	}
}

func TestCoverage_NotificationLogEntryZeroValue(t *testing.T) {
	var e NotificationLogEntry
	if e.ID != 0 {
		t.Errorf("ID = %d, want 0", e.ID)
	}
	if e.ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil", e.ChannelID)
	}
	if e.Event != "" {
		t.Errorf("Event = %q, want empty", e.Event)
	}
	if e.Subject != "" {
		t.Errorf("Subject = %q, want empty", e.Subject)
	}
	if e.Body != "" {
		t.Errorf("Body = %q, want empty", e.Body)
	}
	if e.Status != "" {
		t.Errorf("Status = %q, want empty", e.Status)
	}
	if e.Error != "" {
		t.Errorf("Error = %q, want empty", e.Error)
	}
	if !e.SentAt.IsZero() {
		t.Error("SentAt is not zero")
	}
}

// ---------------------------------------------------------------------------
// validateByType branch coverage
// ---------------------------------------------------------------------------

func TestCoverage_ValidateByType_ExecMode(t *testing.T) {
	// The "exec_mode" vtype uses validateEnum with
	// validExecutionModes.
	err := validateByType("exec_mode", "test.key", "auto")
	if err != nil {
		t.Errorf("exec_mode auto: unexpected error: %v", err)
	}

	err = validateByType("exec_mode", "test.key", "approval")
	if err != nil {
		t.Errorf("exec_mode approval: unexpected error: %v", err)
	}

	err = validateByType("exec_mode", "test.key", "manual")
	if err != nil {
		t.Errorf("exec_mode manual: unexpected error: %v", err)
	}

	err = validateByType("exec_mode", "test.key", "bogus")
	if err == nil {
		t.Error("exec_mode bogus: expected error, got nil")
	}
}

func TestCoverage_ValidateByType_UnknownType(t *testing.T) {
	// Unknown vtypes fall through to the default case, returning nil.
	err := validateByType("unknown_vtype", "test.key", "anything")
	if err != nil {
		t.Errorf("unknown vtype: unexpected error: %v", err)
	}
}

func TestCoverage_ValidateByType_AllBranches(t *testing.T) {
	tests := []struct {
		vtype   string
		key     string
		value   string
		wantErr bool
	}{
		{"int_pos", "k", "10", false},
		{"int_pos", "k", "0", true},
		{"int_nonneg", "k", "0", false},
		{"int_nonneg", "k", "-1", true},
		{"int_min5", "k", "5", false},
		{"int_min5", "k", "4", true},
		{"pct", "k", "50", false},
		{"pct", "k", "101", true},
		{"pct1_100", "k", "1", false},
		{"pct1_100", "k", "0", true},
		{"float01", "k", "0.5", false},
		{"float01", "k", "2.0", true},
		{"bool", "k", "true", false},
		{"bool", "k", "maybe", true},
		{"trust_level", "k", "advisory", false},
		{"trust_level", "k", "invalid", true},
		{"exec_mode", "k", "auto", false},
		{"exec_mode", "k", "invalid", true},
		{"string", "k", "anything", false},
		{"future_unknown_type", "k", "val", false},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("%s/%s", tt.vtype, tt.value)
		t.Run(name, func(t *testing.T) {
			err := validateByType(tt.vtype, tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateByType(%q, %q, %q) err=%v, wantErr=%v",
					tt.vtype, tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateInput boundary tests
// ---------------------------------------------------------------------------

func TestCoverage_ValidateInput_AllSSLModes(t *testing.T) {
	for mode := range validSSLModes {
		input := validInput()
		input.SSLMode = mode
		if err := validateInput(input, true); err != nil {
			t.Errorf("sslmode %q: unexpected error: %v", mode, err)
		}
	}
}

func TestCoverage_ValidateInput_AllTrustLevels(t *testing.T) {
	for level := range validTrustLevels {
		input := validInput()
		input.TrustLevel = level
		if err := validateInput(input, true); err != nil {
			t.Errorf("trust_level %q: unexpected error: %v",
				level, err)
		}
	}
}

func TestCoverage_ValidateInput_AllExecutionModes(t *testing.T) {
	for mode := range validExecutionModes {
		input := validInput()
		input.ExecutionMode = mode
		if err := validateInput(input, true); err != nil {
			t.Errorf("execution_mode %q: unexpected error: %v",
				mode, err)
		}
	}
}

func TestCoverage_ValidateInput_NameLengthBoundary(t *testing.T) {
	// Exactly 63 characters is valid.
	input := validInput()
	input.Name = strings.Repeat("a", 63)
	if err := validateInput(input, true); err != nil {
		t.Errorf("63-char name: unexpected error: %v", err)
	}

	// 64 characters is invalid.
	input.Name = strings.Repeat("a", 64)
	if err := validateInput(input, true); err == nil {
		t.Error("64-char name: expected error, got nil")
	}
}

func TestCoverage_ValidateInput_PortBoundaries(t *testing.T) {
	tests := []struct {
		port    int
		wantErr bool
	}{
		{0, true},
		{1, false},
		{5432, false},
		{65535, false},
		{65536, true},
		{-1, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("port_%d", tt.port), func(t *testing.T) {
			input := validInput()
			input.Port = tt.port
			err := validateInput(input, true)
			if (err != nil) != tt.wantErr {
				t.Errorf("port %d: err=%v, wantErr=%v",
					tt.port, err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ActionStore DB integration tests
// ---------------------------------------------------------------------------

func insertActionStoreFinding(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	id int,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.findings
		 (id, category, severity, title, detail, status,
		  object_identifier)
		 VALUES ($1, $2, 'warning', $3, '{}', 'open', $4)
		 ON CONFLICT (id) DO UPDATE
		    SET status = 'open',
		        acted_on_at = NULL,
		        resolved_at = NULL`,
		id,
		fmt.Sprintf("action_store_%d", id),
		fmt.Sprintf("action store finding %d", id),
		fmt.Sprintf("action_store_obj_%d", id))
	if err != nil {
		t.Fatalf("insert finding %d: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.findings WHERE id = $1", id)
	})
}

func TestCoverage_ActionStore_ProposeAndGetByID(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(ctx, nil, 100,
		"CREATE INDEX CONCURRENTLY idx_cb ON t (c)",
		"DROP INDEX CONCURRENTLY idx_cb",
		"safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	// GetByID
	action, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if action.ID != id {
		t.Errorf("ID = %d, want %d", action.ID, id)
	}
	if action.ProposedSQL != "CREATE INDEX CONCURRENTLY idx_cb ON t (c)" {
		t.Errorf("ProposedSQL = %q", action.ProposedSQL)
	}
	if action.RollbackSQL != "DROP INDEX CONCURRENTLY idx_cb" {
		t.Errorf("RollbackSQL = %q", action.RollbackSQL)
	}
	if action.ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q, want safe", action.ActionRisk)
	}
	if action.Status != "pending" {
		t.Errorf("Status = %q, want pending", action.Status)
	}
	if action.FindingID != 100 {
		t.Errorf("FindingID = %d, want 100", action.FindingID)
	}
}

func TestCoverage_ActionStore_ProposeWithDatabaseID(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)

	dbID := 1
	id, err := s.Propose(ctx, &dbID, 101,
		"ANALYZE public.orders", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose with dbID: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	action, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if action.DatabaseID == nil || *action.DatabaseID != 1 {
		t.Errorf("DatabaseID = %v, want 1", action.DatabaseID)
	}
	// Empty rollback SQL should result in empty string.
	if action.RollbackSQL != "" {
		t.Errorf("RollbackSQL = %q, want empty", action.RollbackSQL)
	}
}

func TestCoverage_ActionStore_ListPendingAll(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)
	insertActionStoreFinding(t, pool, ctx, 102)

	id, err := s.Propose(ctx, nil, 102,
		"SELECT 1", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	actions, err := s.ListPending(ctx, nil)
	if err != nil {
		t.Fatalf("ListPending(nil): %v", err)
	}
	found := false
	for _, a := range actions {
		if a.ID == id {
			found = true
			if a.Status != "pending" {
				t.Errorf("status = %q, want pending", a.Status)
			}
		}
	}
	if !found {
		t.Error("proposed action not found in pending list")
	}
}

func TestCoverage_ActionStore_ListPendingByDatabase(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)
	insertActionStoreFinding(t, pool, ctx, 103)

	dbID := 42
	id, err := s.Propose(ctx, &dbID, 103,
		"VACUUM public.t", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	// Filter by database.
	actions, err := s.ListPending(ctx, &dbID)
	if err != nil {
		t.Fatalf("ListPending(dbID=%d): %v", dbID, err)
	}
	found := false
	for _, a := range actions {
		if a.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("action not found when filtering by database")
	}

	// Filter by different database should not find it.
	other := 999
	actions, err = s.ListPending(ctx, &other)
	if err != nil {
		t.Fatalf("ListPending(dbID=%d): %v", other, err)
	}
	for _, a := range actions {
		if a.ID == id {
			t.Error("action found under wrong database ID")
		}
	}
}

func TestCoverage_ActionStore_Approve(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)
	insertActionStoreFinding(t, pool, ctx, 104)

	id, err := s.Propose(ctx, nil, 104,
		"REINDEX INDEX idx_test", "", "moderate",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	action, err := s.Approve(ctx, id, 99)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if action.Status != "approved" {
		t.Errorf("status = %q, want approved", action.Status)
	}
	if action.DecidedBy == nil || *action.DecidedBy != 99 {
		t.Errorf("DecidedBy = %v, want 99", action.DecidedBy)
	}
	if action.DecidedAt == nil {
		t.Error("DecidedAt is nil, want non-nil")
	}
}

func TestCoverage_ActionStore_Reject(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)

	id, err := s.Propose(ctx, nil, 105,
		"DROP INDEX idx_unused", "", "moderate",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	err = s.Reject(ctx, id, 99, "too risky")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}

	action, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID after reject: %v", err)
	}
	if action.Status != "rejected" {
		t.Errorf("status = %q, want rejected", action.Status)
	}
	if action.Reason != "too risky" {
		t.Errorf("reason = %q, want 'too risky'", action.Reason)
	}
}

func TestCoverage_ActionStore_RejectNonexistent(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)

	err := s.Reject(ctx, 999999, 99, "not found")
	if err == nil {
		t.Error("expected error rejecting nonexistent action")
	}
	if !strings.Contains(err.Error(), "not found or not pending") {
		t.Errorf("error = %q, want 'not found or not pending'",
			err.Error())
	}
}

func TestCoverage_ActionStore_ExpireStale(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)

	// Insert already-expired action directly.
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.action_queue
		    (finding_id, proposed_sql, action_risk,
		     expires_at)
		 VALUES (200, 'SELECT 1', 'safe',
		     now() - INTERVAL '1 day')
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("inserting expired action: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	expired, err := s.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if expired < 1 {
		t.Error("expected at least 1 expired action")
	}

	action, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if action.Status != "expired" {
		t.Errorf("status = %q, want expired", action.Status)
	}
}

func TestCoverage_ActionStore_PendingCount(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)
	insertActionStoreFinding(t, pool, ctx, 106)

	// Get baseline count.
	before, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}

	id, err := s.Propose(ctx, nil, 106,
		"SELECT 1", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	after, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount after: %v", err)
	}
	if after != before+1 {
		t.Errorf("PendingCount after = %d, want %d",
			after, before+1)
	}
}

func TestCoverage_ActionStore_HasPendingForFinding(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)

	findingID := 107
	insertActionStoreFinding(t, pool, ctx, findingID)

	// Before propose, should be false.
	has, err := s.HasPendingForFinding(ctx, findingID)
	if err != nil {
		t.Fatalf("HasPendingForFinding: %v", err)
	}
	if has {
		t.Error("expected false before propose, got true")
	}

	id, err := s.Propose(ctx, nil, findingID,
		"SELECT 1", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx,
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})

	has, err = s.HasPendingForFinding(ctx, findingID)
	if err != nil {
		t.Fatalf("HasPendingForFinding after: %v", err)
	}
	if !has {
		t.Error("expected true after propose, got false")
	}
}

func TestCoverage_ActionStore_HidesPendingForResolvedFinding(t *testing.T) {
	pool, ctx := coverageDB(t)
	s := NewActionStore(pool)
	insertActionStoreFinding(t, pool, ctx, 108)

	id, err := s.Propose(ctx, nil, 108,
		"SELECT 1", "", "safe",
	)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})
	_, err = pool.Exec(ctx,
		`UPDATE sage.findings
		    SET status = 'resolved',
		        resolved_at = now(),
		        acted_on_at = now()
		  WHERE id = 108`)
	if err != nil {
		t.Fatalf("resolve finding: %v", err)
	}

	pending, err := s.ListPending(ctx, nil)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	for _, action := range pending {
		if action.ID == id {
			t.Fatal("resolved finding action was returned as pending")
		}
	}
	if _, err := s.Approve(ctx, id, 99); err == nil {
		t.Fatal("Approve succeeded for resolved finding action")
	}
}

// ---------------------------------------------------------------------------
// ConfigStore DB integration tests
// ---------------------------------------------------------------------------

// coverageDBWithUser wraps coverageDB and ensures user 99 exists.
// Schema tests running in parallel may drop/recreate the sage schema,
// which removes the test user inserted during sync.Once setup.
func coverageDBWithUser(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, `
		INSERT INTO sage.users (id, email, password, role)
		VALUES (99, 'coverage@test.com', 'hashed', 'admin')
		ON CONFLICT (id) DO NOTHING`)
	return pool, ctx
}

func TestCoverage_ConfigStore_SetAndGetOverrides(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	err := cs.SetOverride(ctx,
		"collector.interval_seconds", "90", 0, 99)
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.DeleteOverride(ctx,
			"collector.interval_seconds", 0)
	})

	overrides, err := cs.GetOverrides(ctx, 0)
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}
	found := false
	for _, o := range overrides {
		if o.Key == "collector.interval_seconds" {
			found = true
			if o.Value != "90" {
				t.Errorf("value = %q, want 90", o.Value)
			}
		}
	}
	if !found {
		t.Error("override not found in GetOverrides")
	}
}

func TestCoverage_ConfigStore_GetOverridesAllDatabases(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	// Set a global override.
	err := cs.SetOverride(ctx,
		"collector.batch_size", "200", 0, 99)
	if err != nil {
		t.Fatalf("SetOverride global: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.DeleteOverride(ctx, "collector.batch_size", 0)
	})

	// databaseID=-1 should return all overrides.
	overrides, err := cs.GetOverrides(ctx, -1)
	if err != nil {
		t.Fatalf("GetOverrides(-1): %v", err)
	}
	found := false
	for _, o := range overrides {
		if o.Key == "collector.batch_size" {
			found = true
		}
	}
	if !found {
		t.Error("override not found when querying all databases")
	}
}

func TestCoverage_ConfigStore_PerDBOverride(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	err := cs.SetOverride(ctx,
		"collector.max_queries", "50", 1, 99)
	if err != nil {
		t.Fatalf("SetOverride perDB: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.DeleteOverride(ctx, "collector.max_queries", 1)
	})

	overrides, err := cs.GetOverrides(ctx, 1)
	if err != nil {
		t.Fatalf("GetOverrides(1): %v", err)
	}
	found := false
	for _, o := range overrides {
		if o.Key == "collector.max_queries" && o.Value == "50" {
			found = true
		}
	}
	if !found {
		t.Error("per-DB override not found")
	}
}

func TestCoverage_ConfigStore_DeleteOverride(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	cs.SetOverride(ctx,
		"safety.query_timeout_ms", "3000", 0, 99)

	err := cs.DeleteOverride(ctx, "safety.query_timeout_ms", 0)
	if err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}

	overrides, _ := cs.GetOverrides(ctx, 0)
	for _, o := range overrides {
		if o.Key == "safety.query_timeout_ms" {
			t.Error("override still present after delete")
		}
	}
}

func TestCoverage_ConfigStore_DeleteOverridePerDB(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	cs.SetOverride(ctx,
		"safety.lock_timeout_ms", "5000", 2, 99)

	err := cs.DeleteOverride(ctx, "safety.lock_timeout_ms", 2)
	if err != nil {
		t.Fatalf("DeleteOverride perDB: %v", err)
	}
}

func TestCoverage_ConfigStore_SetOverrideValidation(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	// Invalid key.
	err := cs.SetOverride(ctx, "bogus.key", "42", 0, 99)
	if err == nil {
		t.Error("expected error for invalid key")
	}

	// Invalid value.
	err = cs.SetOverride(ctx,
		"collector.interval_seconds", "not-int", 0, 99)
	if err == nil {
		t.Error("expected error for non-integer value")
	}

	// Value too low.
	err = cs.SetOverride(ctx,
		"collector.interval_seconds", "2", 0, 99)
	if err == nil {
		t.Error("expected error for value below minimum")
	}
}

func TestCoverage_ConfigStore_GetMergedConfig(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	cfg, err := config.Load(
		[]string{"-mode", "standalone", "-pg-host", "localhost"})
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	// No overrides.
	merged, err := cs.GetMergedConfig(ctx, cfg, 0)
	if err != nil {
		t.Fatalf("GetMergedConfig: %v", err)
	}
	entry, ok := merged["collector.interval_seconds"]
	if !ok {
		t.Fatal("missing collector.interval_seconds")
	}
	em := entry.(map[string]any)
	if em["source"] != "yaml" {
		t.Errorf("source = %v, want yaml", em["source"])
	}

	// Set global override.
	cs.SetOverride(ctx,
		"collector.interval_seconds", "120", 0, 99)
	t.Cleanup(func() {
		_ = cs.DeleteOverride(ctx,
			"collector.interval_seconds", 0)
		_ = cs.DeleteOverride(ctx,
			"collector.interval_seconds", 1)
	})

	merged, _ = cs.GetMergedConfig(ctx, cfg, 0)
	em = merged["collector.interval_seconds"].(map[string]any)
	if em["source"] != "override" {
		t.Errorf("source = %v, want override", em["source"])
	}

	// Per-DB override wins over global.
	cs.SetOverride(ctx,
		"collector.interval_seconds", "30", 1, 99)
	merged, _ = cs.GetMergedConfig(ctx, cfg, 1)
	em = merged["collector.interval_seconds"].(map[string]any)
	if em["source"] != "db_override" {
		t.Errorf("source = %v, want db_override", em["source"])
	}
}

func TestCoverage_ConfigStore_GetAuditLog(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)

	cs.SetOverride(ctx,
		"retention.snapshots_days", "60", 0, 99)
	cs.SetOverride(ctx,
		"retention.snapshots_days", "90", 0, 99)
	t.Cleanup(func() {
		_ = cs.DeleteOverride(ctx,
			"retention.snapshots_days", 0)
	})

	entries, err := cs.GetAuditLog(ctx, 10)
	if err != nil {
		t.Fatalf("GetAuditLog: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.Key == "retention.snapshots_days" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("expected >= 2 audit entries, got %d", count)
	}
}

func TestCoverage_ConfigStore_GetAuditLog_DefaultLimit(t *testing.T) {
	pool, ctx := coverageDB(t)
	cs := NewConfigStore(pool)

	// limit=0 should default to 100.
	_, err := cs.GetAuditLog(ctx, 0)
	if err != nil {
		t.Fatalf("GetAuditLog(0): %v", err)
	}

	// limit=-1 should also default.
	_, err = cs.GetAuditLog(ctx, -1)
	if err != nil {
		t.Fatalf("GetAuditLog(-1): %v", err)
	}

	// limit > 200 should default.
	_, err = cs.GetAuditLog(ctx, 300)
	if err != nil {
		t.Fatalf("GetAuditLog(300): %v", err)
	}
}

// ---------------------------------------------------------------------------
// DatabaseStore DB integration tests
// ---------------------------------------------------------------------------

func TestCoverage_DatabaseStore_CreateAndGet(t *testing.T) {
	pool, ctx := coverageDB(t)
	// Clean up databases table.
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)

	input := validInput()
	id, err := store.Create(ctx, input, 99)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id < 1 {
		t.Errorf("expected positive ID, got %d", id)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.databases")
	})

	rec, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Name != "prod-db" {
		t.Errorf("Name = %q, want prod-db", rec.Name)
	}
	if rec.Host != "db.example.com" {
		t.Errorf("Host = %q, want db.example.com", rec.Host)
	}
	if rec.Port != 5432 {
		t.Errorf("Port = %d, want 5432", rec.Port)
	}
	if rec.DatabaseName != "myapp" {
		t.Errorf("DatabaseName = %q, want myapp", rec.DatabaseName)
	}
	if rec.SSLMode != "require" {
		t.Errorf("SSLMode = %q, want require", rec.SSLMode)
	}
	if rec.Tags["env"] != "prod" {
		t.Errorf("Tags[env] = %q, want prod", rec.Tags["env"])
	}
	if rec.CreatedBy != 99 {
		t.Errorf("CreatedBy = %d, want 99", rec.CreatedBy)
	}
}

func TestCoverage_DatabaseStore_List(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.databases")
	})

	for i := range 3 {
		input := validInput()
		input.Name = fmt.Sprintf("cb-db-%d", i)
		if _, err := store.Create(ctx, input, 99); err != nil {
			t.Fatalf("Create db-%d: %v", i, err)
		}
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("List returned %d, want 3", len(list))
	}
}

func TestCoverage_DatabaseStore_Update(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.databases")
	})

	id, err := store.Create(ctx, validInput(), 99)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated := validInput()
	updated.Name = "staging-db"
	updated.Host = "staging.example.com"
	updated.Password = "new-password"

	if err := store.Update(ctx, id, updated); err != nil {
		t.Fatalf("Update with password: %v", err)
	}

	rec, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if rec.Name != "staging-db" {
		t.Errorf("Name = %q, want staging-db", rec.Name)
	}
	if rec.Host != "staging.example.com" {
		t.Errorf("Host = %q", rec.Host)
	}
}

func TestCoverage_DatabaseStore_UpdateWithoutPassword(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.databases")
	})

	id, err := store.Create(ctx, validInput(), 99)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated := validInput()
	updated.Name = "renamed-db"
	updated.Password = "" // no password change

	if err := store.Update(ctx, id, updated); err != nil {
		t.Fatalf("Update without password: %v", err)
	}

	// Password should be preserved.
	connStr, err := store.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("GetConnectionString: %v", err)
	}
	if !strings.Contains(connStr, "s3cret") {
		t.Error("password was lost after update without password")
	}
}

func TestCoverage_DatabaseStore_UpdateMissingReturnsNotFound(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)

	if err := store.Update(ctx, 999999, validInput()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing error = %v, want ErrNotFound", err)
	}
}

func TestCoverage_DatabaseStore_Delete(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)

	id, err := store.Create(ctx, validInput(), 99)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	count, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("Count = %d after delete, want 0", count)
	}
}

func TestCoverage_DatabaseStore_GetConnectionString(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.databases")
	})

	id, err := store.Create(ctx, validInput(), 99)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	connStr, err := store.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("GetConnectionString: %v", err)
	}
	want := "postgres://admin:s3cret@db.example.com:5432/myapp?sslmode=require"
	if connStr != want {
		t.Errorf("connStr = %q, want %q", connStr, want)
	}
}

func TestCoverage_DatabaseStore_Count(t *testing.T) {
	pool, ctx := coverageDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.databases")
	store := NewDatabaseStore(pool, cbKey)
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.databases")
	})

	count, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("Count = %d, want 0", count)
	}

	input := validInput()
	if _, err := store.Create(ctx, input, 99); err != nil {
		t.Fatalf("Create: %v", err)
	}

	count, err = store.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Errorf("Count = %d, want 1", count)
	}
}

// ---------------------------------------------------------------------------
// NotificationStore DB integration tests
// ---------------------------------------------------------------------------

func TestCoverage_NotificationStore_CreateAndGetChannel(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	name := fmt.Sprintf("cb-slack-%d", time.Now().UnixNano())
	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cb",
	}
	id, err := ns.CreateChannel(ctx, name, "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	ch, err := ns.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Name != name {
		t.Errorf("Name = %q, want %q", ch.Name, name)
	}
	if ch.Type != "slack" {
		t.Errorf("Type = %q, want slack", ch.Type)
	}
	if ch.Config["webhook_url"] != "https://hooks.slack.com/cb" {
		t.Errorf("Config[webhook_url] = %q",
			ch.Config["webhook_url"])
	}
}

func TestCoverage_NotificationStore_ListChannels(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cblist",
	}
	id, err := ns.CreateChannel(
		ctx, "cb-list", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	channels, err := ns.ListChannels(ctx)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	found := false
	for _, ch := range channels {
		if ch.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("channel %d not in list", id)
	}
}

func TestCoverage_NotificationStore_UpdateChannel(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cbupd",
	}
	id, err := ns.CreateChannel(
		ctx, "cb-update", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	newCfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/new",
	}
	err = ns.UpdateChannel(ctx, id, "cb-updated", newCfg, false)
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	ch, err := ns.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Name != "cb-updated" {
		t.Errorf("Name = %q, want cb-updated", ch.Name)
	}
	if ch.Enabled {
		t.Error("expected disabled, got enabled")
	}
}

func TestCoverage_NotificationStore_UpdateChannelPreservesMaskedSecrets(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	originalURL := "https://hooks.slack.com/services/REAL/SECRET/TOKEN"
	id, err := ns.CreateChannel(
		ctx, "cb-masked-update", "slack",
		map[string]string{"webhook_url": originalURL}, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	err = ns.UpdateChannel(ctx, id, "cb-masked-update",
		map[string]string{"webhook_url": "http****OKEN"}, false)
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	ch, err := ns.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Config["webhook_url"] != originalURL {
		t.Fatalf("webhook_url = %q, want original secret",
			ch.Config["webhook_url"])
	}
	if ch.Enabled {
		t.Fatal("enabled = true, want false")
	}
}

func TestCoverage_NotificationStore_DeleteChannel(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cbdel",
	}
	id, err := ns.CreateChannel(
		ctx, "cb-delete", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	err = ns.DeleteChannel(ctx, id)
	if err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}

	_, err = ns.GetChannel(ctx, id)
	if err == nil {
		t.Error("expected error getting deleted channel")
	}
}

func TestCoverage_NotificationStore_CreateRule(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cbrule",
	}
	chID, err := ns.CreateChannel(
		ctx, "cb-rule-ch", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	ruleID, err := ns.CreateRule(
		ctx, chID, "action_executed", "warning")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if ruleID <= 0 {
		t.Errorf("expected positive rule ID, got %d", ruleID)
	}
}

func TestCoverage_NotificationStore_CreateRule_InvalidEvent(
	t *testing.T,
) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	_, err := ns.CreateRule(ctx, 1, "bogus_event", "info")
	if err == nil {
		t.Error("expected error for invalid event type")
	}
}

func TestCoverage_NotificationStore_CreateRule_InvalidSeverity(
	t *testing.T,
) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	_, err := ns.CreateRule(
		ctx, 1, "action_executed", "extreme")
	if err == nil {
		t.Error("expected error for invalid severity")
	}
}

func TestCoverage_NotificationStore_ListRules(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cblr",
	}
	chID, err := ns.CreateChannel(
		ctx, "cb-listrule", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	_, err = ns.CreateRule(
		ctx, chID, "action_failed", "info")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}

	rules, err := ns.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) == 0 {
		t.Error("expected at least one rule")
	}
}

func TestCoverage_NotificationStore_DeleteRule(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cbdr",
	}
	chID, err := ns.CreateChannel(
		ctx, "cb-delrule", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	ruleID, err := ns.CreateRule(
		ctx, chID, "approval_needed", "critical")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}

	err = ns.DeleteRule(ctx, ruleID)
	if err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
}

func TestCoverage_NotificationStore_UpdateRule(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cbur",
	}
	chID, err := ns.CreateChannel(
		ctx, "cb-updrule", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	ruleID, err := ns.CreateRule(
		ctx, chID, "finding_critical", "info")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}

	// Disable the rule.
	err = ns.UpdateRule(ctx, ruleID, false)
	if err != nil {
		t.Fatalf("UpdateRule(false): %v", err)
	}

	// Re-enable.
	err = ns.UpdateRule(ctx, ruleID, true)
	if err != nil {
		t.Fatalf("UpdateRule(true): %v", err)
	}
}

func TestCoverage_NotificationStore_ListLog(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	entries, err := ns.ListLog(ctx, 10)
	if err != nil {
		t.Fatalf("ListLog: %v", err)
	}
	// Log may be empty; just verify no error.
	_ = entries
}

func TestCoverage_NotificationStore_ListLog_DefaultLimit(
	t *testing.T,
) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	// limit=0 should default to 100.
	_, err := ns.ListLog(ctx, 0)
	if err != nil {
		t.Fatalf("ListLog(0): %v", err)
	}

	// limit=-1 should also default.
	_, err = ns.ListLog(ctx, -1)
	if err != nil {
		t.Fatalf("ListLog(-1): %v", err)
	}

	// limit > 500 should default.
	_, err = ns.ListLog(ctx, 600)
	if err != nil {
		t.Fatalf("ListLog(600): %v", err)
	}
}

func TestCoverage_NotificationStore_TestChannel_NoDispatcher(
	t *testing.T,
) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil) // nil dispatcher

	cfg := map[string]string{
		"webhook_url": "https://hooks.slack.com/cbtc",
	}
	id, err := ns.CreateChannel(
		ctx, "cb-testch", "slack", cfg, 99)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	err = ns.TestChannel(ctx, id)
	if err == nil {
		t.Error("expected error with nil dispatcher")
	}
	if !strings.Contains(err.Error(), "dispatcher not configured") {
		t.Errorf("error = %q, want 'dispatcher not configured'",
			err.Error())
	}
}

func TestCoverage_NotificationStore_CreateChannel_InvalidType(
	t *testing.T,
) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	_, err := ns.CreateChannel(
		ctx, "bad", "sms", map[string]string{}, 99)
	if err == nil {
		t.Error("expected error for invalid channel type")
	}
}

func TestCoverage_NotificationStore_CreateChannel_MissingConfig(
	t *testing.T,
) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)

	_, err := ns.CreateChannel(
		ctx, "bad", "slack", map[string]string{}, 99)
	if err == nil {
		t.Error("expected error for missing webhook_url")
	}
}

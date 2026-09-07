package agentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const wave3ResolvedSecret = "wave3-plaintext-secret-must-not-persist"

type wave3MonitoringClaim struct {
	WorkID            string    `json:"work_id"`
	DeploymentID      string    `json:"deployment_id"`
	PhysicalTargetKey string    `json:"physical_target_key"`
	TenantID          string    `json:"tenant_id"`
	Provider          string    `json:"provider"`
	Tier              string    `json:"tier"`
	ClaimID           string    `json:"claim_id"`
	ClaimOwner        string    `json:"claim_owner"`
	ClaimExpiresAt    time.Time `json:"claim_expires_at"`
	SecretRef         string    `json:"secret_ref,omitempty"`
}

type wave3ScheduleResult struct {
	ScannedRows     int `json:"scanned_rows"`
	Enqueued        int `json:"enqueued"`
	PhysicalTargets int `json:"physical_targets"`
}

func TestWave3MonitoringSchemaContract(t *testing.T) {
	schema := strings.ToLower(strings.Join(strings.Fields(
		strings.Join(schemaStatements, "\n")), " "))
	required := []string{
		"monitoring_mode text not null default 'adaptive'",
		"execution_mode text not null default 'manual'",
		"wake_idle_allowed boolean not null default false",
		"sage.agent_db_monitoring_policies",
		"sage.agent_db_monitoring_state",
		"sage.agent_db_monitoring_work",
		"physical_target_key",
		"next_due_at",
		"claim_id",
		"claim_owner",
		"claim_expires_at",
		"idx_agent_db_monitoring_work_due",
	}
	for _, fragment := range required {
		if !strings.Contains(schema, fragment) {
			t.Errorf("AgentDB monitoring schema missing %q", fragment)
		}
	}
}

func TestWave3MonitoringDefaultsAreSafe(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_defaults_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	registerWave3Deployment(t, st, ctx, "wave3_defaults", tenant,
		ProviderLocalPostgres, "physical-default", nil)

	var mode, executionMode string
	var wakeIdle bool
	err := pool.QueryRow(ctx, `SELECT monitoring_mode, execution_mode,
		wake_idle_allowed FROM sage.agent_db_deployments
		WHERE deployment_id='wave3_defaults'`).Scan(
		&mode, &executionMode, &wakeIdle)
	if err != nil {
		t.Fatalf("read monitoring defaults: %v", err)
	}
	if mode != "adaptive" || executionMode != "manual" || wakeIdle {
		t.Fatalf("defaults = mode:%q execution:%q wake:%t; want adaptive/manual/false",
			mode, executionMode, wakeIdle)
	}
}

func TestWave3MonitoringClaimsAreDurableLeases(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_claim_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	registerWave3Deployment(t, st, ctx, "wave3_claim_due", tenant,
		ProviderAWSRDS, "physical-claim", nil)
	registerWave3Deployment(t, st, ctx, "wave3_claim_future", tenant,
		ProviderAWSRDS, "physical-future", nil)
	now := time.Now().UTC().Truncate(time.Second)
	insertWave3Work(t, ctx, pool, "work_due", "wave3_claim_due", tenant,
		ProviderAWSRDS, "physical-claim", "light_sql_probe", now)
	insertWave3Work(t, ctx, pool, "work_future", "wave3_claim_future", tenant,
		ProviderAWSRDS, "physical-future", "light_sql_probe", now.Add(time.Hour))

	first := mustClaimWave3(t, st, ctx, "worker-a", now, time.Minute, 10)
	if len(first) != 1 || first[0].WorkID != "work_due" {
		t.Fatalf("first claim = %#v, want only due work", first)
	}
	second := mustClaimWave3(t, st, ctx, "worker-b", now, time.Minute, 10)
	if len(second) != 0 {
		t.Fatalf("active lease was double-claimed: %#v", second)
	}
	reclaimed := mustClaimWave3(t, st, ctx, "worker-b",
		now.Add(2*time.Minute), time.Minute, 10)
	if len(reclaimed) != 1 || reclaimed[0].WorkID != "work_due" ||
		reclaimed[0].ClaimOwner != "worker-b" {
		t.Fatalf("expired lease was not durably reclaimed: %#v", reclaimed)
	}
}

func TestWave3MonitoringClaimsHonorConcurrencyLimits(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_limit_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	now := time.Now().UTC()
	seedWave3ConcurrencyFixtures(t, st, ctx, pool, tenant, now)

	start := make(chan struct{})
	results := make(chan wave3ClaimResult, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			claims, err := claimWave3(st, ctx, owner, now, time.Minute, 4)
			results <- wave3ClaimResult{claims: claims, err: err}
		}(owner)
	}
	close(start)
	wg.Wait()
	close(results)

	all := collectWave3Claims(t, results)
	assertWave3ClaimLimits(t, all)
}

func TestWave3IdleServerlessGateDoesNotBlockLifecycle(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_idle_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	idle := map[string]any{"serverless_state": "idle"}
	registerWave3Deployment(t, st, ctx, "wave3_idle_blocked", tenant,
		ProviderDatabricksLakebase, "physical-idle-blocked", idle)
	registerWave3Deployment(t, st, ctx, "wave3_idle_allowed", tenant,
		ProviderDatabricksLakebase, "physical-idle-allowed", idle)
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET wake_idle_allowed=true WHERE deployment_id='wave3_idle_allowed'`); err != nil {
		t.Fatalf("allow idle wake: %v", err)
	}
	now := time.Now().UTC()
	insertWave3Work(t, ctx, pool, "blocked_sql", "wave3_idle_blocked", tenant,
		ProviderDatabricksLakebase, "physical-idle-blocked", "light_sql_probe", now)
	insertWave3Work(t, ctx, pool, "blocked_lifecycle", "wave3_idle_blocked", tenant,
		ProviderDatabricksLakebase, "physical-idle-blocked", "lifecycle_provider", now)
	insertWave3Work(t, ctx, pool, "allowed_sql", "wave3_idle_allowed", tenant,
		ProviderDatabricksLakebase, "physical-idle-allowed", "light_sql_probe", now)

	claims := mustClaimWave3(t, st, ctx, "idle-gate-worker", now, time.Minute, 10)
	assertWave3WorkIDs(t, claims, []string{"allowed_sql", "blocked_lifecycle"})
}

func TestWave3TerminalDeploymentRevokesMonitoringWork(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_terminal_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	registerWave3Deployment(t, st, ctx, "wave3_terminal", tenant,
		ProviderLocalPostgres, "physical-terminal", nil)
	now := time.Now().UTC()
	insertWave3Work(t, ctx, pool, "terminal_work", "wave3_terminal", tenant,
		ProviderLocalPostgres, "physical-terminal", "deep_analysis", now)
	if _, err := st.RecordBackup(ctx, "wave3_terminal", BackupRequest{
		BackupID:          "wave3_terminal_backup",
		Provider:          ProviderLocalPostgres,
		Status:            "restore_verified",
		VerifiedAt:        now,
		RestoreVerifiedAt: now,
	}); err != nil {
		t.Fatalf("record restore-verified backup: %v", err)
	}
	if _, err := st.Archive(ctx, "wave3_terminal"); err != nil {
		t.Fatalf("archive deployment: %v", err)
	}
	if err := st.Delete(ctx, "wave3_terminal"); err != nil {
		t.Fatalf("delete deployment: %v", err)
	}

	var status string
	var revokedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT status, revoked_at
		FROM sage.agent_db_monitoring_work WHERE work_id='terminal_work'`).Scan(
		&status, &revokedAt)
	if err != nil {
		t.Fatalf("read terminal work: %v", err)
	}
	if status != "revoked" || revokedAt == nil {
		t.Fatalf("terminal work = status:%q revoked_at:%v", status, revokedAt)
	}
}

func TestWave3MonitoringClaimsNeverPersistResolvedSecrets(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	t.Setenv("WAVE3_MONITORING_SECRET", wave3ResolvedSecret)
	const tenant = "wave3_secret_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	registerWave3Deployment(t, st, ctx, "wave3_secret", tenant,
		ProviderAWSRDS, "physical-secret", map[string]any{
			"serverless_state": "active",
		})
	if _, err := pool.Exec(ctx, `UPDATE sage.agent_db_deployments
		SET secret_ref='env://WAVE3_MONITORING_SECRET'
		WHERE deployment_id='wave3_secret'`); err != nil {
		t.Fatalf("set secret reference: %v", err)
	}
	insertWave3Work(t, ctx, pool, "secret_work", "wave3_secret", tenant,
		ProviderAWSRDS, "physical-secret", "light_sql_probe", time.Now().UTC())

	claims := mustClaimWave3(t, st, ctx, "secret-worker", time.Now().UTC(),
		time.Minute, 1)
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	if strings.Contains(string(body), wave3ResolvedSecret) {
		t.Fatalf("durable claim leaked resolved secret: %s", body)
	}
	assertWave3WorkTableHasNoSecretColumn(t, ctx, pool)
}

func TestWave3TenThousandSchemaRowsScheduleByPhysicalTarget(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_10k_tenant"
	cleanupWave3Monitoring(t, ctx, pool, tenant)
	seedWave3TenThousandDeployments(t, ctx, pool, tenant)

	beforeGoroutines := runtime.NumGoroutine()
	beforeConnections := pool.Stat().TotalConns()
	result := mustScheduleWave3(t, st, ctx, time.Now().UTC(), 10_000)
	afterGoroutines := runtime.NumGoroutine()
	afterConnections := pool.Stat().TotalConns()
	if result.ScannedRows != 10_000 || result.PhysicalTargets != 100 ||
		result.Enqueued > 100 {
		t.Fatalf("schedule result = %#v, want 10k rows grouped to <=100 jobs", result)
	}
	if delta := afterGoroutines - beforeGoroutines; delta > 8 {
		t.Fatalf("scheduler retained %d goroutines for catalog rows", delta)
	}
	if delta := afterConnections - beforeConnections; delta > 2 {
		t.Fatalf("scheduler retained %d connections for catalog rows", delta)
	}
	assertWave3PhysicalWorkGrouping(t, ctx, pool, tenant)
}

type wave3ClaimResult struct {
	claims []wave3MonitoringClaim
	err    error
}

func claimWave3(st *Store, ctx context.Context, owner string, now time.Time,
	lease time.Duration, limit int) ([]wave3MonitoringClaim, error) {
	raw, err := invokeWave3StoreMethod(st, "ClaimMonitoringWork",
		ctx, owner, now, lease, limit)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var claims []wave3MonitoringClaim
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func mustClaimWave3(t *testing.T, st *Store, ctx context.Context, owner string,
	now time.Time, lease time.Duration, limit int) []wave3MonitoringClaim {
	t.Helper()
	claims, err := claimWave3(st, ctx, owner, now, lease, limit)
	if err != nil {
		t.Fatalf("ClaimMonitoringWork: %v", err)
	}
	return claims
}

func mustScheduleWave3(t *testing.T, st *Store, ctx context.Context,
	now time.Time, scanLimit int) wave3ScheduleResult {
	t.Helper()
	raw, err := invokeWave3StoreMethod(st, "ScheduleMonitoring",
		ctx, now, scanLimit)
	if err != nil {
		t.Fatalf("ScheduleMonitoring: %v", err)
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal schedule result: %v", err)
	}
	var result wave3ScheduleResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode schedule result: %v", err)
	}
	return result
}

func invokeWave3StoreMethod(st *Store, name string, args ...any) (any, error) {
	method := reflect.ValueOf(st).MethodByName(name)
	if !method.IsValid() {
		return nil, fmt.Errorf("minimal R13 seam missing: Store.%s", name)
	}
	typ := method.Type()
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if typ.NumIn() != len(args) || typ.NumOut() != 2 ||
		!typ.Out(1).Implements(errorType) {
		return nil, fmt.Errorf("Store.%s has incompatible signature %s", name, typ)
	}
	inputs := make([]reflect.Value, len(args))
	for i, arg := range args {
		inputs[i] = reflect.ValueOf(arg)
		if !inputs[i].Type().AssignableTo(typ.In(i)) {
			return nil, fmt.Errorf("Store.%s argument %d is %s, want %s",
				name, i, inputs[i].Type(), typ.In(i))
		}
	}
	outputs := method.Call(inputs)
	if !outputs[1].IsNil() {
		return nil, outputs[1].Interface().(error)
	}
	return outputs[0].Interface(), nil
}

func registerWave3Deployment(t *testing.T, st *Store, ctx context.Context,
	id, tenant, provider, resource string, metadata map[string]any) {
	t.Helper()
	level := LevelSchema
	if cloudProvider(provider) {
		level = LevelInstance
	}
	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID:       id,
		TenantID:           tenant,
		AgentID:            "agent-" + id,
		DatabaseName:       "app",
		IsolationType:      level,
		SchemaName:         "schema_" + strings.ReplaceAll(id, "-", "_"),
		Provider:           provider,
		ProvisioningLevel:  level,
		ProvisioningStatus: "available",
		ProviderResourceID: resource,
		LeaseSeconds:       3600,
		BackupRequired:     false,
		Metadata:           metadata,
	})
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func insertWave3Work(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	workID, deploymentID, tenant, provider, target, tier string, due time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.agent_db_monitoring_work
		(work_id, deployment_id, tenant_id, provider, physical_target_key,
		 tier, status, next_due_at)
		VALUES ($1,$2,$3,$4,$5,$6,'queued',$7)`, workID, deploymentID,
		tenant, provider, target, tier, due)
	if err != nil {
		t.Fatalf("insert monitoring work %s: %v", workID, err)
	}
}

func cleanupWave3Monitoring(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, tenant string) {
	t.Helper()
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_monitoring_work
			WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_monitoring_policies
			WHERE scope_id LIKE 'wave3_%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_monitoring_state
			WHERE physical_target_key LIKE 'physical-%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_deployments
			WHERE tenant_id=$1`, tenant)
	}
	cleanup()
	t.Cleanup(cleanup)
}

func seedWave3ConcurrencyFixtures(t *testing.T, st *Store, ctx context.Context,
	pool *pgxpool.Pool, tenant string, now time.Time) {
	t.Helper()
	providers := []string{ProviderAWSRDS, ProviderAWSRDS, ProviderAWSRDS,
		ProviderAWSRDS, ProviderGCPCloudSQL, ProviderGCPCloudSQL}
	for i, provider := range providers {
		id := fmt.Sprintf("wave3_limit_%d", i)
		registerWave3Deployment(t, st, ctx, id, tenant, provider,
			fmt.Sprintf("physical-limit-%d", i), nil)
		insertWave3Work(t, ctx, pool, "work_"+id, id, tenant, provider,
			fmt.Sprintf("physical-limit-%d", i), "light_sql_probe", now)
	}
	statements := []struct {
		scopeType string
		scopeID   string
		limit     int
	}{
		{"provider", ProviderAWSRDS, 2},
		{"tenant", tenant, 3},
		{"deployment", "wave3_limit_0", 1},
	}
	for _, item := range statements {
		upsertWave3MonitoringPolicy(
			t, ctx, pool, item.scopeType, item.scopeID, item.limit,
		)
	}
}

func collectWave3Claims(t *testing.T,
	results <-chan wave3ClaimResult) []wave3MonitoringClaim {
	t.Helper()
	all := []wave3MonitoringClaim{}
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent ClaimMonitoringWork: %v", result.err)
		}
		all = append(all, result.claims...)
	}
	return all
}

func assertWave3ClaimLimits(t *testing.T, claims []wave3MonitoringClaim) {
	t.Helper()
	if len(claims) != 3 {
		t.Fatalf("combined claims = %d, want tenant limit 3: %#v", len(claims), claims)
	}
	seen := map[string]bool{}
	providerCounts := map[string]int{}
	tenantCounts := map[string]int{}
	deploymentCounts := map[string]int{}
	for _, claim := range claims {
		if seen[claim.WorkID] {
			t.Fatalf("work %q claimed by concurrent workers twice", claim.WorkID)
		}
		seen[claim.WorkID] = true
		providerCounts[claim.Provider]++
		tenantCounts[claim.TenantID]++
		deploymentCounts[claim.DeploymentID]++
	}
	if providerCounts[ProviderAWSRDS] > 2 ||
		tenantCounts["wave3_limit_tenant"] > 3 ||
		deploymentCounts["wave3_limit_0"] > 1 {
		t.Fatalf("claims exceeded scope limits: %#v", claims)
	}
}

func assertWave3WorkIDs(t *testing.T, claims []wave3MonitoringClaim, want []string) {
	t.Helper()
	got := map[string]bool{}
	for _, claim := range claims {
		got[claim.WorkID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("claimed work IDs = %v, want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("claimed work IDs = %v, missing %q", got, id)
		}
	}
}

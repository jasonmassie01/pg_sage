package agentdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// D4: approvals are the evidence that earns cloud spend, so every human
// decision is attributed and every approved request is used exactly once.

const d4Tenant = "tenant_d4_store"

func resetD4Request(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE request_id=$1", id)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_deployments
			WHERE metadata->>'request_id'=$1`, id)
	}
	clean()
	t.Cleanup(clean)
}

// seedD4CloudRequest creates an aws_rds instance request; a positive budget
// makes request policy approve it at creation.
func seedD4CloudRequest(
	t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool, id string, budget float64,
) Request {
	t.Helper()
	resetD4Request(t, ctx, pool, id)
	req, err := st.CreateRequest(ctx, RequestCreate{RequestID: id, TenantID: d4Tenant,
		AgentID: "agent_d4", IsolationType: LevelInstance, Provider: ProviderAWSRDS,
		DatabaseName: "d4_app", BudgetUSD: budget, BackupRequired: true})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	return req
}

func seedD4Profile(t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	id := "d4_aws_custom_profile"
	if _, err := st.UpsertSizeProfile(ctx, SizeProfile{ProfileID: id,
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, Name: id,
		StorageGB: 30, MonthlyBudgetUSD: 40,
		ProviderParams: map[string]any{"db_instance_class": "db.t4g.small"}}); err != nil {
		t.Fatalf("UpsertSizeProfile: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", id)
	})
	return id
}

func TestSetRequestDecisionRecordsActor(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_actor_approve", 0)
	got, err := st.SetRequestDecision(ctx, "req_d4_actor_approve",
		DecisionRequest{Decision: "approved", Reason: "ok", ActorID: "op@x.test"})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Status != "approved" || got.DecidedBy != "op@x.test" || got.DecidedAt == nil {
		t.Fatalf("approval not attributed: status=%s by=%q at=%v",
			got.Status, got.DecidedBy, got.DecidedAt)
	}
	var column string
	if err := pool.QueryRow(ctx, `SELECT decided_by FROM sage.agent_db_requests
		WHERE request_id='req_d4_actor_approve'`).Scan(&column); err != nil {
		t.Fatalf("read decided_by: %v", err)
	}
	if column != "op@x.test" {
		t.Fatalf("decided_by column = %q", column)
	}
}

func TestSetRequestDecisionDenyRecordsActor(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_actor_deny", 0)
	got, err := st.SetRequestDecision(ctx, "req_d4_actor_deny",
		DecisionRequest{Decision: "denied", ActorID: "lead@x.test"})
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if got.Status != "denied" || got.DecidedBy != "lead@x.test" || got.DecidedAt == nil {
		t.Fatalf("denial not attributed: %+v", got)
	}
}

func TestSetRequestDecisionRequiresActor(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_no_actor", 0)
	for _, decision := range []string{"approved", "denied"} {
		_, err := st.SetRequestDecision(ctx, "req_d4_no_actor",
			DecisionRequest{Decision: decision, ActorID: "  "})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s without actor err = %v, want ErrInvalid", decision, err)
		}
	}
	got, err := st.GetRequest(ctx, "req_d4_no_actor")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "requested" || got.DecidedBy != "" {
		t.Fatalf("unattributed decision changed request: %+v", got)
	}
}

func TestPolicyDecisionAtCreationIsAttributedToPolicy(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	got := seedD4CloudRequest(t, st, ctx, pool, "req_d4_policy_auto", 25)
	if got.Status != "approved" || got.DecidedBy != "policy" || got.DecidedAt == nil {
		t.Fatalf("policy approval not attributed: %+v", got)
	}
	pending := seedD4CloudRequest(t, st, ctx, pool, "req_d4_policy_review", 0)
	if pending.Status != "requested" || pending.DecidedBy != "" || pending.DecidedAt != nil {
		t.Fatalf("pending request carries a decision: %+v", pending)
	}
}

func TestProvisionApprovedRequestCarriesSizeProfileAndSecretRef(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	profile := seedD4Profile(t, st, ctx, pool)
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_fields", 25)
	dep, err := st.ProvisionApprovedRequest(ctx, "req_d4_fields", RequestProvisionRequest{
		DeploymentID: "dep_d4_fields", SizeProfileID: profile,
		SecretRef: "env:PG_SAGE_AGENTDB_D4_DSN", SecretRefProvider: "env",
		ActorID: "op@x.test",
	})
	if err != nil {
		t.Fatalf("ProvisionApprovedRequest: %v", err)
	}
	if dep.SizeProfileID != profile {
		t.Fatalf("size_profile_id = %q, want %q", dep.SizeProfileID, profile)
	}
	if dep.SecretRef != "env:PG_SAGE_AGENTDB_D4_DSN" || dep.SecretRefProvider != "env" {
		t.Fatalf("secret ref dropped: %q/%q", dep.SecretRef, dep.SecretRefProvider)
	}
}

func TestProvisionApprovedRequestCarriesSchemaName(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "req_d4_schema"
	resetD4Request(t, ctx, pool, id)
	cleanupDeployment(t, ctx, pool, "dep_d4_schema")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS d4_named_schema CASCADE`)
	})
	if _, err := st.CreateRequest(ctx, RequestCreate{RequestID: id, TenantID: d4Tenant,
		AgentID: "agent_d4", IsolationType: LevelSchema}); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	dep, err := st.ProvisionApprovedRequest(ctx, id, RequestProvisionRequest{
		DeploymentID: "dep_d4_schema", SchemaName: "d4_named_schema", ActorID: "op@x.test"})
	if err != nil {
		t.Fatalf("ProvisionApprovedRequest: %v", err)
	}
	if dep.SchemaName != "d4_named_schema" {
		t.Fatalf("schema_name = %q, want d4_named_schema", dep.SchemaName)
	}
}

func TestProvisionApprovedRequestLinksAndConsumes(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_link", 25)
	dep, err := st.ProvisionApprovedRequest(ctx, "req_d4_link",
		RequestProvisionRequest{DeploymentID: "dep_d4_link", ActorID: "op@x.test"})
	if err != nil {
		t.Fatalf("ProvisionApprovedRequest: %v", err)
	}
	if dep.Metadata["request_id"] != "req_d4_link" {
		t.Fatalf("deployment not linked to request: %#v", dep.Metadata)
	}
	got, err := st.GetRequest(ctx, "req_d4_link")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsumedDeploymentID != "dep_d4_link" || got.ConsumedBy != "op@x.test" ||
		got.ConsumedAt == nil {
		t.Fatalf("request not consumed/linked: %+v", got)
	}
}

func TestProvisionApprovedRequestReuseRejected(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_reuse", 25)
	first := RequestProvisionRequest{DeploymentID: "dep_d4_reuse_1", ActorID: "op@x.test"}
	if _, err := st.ProvisionApprovedRequest(ctx, "req_d4_reuse", first); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	for _, depID := range []string{"dep_d4_reuse_1", "dep_d4_reuse_2", ""} {
		_, err := st.ProvisionApprovedRequest(ctx, "req_d4_reuse",
			RequestProvisionRequest{DeploymentID: depID, ActorID: "op@x.test"})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("reuse with deployment %q err = %v, want ErrConflict", depID, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.agent_db_deployments
		WHERE metadata->>'request_id'='req_d4_reuse'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deployments produced by one approval = %d, want 1", n)
	}
}

func TestProvisionUnapprovedRequestRejected(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_review", 0)
	_, err := st.ProvisionApprovedRequest(ctx, "req_d4_review",
		RequestProvisionRequest{DeploymentID: "dep_d4_review", ActorID: "op@x.test"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("review request provision err = %v, want ErrInvalid", err)
	}
	if _, err := st.SetRequestDecision(ctx, "req_d4_review",
		DecisionRequest{Decision: "denied", ActorID: "op@x.test"}); err != nil {
		t.Fatalf("deny: %v", err)
	}
	_, err = st.ProvisionApprovedRequest(ctx, "req_d4_review",
		RequestProvisionRequest{DeploymentID: "dep_d4_review", ActorID: "op@x.test"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("denied request provision err = %v, want ErrInvalid", err)
	}
	if _, err := st.Get(ctx, "dep_d4_review"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unapproved request produced a deployment: %v", err)
	}
}

func TestProvisionUnknownRequestNotFound(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	_, err := st.ProvisionApprovedRequest(ctx, "req_d4_missing",
		RequestProvisionRequest{ActorID: "op@x.test"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request err = %v, want ErrNotFound", err)
	}
}

func TestProvisionRequestForAnotherTenantOrAgentRejected(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_scope", 25)
	cases := []RequestProvisionRequest{
		{TenantID: "tenant_other"},
		{AgentID: "agent_other"},
		{Provider: ProviderGCPCloudSQL},
		{ProvisioningLevel: LevelSchema},
	}
	for _, c := range cases {
		c.DeploymentID, c.ActorID = "dep_d4_scope", "op@x.test"
		if _, err := st.ProvisionApprovedRequest(ctx, "req_d4_scope", c); !errors.Is(err,
			ErrConflict) {
			t.Fatalf("mismatched provision %+v err = %v, want ErrConflict", c, err)
		}
	}
	got, err := st.GetRequest(ctx, "req_d4_scope")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsumedDeploymentID != "" {
		t.Fatalf("mismatched provision consumed the request: %+v", got)
	}
	matching := RequestProvisionRequest{DeploymentID: "dep_d4_scope", TenantID: d4Tenant,
		AgentID: "agent_d4", Provider: ProviderAWSRDS, ActorID: "op@x.test"}
	if _, err := st.ProvisionApprovedRequest(ctx, "req_d4_scope", matching); err != nil {
		t.Fatalf("matching provision: %v", err)
	}
}

func TestProvisionFailureLeavesRequestUnconsumed(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_release", 25)
	_, err := st.ProvisionApprovedRequest(ctx, "req_d4_release", RequestProvisionRequest{
		DeploymentID: "dep_d4_release", SizeProfileID: "d4_profile_missing",
		ActorID: "op@x.test"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing profile err = %v, want ErrNotFound", err)
	}
	got, err := st.GetRequest(ctx, "req_d4_release")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsumedDeploymentID != "" || got.ConsumedAt != nil {
		t.Fatalf("failed provision consumed the approval: %+v", got)
	}
	if _, err := st.ProvisionApprovedRequest(ctx, "req_d4_release", RequestProvisionRequest{
		DeploymentID: "dep_d4_release", ActorID: "op@x.test"}); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestDecisionAfterConsumptionRejected(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedD4CloudRequest(t, st, ctx, pool, "req_d4_final", 25)
	if _, err := st.ProvisionApprovedRequest(ctx, "req_d4_final", RequestProvisionRequest{
		DeploymentID: "dep_d4_final", ActorID: "op@x.test"}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	_, err := st.SetRequestDecision(ctx, "req_d4_final",
		DecisionRequest{Decision: "denied", ActorID: "op@x.test"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("deny after consumption err = %v, want ErrConflict", err)
	}
}

// Racers either replay one deployment id or each bring their own; in both
// shapes exactly one provision may consume the approval.
func TestProvisionApprovedRequestConcurrentExactlyOne(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	for _, shared := range []bool{true, false} {
		requestID := fmt.Sprintf("req_d4_race_shared_%v", shared)
		seedD4CloudRequest(t, st, ctx, pool, requestID, 25)
		wins, errs := raceProvision(st, ctx, requestID, shared)
		for _, err := range errs {
			if !errors.Is(err, ErrConflict) {
				t.Errorf("shared=%v losing racer err = %v, want ErrConflict", shared, err)
			}
		}
		if wins != 1 {
			t.Fatalf("shared=%v successful provisions = %d, want exactly 1", shared, wins)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.agent_db_deployments
			WHERE metadata->>'request_id'=$1`, requestID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("shared=%v deployments produced = %d, want 1", shared, n)
		}
	}
}

func raceProvision(st *Store, ctx context.Context, requestID string, shared bool) (int, []error) {
	const racers = 8
	var wg sync.WaitGroup
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		depID := requestID + "_dep"
		if !shared {
			depID = fmt.Sprintf("%s_dep_%d", requestID, i)
		}
		wg.Add(1)
		go func(depID string) {
			defer wg.Done()
			_, err := st.ProvisionApprovedRequest(ctx, requestID,
				RequestProvisionRequest{DeploymentID: depID, ActorID: "op@x.test"})
			results <- err
		}(depID)
	}
	wg.Wait()
	close(results)
	wins, errs := 0, []error{}
	for err := range results {
		if err == nil {
			wins++
		} else {
			errs = append(errs, err)
		}
	}
	return wins, errs
}

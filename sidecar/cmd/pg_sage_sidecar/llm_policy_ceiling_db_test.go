//go:build integration

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// llm-03 on real PostgreSQL: a ratified policy with llm_tokens_daily: 100
// caps the per-database client below its 500k config budget.

func policyCeilingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func ratifyTokenCap(t *testing.T, store *policy.Store, id, tokens int64) {
	t.Helper()
	ctx := context.Background()
	scope := policy.Scope{DatabaseID: &id}
	current, err := store.Bootstrap(ctx, scope, "unattended", "bootstrap")
	if err != nil {
		t.Fatalf("bootstrap policy: %v", err)
	}
	doc := policy.UnattendedProfile()
	doc.Budgets.LLMTokensDaily = policy.NewBudgetLimit(tokens)
	raw, err := policy.MarshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := store.Propose(ctx, policy.ProposalRequest{
		Scope: scope, ExpectedVersion: current.Version, Profile: "unattended",
		Document: raw, Actor: "operator@test",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := store.Ratify(ctx, policy.RatifyRequest{
		ProposalID: proposal.ID, ExpectedVersion: current.Version,
		Actor: "operator@test",
	}); err != nil {
		t.Fatalf("ratify: %v", err)
	}
}

func policyCappedClient(t *testing.T, pool *pgxpool.Pool, id int) *llm.Client {
	t.Helper()
	client := llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: fakeLLMProvider(t, 10).URL, APIKey: "test-key",
		Model: "test-model", TimeoutSeconds: 5, TokenBudgetDaily: 500000,
	}, func(string, string, ...any) {})
	rt := &databaseRuntime{spec: databaseRuntimeSpec{
		Name: "orders", DatabaseID: id, Pool: pool,
	}}
	rt.attachPolicyCeiling(client)
	return client
}

func TestPolicyCeilingDB_RatifiedPolicyBlocksPerDatabaseClient(t *testing.T) {
	pool := policyCeilingPool(t)
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM sage.policy WHERE database_id IN (9301, 9302)"); err != nil {
		t.Fatal(err)
	}
	store := policy.NewStore(pool)
	ratifyTokenCap(t, store, 9301, 100)
	if _, err := store.Bootstrap(context.Background(),
		policy.Scope{DatabaseID: int64Ptr(9302)}, "unattended", "bootstrap"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	capped := policyCappedClient(t, pool, 9301)
	_, _, err := capped.Chat(ctx, "sys", "needs 101", 101)
	if !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("101 under a ratified 100 cap = %v, want ErrBudgetExhausted", err)
	}
	if _, _, err := capped.Chat(ctx, "sys", "needs 100", 100); err != nil {
		t.Fatalf("100 under a ratified 100 cap: %v", err)
	}

	// Another database keeps the profile's 500k cap.
	other := policyCappedClient(t, pool, 9302)
	if _, _, err := other.Chat(ctx, "sys", "needs 101", 101); err != nil {
		t.Fatalf("101 on a database without the narrowed policy: %v", err)
	}
	if got := other.EffectiveTokenBudgetDaily(); got != 500000 {
		t.Fatalf("other database effective budget = %d, want 500000", got)
	}
}

func int64Ptr(v int64) *int64 { return &v }

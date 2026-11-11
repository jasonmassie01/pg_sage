package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-01 wiring: each database's ledger compares a measurement with the
// model that database's own LLM client runs, both when an investigation
// asks and when the hourly reconcile or the model-lift view reads it.

func TestLLMModelSource_FollowsTheClient(t *testing.T) {
	if got := llmModelSource(nil)(); got != "" {
		t.Fatalf("no client: %q, want no model", got)
	}
	client := llm.New(&config.LLMConfig{Enabled: true, Model: "gpt-6"}, nil)
	src := llmModelSource(client)
	if got := src(); got != "gpt-6" {
		t.Fatalf("model = %q, want gpt-6", got)
	}
	client.Reconfigure(&config.LLMConfig{Enabled: true, Model: "llama3"})
	if got := src(); got != "llama3" {
		t.Fatalf("after a reload: %q, want llama3", got)
	}
}

func TestRegistryRootAuthority_AnotherModelIsAdvisory(t *testing.T) {
	pool := autonomyPool(t)
	a := registryRootAuthority{registry: ledgerWithLift(t, pool, 16, 16), database: "orders"}
	got, err := a.ModelRootAuthority(context.Background(), "lock_blocking", "llama3")
	if err != nil || got.Granted || !strings.Contains(got.Reason,
		`measured with "gpt-4o-mini"; the investigator runs "llama3"`) {
		t.Fatalf("authority = %+v (%v), want advisory naming both models", got, err)
	}
	if got, err = a.ModelRootAuthority(context.Background(), "lock_blocking",
		"OpenAI/GPT-4o-mini"); err != nil || !got.Granted {
		t.Fatalf("the measured model under its provider prefix: %+v (%v)", got, err)
	}
}

func TestInstallGivesTheLedgerTheRunningModel(t *testing.T) {
	asRelease(t) // the signed lift names this build
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	db := uniqueDatabase("model")
	binding := autonomyBinding{database: db, control: pool, monitored: pool,
		settings:     config.DefaultConfig().SRE.Autonomy,
		runningModel: func() string { return "llama3" }}
	if err := ledgers.install(context.Background(), autonomousExecutor(pool),
		binding); err != nil {
		t.Fatal(err)
	}
	entry, ok := ledgers.registry.Lookup(db)
	if !ok {
		t.Fatal("not registered")
	}
	ingestSignedLift(t, entry.Service, time.Now().Add(-time.Hour), "lock_blocking", 16, 16)
	v, err := entry.Service.ModelLiftView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Families {
		if f.Family != earned.FamilyLockBlocking {
			continue
		}
		if f.Granted || f.RunningModel != "llama3" || f.MeasuredModel != "gpt-4o-mini" {
			t.Fatalf("lock_blocking = %+v, want advisory for llama3", f)
		}
		return
	}
	t.Fatal("lock_blocking missing from the view")
}

// liftModel is the model ledgerWithLift's report measured.
const liftModel = "gpt-4o-mini"

// liftModelClient is testLLMClient at endpoint running model.
func liftModelClient(endpoint, model string) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: endpoint, APIKey: "k",
		Model: model, TimeoutSeconds: 5, TokenBudgetDaily: 500000},
		func(string, string, ...any) {})
}

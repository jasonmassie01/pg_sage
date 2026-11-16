package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// evid-02c review: get_workload_pack's backend answers a stored pack's
// model tier only, and an unknown database or pack as not found.
// No concurrency test here: the backend holds no state of its own.

func TestWorkloadPackMCP_UnconnectedDatabaseIsNotFound(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for _, b := range []workloadPackMCPBackend{{}, {manager: fleet.NewManager(
		&config.Config{Mode: "fleet"})}} {
		got, err := b.WorkloadPackModel(context.Background(),
			mcp.WorkloadPackRequest{Database: "nope", ContentSHA256: sha})
		if !errors.Is(err, mcp.ErrWorkloadPackNotFound) || got != nil ||
			!strings.Contains(err.Error(), `"nope"`) {
			t.Fatalf("unconnected database: (%v, %v), want ErrWorkloadPackNotFound naming it",
				got, err)
		}
	}
}

func TestWorkloadPseudonymKey_NoControlDatabaseIsNil(t *testing.T) {
	if key := workloadPseudonymKey(nil); key != nil {
		t.Fatalf("no control database gave a pseudonym key of %d bytes", len(key))
	}
}

func TestWorkloadPackMCPDB_ServesTheModelTierOnly(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	pack, err := workpack.Build(ctx, pool, workpack.Request{Database: "live",
		Window:   workpack.Window{From: now.Add(-21 * 24 * time.Hour), To: now},
		Location: time.UTC, Now: now})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := workpack.Save(ctx, pool, pack, now, "test"); err != nil {
		t.Fatalf("save: %v", err)
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "live", Pool: pool,
		Config: config.DatabaseConfig{Name: "live"}, Status: &fleet.InstanceStatus{}})
	b := workloadPackMCPBackend{manager: mgr}
	got, err := b.WorkloadPackModel(ctx, mcp.WorkloadPackRequest{Database: "live",
		ContentSHA256: pack.ContentSHA256})
	if err != nil {
		t.Fatalf("model tier: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Database  string            `json:"database"`
		ModelTier workpack.TierView `json:"model_tier"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	v := out.ModelTier
	if out.Database != "live" || v.Tier != workpack.TierModel ||
		v.ContentSHA256 == pack.ContentSHA256 || len(v.ContentSHA256) != 64 ||
		v.Sections["tier.json"] == nil || v.Bytes == 0 ||
		strings.Contains(string(raw), `"pseudonyms"`) {
		t.Fatalf("get_workload_pack = %s", raw)
	}
	_, err = b.WorkloadPackModel(ctx, mcp.WorkloadPackRequest{Database: "live",
		ContentSHA256: strings.Repeat("0", 64)})
	if !errors.Is(err, mcp.ErrWorkloadPackNotFound) {
		t.Fatalf("unknown pack: %v, want ErrWorkloadPackNotFound", err)
	}
}

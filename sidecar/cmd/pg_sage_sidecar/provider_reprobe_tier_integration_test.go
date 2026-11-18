package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01d review: a member moved to observe-only while its runtime
// still runs is never rebuilt by a provider re-probe: an observe-only
// member gets no runtime, so no executor can come back for it.
func TestProviderReprobeSkipsObserveOnlyMetaRecord(t *testing.T) {
	env := newMetaReconcileEnv(t)
	builds := stubFirstProviderDetection(t, "meta_md")
	providerReprobeMin = time.Hour // the test calls the rebuild itself
	rec := env.record(t, "md")
	env.reconcile(t)
	first := builds.waitFor(t, 1)[0]
	if _, err := env.state.Pool.Exec(context.Background(),
		"UPDATE sage.databases SET tier = $1 WHERE id = $2", store.TierObserveOnly,
		rec.ID); err != nil {
		t.Fatalf("set tier: %v", err)
	}
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Scope: "meta-db", Name: "md",
		DatabaseID: rec.ID, Parent: context.Background()}, inst: first.inst}
	err := rt.rebuildForProvider(providerSelfManaged)
	if !errors.Is(err, fleet.ErrInstanceConflict) || !strings.Contains(err.Error(),
		"observe-only") {
		t.Fatalf("rebuild of an observe-only record = %v, want an observe-only conflict", err)
	}
	if fleetMgr.GetInstance("md") != first.inst || builds.count() != 1 {
		t.Fatalf("observe-only record was rebuilt (builds=%d)", builds.count())
	}
}

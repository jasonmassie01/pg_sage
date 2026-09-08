//go:build providerlive

package providerobs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autoexplain"
)

// Explicit build tag and owned project ref are required; this test performs only API reads.
func TestLiveSupabaseObservability(t *testing.T) {
	ref := os.Getenv("SAGE_TEST_SUPABASE_PROJECT_REF")
	if ref == "" || ref != os.Getenv("SAGE_TEST_SUPABASE_EXPECTED_PROJECT_REF") {
		t.Fatal("live test requires matching designated and expected disposable project refs")
	}
	c, err := NewSupabase(ref, os.Getenv("SAGE_TEST_SUPABASE_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := c.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ParseMetrics(body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.IdleCPUSeconds) == 0 || snapshot.MemoryTotalBytes == nil {
		t.Fatal("live API lacks CPU core counters or memory byte capacity")
	}
	now := time.Now().UTC()
	entries, err := c.Logs(ctx, "postgres", now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("live API returned no postgres logs from synthetic project")
	}
	for _, entry := range entries {
		if entry.Database != "postgres" || entry.Timestamp.IsZero() || entry.Message == "" {
			t.Fatal("live log did not preserve database/time/message")
		}
	}
	t.Logf("CHECK-OBS-01: PASS live metrics: %d CPU cores, memory capacity present",
		len(snapshot.IdleCPUSeconds))
	t.Logf("CHECK-OBS-02: PASS %d database-isolated log entries", len(entries))
}

func TestLiveSupabaseObservedPlan(t *testing.T) {
	ref := os.Getenv("SAGE_TEST_SUPABASE_PROJECT_REF")
	if ref == "" || ref != os.Getenv("SAGE_TEST_SUPABASE_EXPECTED_PROJECT_REF") {
		t.Fatal("live test requires matching disposable project identity")
	}
	c, err := NewSupabase(ref, os.Getenv("SAGE_TEST_SUPABASE_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entries, err := c.Logs(context.Background(), "postgres", now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.Contains(entry.Message, "pg_sage_provider_obs_synthetic") {
			continue
		}
		plan, err := autoexplain.ParseObservedPlan(entry, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		if plan.QueryID == 0 || plan.ExecutionMS <= 0 || plan.TotalCost <= 0 {
			t.Fatal("observed plan lacks query identity or actual duration and cost")
		}
		t.Log("CHECK-OBS-03: PASS genuine auto_explain JSON, query identity, execution timing")
		return
	}
	t.Fatal("synthetic auto_explain event absent from live logs")
}

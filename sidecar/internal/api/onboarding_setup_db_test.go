package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (ux-04b): the fleet reader records the wizard's autonomy
// choice in the database's sage.onboarding row, the checklist then shows
// Grant more done, and a second record leaves the first time.
func TestFleetOnboardingRecordsSetupStep(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "api_ob_setup")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := onboarding.Init(ctx, pool, "live"); err != nil {
		t.Fatalf("init: %v", err)
	}
	reader := fleetReader(t, &fleet.DatabaseInstance{Name: "live", Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true, TrustLevel: "observation"}})
	first := time.Now().UTC().Truncate(time.Second)
	h := onboardingSetupHandler(reader, func() time.Time { return first })
	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup",
			strings.NewReader(`{"database":"live","step":"autonomy"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		first = first.Add(time.Hour) // the second request's clock
	}
	rec, body := obGet(t, onboardingHandler(reader), "/api/v1/onboarding?database=live")
	if rec.Code != http.StatusOK {
		t.Fatalf("onboarding status = %d", rec.Code)
	}
	if g := entrySteps(t, body)[onboarding.StepGrantMore]; g["done"] != true {
		t.Fatalf("grant step = %v, want done after the autonomy choice", g)
	}
	setup := entryNamed(t, body, "live")["setup"].(map[string]any)
	want := first.Add(-2 * time.Hour).Format(time.RFC3339)
	if setup["autonomy_chosen_at"] != want {
		t.Fatalf("autonomy_chosen_at = %v, want the first request's %s", setup, want)
	}
	if got := reader.Model(ctx, "live"); got == nil || got.Configured {
		t.Fatalf("model with no LLM registry = %+v, want known and unconfigured", got)
	}
}

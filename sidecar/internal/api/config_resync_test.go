package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-05 without a database: the conflict path consults the durable
// source and still answers 409 when the source cannot be read.

type fakeAPIDurable struct {
	generation    uint64
	timeout       int
	generationErr error
}

func (d fakeAPIDurable) DurableGeneration(context.Context) (uint64, error) {
	return d.generation, d.generationErr
}

func (d fakeAPIDurable) DurableCandidate(context.Context) (*config.Config, error) {
	candidate := config.DefaultConfig()
	candidate.LLM.TimeoutSeconds = d.timeout
	return candidate, nil
}

func staleControllerPut(
	t *testing.T, controller *config.ConfigController, body string,
) (int, map[string]any) {
	t.Helper()
	user := &auth.User{ID: 1, Role: auth.RoleAdmin}
	// The controller refuses before persistence, so no pool is reached.
	response := doRequestWithUser(
		configGlobalPutHandler(store.NewConfigStore(nil), controller),
		http.MethodPut, "/api/v1/config/global", body, user)
	return response.Code, decodeResyncBody(t, response)
}

// A client that read generation 5 from another replica (behind a load
// balancer) makes this replica resync, and the 409 carries generation 5.
func TestConfigPutConflictAheadOfReplicaResyncs(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil, resyncLLMOwner{})
	controller.SetDurableSource(fakeAPIDurable{generation: 5, timeout: 44})
	code, body := staleControllerPut(t, controller,
		`{"llm.timeout_seconds":55,"expected_generation":5}`)
	if code != http.StatusConflict {
		t.Fatalf("PUT status = %d %v, want 409", code, body)
	}
	if body["desired_generation"] != float64(5) || body["active_generation"] != float64(5) {
		t.Fatalf("409 body = %v, want desired and active generation 5", body)
	}
	if got := controller.Active().Config.LLM.TimeoutSeconds; got != 44 {
		t.Fatalf("active timeout = %d, want the durable 44", got)
	}
}

func TestConfigPutConflictStillAnswers409WhenResyncFails(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	controller.SetDurableSource(fakeAPIDurable{
		generation: 9, generationErr: errors.New("connection refused"),
	})
	code, body := staleControllerPut(t, controller,
		`{"llm.timeout_seconds":55,"expected_generation":7}`)
	if code != http.StatusConflict || body["error"] == nil {
		t.Fatalf("PUT = %d %v, want 409 with an error", code, body)
	}
	if body["desired_generation"] != float64(1) {
		t.Fatalf("409 desired_generation = %v, want the unchanged 1", body["desired_generation"])
	}
	if got := controller.Desired().Generation; got != 1 {
		t.Fatalf("desired generation = %d after a failed resync, want 1", got)
	}
}

// Nil/empty: no durable source (YAML fleet) keeps the plain 409.
func TestConfigPutConflictWithoutDurableSource(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	code, body := staleControllerPut(t, controller,
		`{"llm.timeout_seconds":55,"expected_generation":3}`)
	if code != http.StatusConflict || body["desired_generation"] != float64(1) {
		t.Fatalf("PUT = %d %v, want 409 at generation 1", code, body)
	}
}

func TestConfigGetReportsLastResync(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	get := func() map[string]any {
		response := httptest.NewRecorder()
		configGetHandler(nil, controller).ServeHTTP(response,
			httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/config = %d", response.Code)
		}
		return decodeResyncBody(t, response)
	}
	if body := get(); body["last_resync_at"] != nil {
		t.Fatalf("last_resync_at = %v before any resync, want null", body["last_resync_at"])
	}
	before := time.Now().Add(-time.Second)
	if _, _, err := controller.Resync(
		context.Background(), 4, config.DefaultConfig(),
	); err != nil {
		t.Fatalf("resync: %v", err)
	}
	raw, ok := get()["last_resync_at"].(string)
	if !ok {
		t.Fatalf("last_resync_at missing after a resync")
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || at.Before(before) {
		t.Fatalf("last_resync_at = %q (%v), want a time after %v", raw, err, before)
	}
}

// Nil/empty: a source without a store or a file base fails with context
// instead of panicking, and no query is attempted.
func TestGlobalDurableSourceWithoutStoreOrBase(t *testing.T) {
	ctx := context.Background()
	if _, err := (GlobalDurableSource{}).DurableGeneration(ctx); err == nil {
		t.Fatal("DurableGeneration without a store returned no error")
	}
	if _, err := (GlobalDurableSource{}).DurableCandidate(ctx); err == nil {
		t.Fatal("DurableCandidate without a store returned no error")
	}
	noBase := GlobalDurableSource{Store: store.NewConfigStore(nil)}
	candidate, err := noBase.DurableCandidate(ctx)
	if err == nil || candidate != nil {
		t.Fatalf("DurableCandidate without a base = %v, %v; want an error", candidate, err)
	}
}

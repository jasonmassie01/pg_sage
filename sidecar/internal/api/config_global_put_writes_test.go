package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-10 deleted the legacy global write path and its tests. These keep
// two of its guarantees on the controller path, the only one left: a save
// with one invalid key writes nothing, and a masked secret echoed back by
// the Settings page never replaces the stored secret.

// generationKey is the CAS generation row the store keeps in sage.config.
const generationKey = "__pg_sage_config_generation"

func TestGlobalPut_OneInvalidKeyWritesNothing(t *testing.T) {
	cs, user := contractSetup(t)
	pool, ctx := phase2RequireDB(t)
	cfg := config.DefaultConfig()
	controller := config.NewConfigController(cfg, nil)
	handler := configGlobalPutHandler(cs, controller)

	w := doRequestWithUser(handler, "PUT", "/api/v1/config/global",
		`{"expected_generation": 1,
		  "collector.interval_seconds": "30",
		  "collector.batch_size": "0"}`, user)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	desired := controller.Desired()
	if desired.Generation != 1 {
		t.Fatalf("desired generation = %d, want 1", desired.Generation)
	}
	got := desired.Config.Collector.IntervalSeconds
	if got != cfg.Collector.IntervalSeconds {
		t.Fatalf("interval applied despite the batch_size error: %d", got)
	}
	var rows int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.config WHERE key <> $1`, generationKey,
	).Scan(&rows)
	if err != nil {
		t.Fatalf("count sage.config: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d sage.config rows written despite the validation error", rows)
	}
}

func TestGlobalPut_MaskedAPIKeyIsNotWritten(t *testing.T) {
	cs, user := contractSetup(t)
	pool, ctx := phase2RequireDB(t)
	cfg := config.DefaultConfig()
	cfg.LLM.APIKey = "real-secret-1234"
	controller := config.NewConfigController(cfg, nil)
	handler := configGlobalPutHandler(cs, controller)

	w := doRequestWithUser(handler, "PUT", "/api/v1/config/global",
		`{"expected_generation": 1, "llm.api_key": "************1234",
		  "llm.timeout_seconds": "45"}`, user)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	desired := controller.Desired().Config
	if desired.LLM.APIKey != "real-secret-1234" {
		t.Fatalf("api key replaced by the masked echo: %q", desired.LLM.APIKey)
	}
	if desired.LLM.TimeoutSeconds != 45 {
		t.Fatalf("llm.timeout_seconds = %d, want 45", desired.LLM.TimeoutSeconds)
	}
	var keys []string
	rows, err := pool.Query(ctx,
		`SELECT key FROM sage.config WHERE key <> $1 ORDER BY key`, generationKey)
	if err != nil {
		t.Fatalf("query sage.config: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(keys) != 1 || keys[0] != "llm.timeout_seconds" {
		t.Fatalf("sage.config keys = %v, want only llm.timeout_seconds", keys)
	}
}

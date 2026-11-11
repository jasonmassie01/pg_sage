package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// llm-06b: the MCP briefing resource reads the named database's latest
// sage.briefings row through the fleet. The SQL is tested on a real
// database in internal/briefing (store_db_test.go).
// No concurrent access tests: the adapter holds no state of its own.

func TestMCPBriefingsUnavailableWithoutThePool(t *testing.T) {
	var none fleetBriefingsMCP
	if _, err := none.LatestBriefing(context.Background(), "orders"); !errors.Is(err,
		mcp.ErrIntentUnavailable) {
		t.Fatalf("nil manager: %v, want ErrIntentUnavailable", err)
	}
	manager := fleet.NewManager(config.DefaultConfig())
	access := fleetBriefingsMCP{manager: manager}
	_, err := access.LatestBriefing(context.Background(), "orders")
	if !errors.Is(err, mcp.ErrIntentUnavailable) || !strings.Contains(err.Error(), `"orders"`) {
		t.Fatalf("unknown database: %v, want ErrIntentUnavailable naming orders", err)
	}
}

func TestMCPBriefingRecordMapsTheStoreResult(t *testing.T) {
	_, err := mcpBriefingRecord("orders", briefing.Record{}, briefing.ErrNoBriefing)
	if !errors.Is(err, mcp.ErrNoBriefing) || !strings.Contains(err.Error(),
		briefing.NoBriefingReason) {
		t.Fatalf("no briefing: %v, want ErrNoBriefing with the reason", err)
	}
	cause := errors.New("connection reset")
	if _, err := mcpBriefingRecord("orders", briefing.Record{}, cause); !errors.Is(err, cause) ||
		errors.Is(err, mcp.ErrNoBriefing) {
		t.Fatalf("store error: %v, want the cause", err)
	}
	at := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	rec := briefing.Record{ID: 4, GeneratedAt: at, PeriodStart: at.Add(-time.Hour),
		PeriodEnd: at, Mode: "executive", ContentText: "text",
		ContentJSON: json.RawMessage(`{"total_open": 2, "window": {"end": "x"}}`)}
	raw, err := mcpBriefingRecord("orders", rec, nil)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if string(got["database"]) != `"orders"` || string(got["id"]) != "4" ||
		string(got["delivery_status"]) != "null" {
		t.Fatalf("record fields = %s", raw)
	}
	var content, want any
	_ = json.Unmarshal(got["content_json"], &content)
	_ = json.Unmarshal(rec.ContentJSON, &want)
	cb, _ := json.Marshal(content)
	wb, _ := json.Marshal(want)
	if string(cb) != string(wb) {
		t.Fatalf("content_json changed: %s, want %s", cb, wb)
	}
}

func TestMCPDependenciesServeBriefings(t *testing.T) {
	prev := cfg
	cfg = config.DefaultConfig()
	t.Cleanup(func() { cfg = prev })
	deps := mcpDependencies()
	if _, ok := deps.Briefings.(fleetBriefingsMCP); !ok {
		t.Fatalf("Briefings dependency = %T, want fleetBriefingsMCP", deps.Briefings)
	}
}

package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// No concurrent access tests: findingDetailJSON copies the detail map and
// never writes the caller's, which TestFindingDetailJSONDoesNotMutate pins.

func decodeDetail(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("detail %s is not a JSON object: %v", raw, err)
	}
	return m
}

func TestFindingDetailJSONAddsActionRisk(t *testing.T) {
	raw, err := findingDetailJSON(Finding{ActionRisk: "high_risk",
		Detail: map[string]any{"subset_index": "public.ix"}})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeDetail(t, raw)
	if m[DetailActionRisk] != "high_risk" || m["subset_index"] != "public.ix" {
		t.Fatalf("detail = %v, want action_risk high_risk beside the producer keys", m)
	}
}

func TestFindingDetailJSONNilDetailWithRisk(t *testing.T) {
	raw, err := findingDetailJSON(Finding{ActionRisk: "safe"})
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeDetail(t, raw); len(m) != 1 || m[DetailActionRisk] != "safe" {
		t.Fatalf("detail = %v, want only action_risk safe", m)
	}
}

func TestFindingDetailJSONWithoutRiskIsUnchanged(t *testing.T) {
	raw, err := findingDetailJSON(Finding{Detail: map[string]any{"n": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"n":1}` {
		t.Fatalf("detail = %s, want the producer detail unchanged", raw)
	}
	raw, err = findingDetailJSON(Finding{})
	if err != nil || string(raw) != "null" {
		t.Fatalf("empty finding detail = %s (err %v), want null as before", raw, err)
	}
}

func TestFindingDetailJSONFieldWinsOverStaleKey(t *testing.T) {
	raw, _ := findingDetailJSON(Finding{ActionRisk: "moderate",
		Detail: map[string]any{DetailActionRisk: "safe"}})
	if m := decodeDetail(t, raw); m[DetailActionRisk] != "moderate" {
		t.Fatalf("detail = %v, want the ActionRisk field to win", m)
	}
}

func TestFindingDetailJSONDoesNotMutate(t *testing.T) {
	detail := map[string]any{"n": 1}
	if _, err := findingDetailJSON(Finding{ActionRisk: "safe", Detail: detail}); err != nil {
		t.Fatal(err)
	}
	if _, ok := detail[DetailActionRisk]; ok || len(detail) != 1 {
		t.Fatalf("caller's detail was mutated: %v", detail)
	}
}

func TestFindingDetailJSONMarshalError(t *testing.T) {
	_, err := findingDetailJSON(Finding{ActionRisk: "safe",
		Detail: map[string]any{"bad": make(chan int)}})
	if err == nil {
		t.Fatal("an unmarshalable detail must return an error")
	}
}

func TestUpsertWritesAndRefreshesActionRisk(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	ident := fmt.Sprintf("public.risk_upsert_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier=$1", ident)
	})
	f := Finding{Category: "duplicate_index", Severity: "info", ObjectType: "index",
		ObjectIdentifier: ident, Title: "t", ActionRisk: "safe",
		Detail: map[string]any{"n": 1}}
	read := func() string {
		var risk string
		if err := pool.QueryRow(ctx, `SELECT detail->>'action_risk' FROM sage.findings
			WHERE object_identifier=$1 AND status='open'`, ident).Scan(&risk); err != nil {
			t.Fatalf("read action_risk: %v", err)
		}
		return risk
	}
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := read(); got != "safe" {
		t.Fatalf("inserted action_risk = %q, want safe", got)
	}
	f.ActionRisk = "high_risk"
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := read(); got != "high_risk" {
		t.Fatalf("refreshed action_risk = %q, want high_risk", got)
	}
}

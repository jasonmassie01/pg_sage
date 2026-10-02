package probes

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

// Signal probes (M5): the change feed and the SLO status are evidence
// sources outside the SQL catalog. Their rows decode the same way from a
// live result and from stored evidence (JSON, numbers as json.Number).

func TestSignalProbes_AreNotSQLCatalogProbes(t *testing.T) {
	for _, id := range []ID{ChangeFeed, SLOStatus} {
		if _, ok := Catalog().Spec(id); ok {
			t.Errorf("%s is in the SQL catalog: the model could propose it", id)
		}
		if !IsSignal(id) {
			t.Errorf("IsSignal(%s) = false", id)
		}
	}
	if IsSignal(LockGraph) {
		t.Fatal("a SQL probe is a signal probe")
	}
}

// roundTrip stores a result as evidence does (JSON) and decodes it back
// with json.Number, as observations() does.
func roundTrip(t *testing.T, res Result) Result {
	t.Helper()
	raw, err := res.Payload()
	if err != nil {
		t.Fatal(err)
	}
	var out Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChangeRows(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 58, 0, 0, time.UTC)
	res := Result{ProbeID: ChangeFeed, Version: "v1", Status: StatusOK, Rows: []Row{{
		"kind": "deploy", "source": "github-actions", "summary": "deploy checkout v1.2.3",
		"occurred_at": at, "age_s": 120.0, "signature": "verified",
		"event_id": "run-1", "service": "checkout"}}}
	for name, r := range map[string]Result{"live": res, "stored": roundTrip(t, res)} {
		rows, err := ChangeRows(r)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: rows=%v err=%v", name, rows, err)
		}
		c := rows[0]
		if c.Kind != "deploy" || c.Source != "github-actions" || c.AgeS != 120 ||
			!c.OccurredAt.Equal(at) || c.Signature != "verified" || c.EventID != "run-1" ||
			c.Service != "checkout" || c.Summary != "deploy checkout v1.2.3" {
			t.Fatalf("%s: row = %+v", name, c)
		}
	}
	empty := Result{ProbeID: ChangeFeed, Status: StatusEmpty}
	if rows, err := ChangeRows(empty); err != nil || len(rows) != 0 {
		t.Fatalf("empty: %v %v", rows, err)
	}
	var unavailable *UnavailableError
	_, err := ChangeRows(Result{ProbeID: ChangeFeed, Status: StatusError, Reason: "x"})
	if !errors.As(err, &unavailable) {
		t.Fatalf("error result: err = %v", err)
	}
	if _, err := ChangeRows(Result{ProbeID: LockGraph, Status: StatusOK}); err == nil {
		t.Fatal("a lock_graph result decoded as the change feed")
	}
	if _, err := ChangeRows(Result{ProbeID: ChangeFeed, Status: StatusOK,
		Rows: []Row{{"summary": "no kind"}}}); err == nil {
		t.Fatal("a row without a kind decoded")
	}
}

func TestSLORows(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 59, 0, 0, time.UTC)
	res := Result{ProbeID: SLOStatus, Version: "v1", Status: StatusOK, Rows: []Row{
		{"name": "checkout", "kind": "app", "state": "page", "fast_burning": true,
			"customer_impact": true, "burn_long": 16.25, "burn_short": 15.0,
			"long_window": "1h", "short_window": "5m", "unknown": "",
			"budget_remaining": 0.4, "evaluated_at": at, "age_s": 30.0},
		{"name": "db_latency", "kind": "proxy", "state": "unknown", "fast_burning": false,
			"customer_impact": false, "burn_long": nil, "burn_short": nil,
			"long_window": "1h", "short_window": "5m", "unknown": "baseline_building",
			"budget_remaining": nil, "evaluated_at": at, "age_s": 30.0},
	}}
	for name, r := range map[string]Result{"live": res, "stored": roundTrip(t, res)} {
		rows, err := SLORows(r)
		if err != nil || len(rows) != 2 {
			t.Fatalf("%s: rows=%v err=%v", name, rows, err)
		}
		a, p := rows[0], rows[1]
		if a.Name != "checkout" || a.Kind != "app" || a.State != "page" || !a.FastBurning ||
			!a.CustomerImpact || a.BurnLong != 16.25 || a.BurnShort != 15 ||
			a.LongWindow != "1h" || a.BudgetRemaining != 0.4 || !a.EvaluatedAt.Equal(at) {
			t.Fatalf("%s: app row = %+v", name, a)
		}
		if p.Kind != "proxy" || p.Unknown != "baseline_building" || !math.IsNaN(p.BurnLong) ||
			!math.IsNaN(p.BudgetRemaining) || p.CustomerImpact {
			t.Fatalf("%s: proxy row = %+v", name, p)
		}
	}
	if _, err := SLORows(Result{ProbeID: SLOStatus, Status: StatusOK,
		Rows: []Row{{"kind": "app"}}}); err == nil {
		t.Fatal("a row without a name decoded")
	}
}

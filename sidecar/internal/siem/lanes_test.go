package siem

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A stub keeps the event's class and type, says Failure, carries the
// link's position, op and hashes and the "undeliverable" label, and leaves
// out the row and the query.
func TestStubEventCarriesTheLinkOnly(t *testing.T) {
	r := Record{Source: "orders", Chain: "action_log", Link: link("action_log", 12, "U"),
		Row: map[string]any{"sql_executed": "CREATE INDEX secret_idx ON t (a)",
			"outcome": "rolled_back"}, Labels: []string{forkLabel}}
	full := Map(r)
	stub := stubEvent(r)
	for _, k := range []string{"class_uid", "class_name", "category_uid", "activity_id",
		"activity_name", "type_uid", "type_name", "time"} {
		if stub[k] != full[k] {
			t.Fatalf("stub %s = %v, event %v", k, stub[k], full[k])
		}
	}
	if stub["status"] != "Failure" || num(t, stub, "status_id") != statusFailure {
		t.Fatalf("stub status = %v/%v, want Failure", stub["status"], stub["status_id"])
	}
	if num(t, stub, "severity_id") < SeverityMedium {
		t.Fatalf("stub severity %v: a lost event must not read as routine",
			stub["severity_id"])
	}
	md := stub["metadata"].(map[string]any)
	labels, _ := md["labels"].([]string)
	if strings.Join(labels, ",") != "fork,undeliverable" {
		t.Fatalf("stub labels = %v, want fork,undeliverable", labels)
	}
	if md["uid"] != "orders/action_log/12" {
		t.Fatalf("stub uid = %v", md["uid"])
	}
	ext := stub["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
	if ext["seq"] != int64(12) || ext["op"] != "U" || ext["hash"] != "h" ||
		ext["prev_hash"] != "p" || ext["sealed_hash"] != "s" || ext["chain"] != "action_log" {
		t.Fatalf("stub chain proof = %v", ext)
	}
	if _, ok := ext["row"]; ok {
		t.Fatalf("stub carries the row: %v", ext["row"])
	}
	body, err := json.Marshal(stub)
	if err != nil {
		t.Fatalf("encode stub: %v", err)
	}
	if strings.Contains(string(body), "secret_idx") || strings.Contains(string(body),
		"query_info") {
		t.Fatalf("stub leaks content: %s", body)
	}
}

// A record without labels gets only "undeliverable", and the stub never
// shares the record's label slice.
func TestStubEventLabelsAreItsOwn(t *testing.T) {
	r := Record{Source: "control", Chain: "auth_audit", Link: link("auth_audit", 1, "I")}
	stub := stubEvent(r)
	labels := stub["metadata"].(map[string]any)["labels"].([]string)
	if len(labels) != 1 || labels[0] != UndeliverableLabel {
		t.Fatalf("labels = %v", labels)
	}
	if r.Labels != nil {
		t.Fatalf("stubEvent changed the record's labels: %v", r.Labels)
	}
}

// Backoff doubles per consecutive failure from Interval up to MaxBackoff.
func TestDelayBoundaries(t *testing.T) {
	e := &Exporter{opts: Options{Interval: 10 * time.Millisecond,
		MaxBackoff: 75 * time.Millisecond}}
	for _, c := range []struct {
		failures int
		want     time.Duration
	}{{0, 10 * time.Millisecond}, {1, 10 * time.Millisecond}, {2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond}, {4, 75 * time.Millisecond},
		{1000, 75 * time.Millisecond}} {
		if got := e.delay(c.failures); got != c.want {
			t.Fatalf("delay(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

// A lane is regressed (export stopped), else poisoned (a stub refused),
// else failing, else ok; lag is head minus cursor, never negative; lanes
// list in source/chain order.
func TestLaneStatusStates(t *testing.T) {
	s := newState("soc")
	if st := s.statusLocked(); st.Lanes == nil || len(st.Lanes) != 0 {
		t.Fatalf("fresh sink lanes = %#v, want empty and not nil", st.Lanes)
	}
	s.lane("orders", "action_log").track(4, 10)
	s.lane("billing", "auth_audit").track(9, 3)
	fl := s.lane("orders", "auth_audit")
	fl.track(1, 2)
	fl.failures, fl.lastError = 2, "receiver 503"
	pl := s.lane("orders", "config_audit")
	pl.failures, pl.lastError, pl.poisoned = 1, "refused", true
	s.regressed[regressionKey("billing", "auth_audit")] = Regression{Source: "billing",
		Chain: "auth_audit", Seq: 9}
	st := s.statusLocked()
	want := []struct {
		key, state string
		lag        int64
	}{{"billing/auth_audit", LaneRegressed, 0}, {"orders/action_log", LaneOK, 6},
		{"orders/auth_audit", LaneFailing, 1}, {"orders/config_audit", LanePoisoned, 0}}
	if len(st.Lanes) != len(want) {
		t.Fatalf("lanes = %+v", st.Lanes)
	}
	for i, w := range want {
		l := st.Lanes[i]
		if l.Source+"/"+l.Chain != w.key || l.State != w.state || l.Lag != w.lag {
			t.Fatalf("lane %d = %+v, want %s %s lag %d", i, l, w.key, w.state, w.lag)
		}
	}
	if st.Lanes[2].LastError != "receiver 503" || st.Lanes[2].ConsecutiveFailures != 2 {
		t.Fatalf("failing lane = %+v", st.Lanes[2])
	}
}

// keepLanes drops lanes a pass no longer visited (a removed source or
// chain); keepSource keeps every lane and regression of a source the pass
// could not read.
func TestLanePruning(t *testing.T) {
	s := newState("soc")
	s.lane("orders", "action_log")
	s.lane("gone", "action_log")
	s.regressed[regressionKey("down", "auth_audit")] = Regression{Source: "down"}
	s.lane("down", "auth_audit")
	visited := map[string]bool{regressionKey("orders", "action_log"): true}
	s.keepSource("down", visited)
	s.keepLanes(visited)
	s.keepRegressions(visited)
	st := s.statusLocked()
	if len(st.Lanes) != 2 || st.Lanes[0].Source != "down" || st.Lanes[1].Source != "orders" {
		t.Fatalf("lanes after pruning = %+v", st.Lanes)
	}
	if len(st.Regressions) != 1 {
		t.Fatalf("the unreadable source lost its regression: %+v", st.Regressions)
	}
}

// errPoisoned is distinguishable from a transient failure and from a
// permanent refusal of the event itself.
func TestPoisonedIsDistinct(t *testing.T) {
	err := poisonedError(7, errors.New("HTTP 413"), errors.New("HTTP 413 again"))
	if !errors.Is(err, errPoisoned) || errors.Is(err, ErrPermanent) {
		t.Fatalf("poisoned error = %v", err)
	}
	if !strings.Contains(err.Error(), "7") || !strings.Contains(err.Error(), "again") {
		t.Fatalf("poisoned error does not name the link and both refusals: %v", err)
	}
}

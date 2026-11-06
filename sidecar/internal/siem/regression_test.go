package siem

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// metadata.labels is set only on labelled records, as a copy.
func TestMapLabels(t *testing.T) {
	r := Record{Source: "orders", Chain: "action_log", Labels: []string{forkLabel},
		Link: auditchain.Link{Seq: 8, Op: "I", At: time.Unix(1, 0)}}
	e := Map(r)
	got, _ := e["metadata"].(map[string]any)["labels"].([]string)
	if len(got) != 1 || got[0] != "fork" {
		t.Fatalf("labels = %v", e["metadata"])
	}
	r.Labels[0] = "changed"
	if got[0] != "fork" {
		t.Fatalf("the event shares the record's label slice")
	}
	r.Labels = nil
	if _, ok := Map(r)["metadata"].(map[string]any)["labels"]; ok {
		t.Fatalf("an unlabelled record carries labels")
	}
}

func newState(name string) *sinkState {
	return &sinkState{cfg: SinkConfig{Sink: &memSink{name: name}},
		turn: make(chan struct{}, 1), status: SinkStatus{Name: name},
		regressed: map[string]Regression{}}
}

// A sink is ok, failing, or regressed (which wins: export of a chain is
// stopped); regressions list in source/chain order and the last error
// names the first and counts the rest.
func TestSinkStatusPrecedence(t *testing.T) {
	s := newState("soc")
	if st := s.statusLocked(); st.Status != StatusOK || st.Regressions != nil {
		t.Fatalf("fresh sink = %+v", st)
	}
	s.status.ConsecutiveFailures = 2
	if st := s.statusLocked(); st.Status != StatusFailing {
		t.Fatalf("failing sink = %+v", st)
	}
	e := &Exporter{logf: func(string, ...any) {}}
	for _, r := range []Regression{{Source: "orders", Chain: "auth_audit", Seq: 4},
		{Source: "billing", Chain: "action_log", Seq: 9},
		{Source: "orders", Chain: "action_log", Seq: 2}} {
		r.Reason = "gone"
		e.regress(s, r)
	}
	st := s.statusLocked()
	if st.Status != StatusRegressed || len(st.Regressions) != 3 {
		t.Fatalf("regressed sink = %+v", st)
	}
	order := []string{"billing/action_log", "orders/action_log", "orders/auth_audit"}
	for i, r := range st.Regressions {
		if r.Source+"/"+r.Chain != order[i] {
			t.Fatalf("regressions out of order: %+v", st.Regressions)
		}
	}
	msg := s.regressionError()
	if !strings.Contains(msg, "billing") || !strings.Contains(msg, "9") ||
		!strings.Contains(msg, "and 2 more") {
		t.Fatalf("last error = %q", msg)
	}
	s.keepRegressions(map[string]bool{regressionKey("orders", "action_log"): true})
	if st := s.statusLocked(); len(st.Regressions) != 1 || st.Regressions[0].Seq != 2 {
		t.Fatalf("after pruning = %+v", st.Regressions)
	}
	s.clearRegression(regressionKey("orders", "action_log"))
	if s.regressionError() != "" || s.statusLocked().Status != StatusFailing {
		t.Fatalf("after clearing = %+v", s.statusLocked())
	}
}

// A regression is logged once, not on every pass.
func TestRegressionIsLoggedOnce(t *testing.T) {
	var lines []string
	e := &Exporter{logf: func(f string, a ...any) { lines = append(lines, f) }}
	s := newState("soc")
	r := Regression{Source: "orders", Chain: "action_log", Seq: 10, Reason: "gone"}
	e.regress(s, r)
	e.regress(s, r)
	if len(lines) != 1 || !strings.Contains(lines[0], "reanchor") {
		t.Fatalf("log lines = %v", lines)
	}
}

// A cursor at 0 has nothing to check and needs no database; a failed
// lookup is an error, not a verdict.
func TestCheckAnchorWithoutLookup(t *testing.T) {
	a, err := CheckAnchor(context.Background(), nil, Cursor{Chain: "action_log"})
	if err != nil || !a.OK {
		t.Fatalf("cursor at 0 = %+v, %v", a, err)
	}
	_, err = CheckAnchor(context.Background(), nil, Cursor{Chain: "action_log", Seq: 3})
	if err == nil || !strings.Contains(err.Error(), "action_log") {
		t.Fatalf("no database: err = %v", err)
	}
}

// The turn is exclusive and its wait ends with the context.
func TestSinkTurnRespectsContext(t *testing.T) {
	s := newState("soc")
	if err := s.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire: err = %v, want the context's deadline", err)
	}
	s.release()
	if err := s.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

// Re-anchor input is refused before anything is read: no leader check,
// no source, no database.
func TestReanchorValidatesFirst(t *testing.T) {
	asked := false
	e := NewExporter(func() []Source { return nil },
		[]SinkConfig{{Sink: &memSink{name: "soc"}}}, NewCursorStore(nil), Options{},
		func() (Fence, bool) { asked = true; return Fence{}, true }, nil)
	ctx := context.Background()
	for _, req := range []ReanchorRequest{
		{Sink: "soc", Source: "orders", Chain: "nope", FromSeq: 1},
		{Sink: "soc", Source: "orders", Chain: "action_log", FromSeq: 0},
	} {
		if _, err := e.Reanchor(ctx, req); !errors.Is(err, ErrInvalidReanchor) {
			t.Fatalf("%+v: err = %v", req, err)
		}
	}
	if _, err := e.Reanchor(ctx, ReanchorRequest{Sink: "x", Chain: "action_log",
		FromSeq: 1}); !errors.Is(err, ErrUnknownSink) {
		t.Fatalf("unknown sink: err = %v", err)
	}
	if asked {
		t.Fatalf("invalid input reached the leader check")
	}
	if _, err := e.Reanchor(ctx, ReanchorRequest{Sink: "soc", Source: "orders",
		Chain: "action_log", FromSeq: 1}); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("no sources: err = %v", err)
	}
}

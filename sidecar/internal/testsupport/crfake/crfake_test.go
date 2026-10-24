package crfake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The fake stages themselves. Pool and Start need a live test database and
// are exercised by the api, mcp and cmd packages that use them.

func TestAnalyzerFindingsFollowTheMarkers(t *testing.T) {
	a := &Analyzer{}
	in := changereview.Input{SQLFiles: []changereview.SQLFile{
		{Content: "ALTER TABLE t DROP COLUMN c; " + MarkRisky},
		{Path: "db/2.sql", Content: "ALTER TABLE t ALTER c TYPE text " + MarkReview},
		{Path: "db/3.sql", Content: "CREATE TABLE n (id bigint)"}}}
	got, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{changereview.SeverityCritical, changereview.SeverityWarning,
		changereview.SeverityInfo}
	if len(got.Findings) != 3 || len(got.Statements) != 3 || len(got.Evidence) != 3 ||
		got.ParseMode != changereview.ParseAST || a.Calls() != 1 {
		t.Fatalf("analysis %+v calls %d", got, a.Calls())
	}
	for i, f := range got.Findings {
		if f.Severity != want[i] || f.Origin != changereview.OriginRule ||
			f.EvidenceIDs[0] != got.Evidence[i].ID || !got.Evidence[i].Verify() {
			t.Errorf("finding %d: %+v (evidence %+v)", i, f, got.Evidence[i])
		}
	}
	if got.Statements[1].File != "db/2.sql" || got.Statements[2].Index != 3 {
		t.Fatalf("statements %+v", got.Statements)
	}
	if v := changereview.VerdictOf(got.Findings, got.ParseMode); v != changereview.VerdictRisky {
		t.Fatalf("verdict %s, want risky", v)
	}
}

func TestAnalyzerGateBlocksUntilReleasedOrCancelled(t *testing.T) {
	a := &Analyzer{Gate: make(chan struct{}), Entered: make(chan struct{}, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Analyze(ctx, changereview.Input{})
		done <- err
	}()
	<-a.Entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled analysis: %v", err)
	}
	close(a.Gate)
	if _, err := a.Analyze(context.Background(), changereview.Input{}); err != nil {
		t.Fatalf("released gate: %v", err)
	}
}

func TestReasonerReportsItsStatus(t *testing.T) {
	r, err := Reasoner{Status: changereview.ModelBudget}.Reason(context.Background(),
		changereview.Input{}, changereview.Analysis{})
	if err != nil || r.ModelStatus != changereview.ModelBudget || len(r.Findings) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if Config().MaxConcurrentPerDatabase != 2 || !Config().Enabled {
		t.Fatalf("config %+v, want the defaults", Config())
	}
}

func TestLogfDropsLinesAfterTheTest(t *testing.T) {
	var logf func(string, ...any)
	t.Run("inner", func(t *testing.T) { logf = Logf(t); logf("during %d", 1) })
	done := make(chan struct{})
	go func() { logf("after the test ended"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Logf blocked after the test ended")
	}
}

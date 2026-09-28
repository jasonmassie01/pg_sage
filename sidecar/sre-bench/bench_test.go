package srebench

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "sre-bench"))
}

// TestPGIncidentBench runs every scenario's fault program on real
// PostgreSQL through the real investigator (store, lease, probe plan,
// causal graph; no LLM) and scores the persisted diagnoses. Gates (the
// R1 targets of AI-SRE-SPEC §12, on this seed set): top-1 >= 80% where
// the cause is known (CHECK-36), 100% abstention on benign scenarios,
// precision >= 90%, and decoys within 10 points of clean top-1
// (CHECK-42). Every fault program must manifest and recover.
func TestPGIncidentBench(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	env := NewEnv(ctx, t, dsn)
	results := Run(ctx, env, Scenarios())
	t.Log("\n" + Report(results))
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("scenario %s: %v", r.Scenario.ID, r.Err)
		}
	}
	_, pooled := ScoreResults(results)
	clean, decoy := ScoreClass(results, ClassPositive), ScoreClass(results, ClassDecoy)
	switch {
	case pooled.Top1() < 0.8:
		t.Errorf("top-1 %.2f below 0.80", pooled.Top1())
	case pooled.Abstention() < 1:
		t.Errorf("abstention %.2f below 1.00", pooled.Abstention())
	case pooled.Precision() < 0.9:
		t.Errorf("precision %.2f below 0.90", pooled.Precision())
	case decoy.Top1() < clean.Top1()-0.1:
		t.Errorf("decoy top-1 %.2f more than 10 points under clean %.2f",
			decoy.Top1(), clean.Top1())
	}
}

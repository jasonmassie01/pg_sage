package candidate

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

const (
	dropSQL     = `DROP INDEX CONCURRENTLY "public"."t_a";`
	rollbackSQL = "CREATE INDEX t_a ON public.t USING btree (a);"
	factID      = "index/idx/public.t_a"
)

// testPack holds one index fact, index/idx/public.t_a.
func testPack(t *testing.T) *workpack.Pack {
	t.Helper()
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90),
		Indexes: &workpack.Indexes{ScansStatus: "ok", Indexes: []workpack.IndexFact{{
			ID: factID, Schema: "public", Table: "t", Index: "t_a", Method: "btree",
			Definition: "CREATE INDEX t_a ON public.t USING btree (a)", SizeBytes: 8192,
			Valid: true, Scans: 0, TableWrites: "unknown",
			DuplicateOf: []string{}, PrefixOf: []string{}}}}})
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

// dropCandidate is a valid drop of public.t_a.
func dropCandidate() Candidate {
	c := Candidate{Generator: "indexes", Dimension: "schema", Kind: "unused",
		Target: "public.t_a", ActionType: "drop_unused_index", ForwardSQL: dropSQL,
		RollbackSQL: rollbackSQL, EvidenceIDs: []string{factID}, Reversible: true,
		Approval: ApprovalAutomatic, Reason: "no scans in 90 days",
		Prediction: Prediction{Metric: "index_bytes", Direction: "decrease", Tier: TierNone,
			Source: "pack"}}
	c.ID = NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL)
	return c
}

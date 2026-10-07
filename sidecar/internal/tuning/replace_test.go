package tuning

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 2.3: a candidate that would make an existing index redundant is
// no longer refused. It becomes one replacement: create the wider index
// CONCURRENTLY and drop the subsumed one in the same approved action, with
// the old index's identity (OID) and definition recorded for the soft drop.
// An in-flight index it would make redundant is still refused: nothing has
// been built to replace yet.

const (
	statusDef  = "CREATE INDEX orders_status_idx ON public.orders USING btree (status)"
	statusWide = "CREATE INDEX CONCURRENTLY orders_s_c ON public.orders (status, created_at)"
)

// replaceSetup puts orders_status_idx (OID 4242) in the table context and
// the snapshot.
func replaceSetup(t *testing.T) (*harness, *collector.Snapshot) {
	t.Helper()
	h := newHarness(t)
	tc := h.indexes.contexts["public.orders"]
	tc.Indexes = append(tc.Indexes, optimizer.IndexInfo{Name: "orders_status_idx",
		IsValid: true, Definition: statusDef})
	h.indexes.contexts["public.orders"] = tc
	cur := validationSnap()
	for i := range cur.Indexes {
		if cur.Indexes[i].IndexRelName == "orders_status_idx" {
			cur.Indexes[i].IndexRelID = 4242
		}
	}
	return h, cur
}

func judgeOn(t *testing.T, h *harness, cur *collector.Snapshot, p Proposal) Judged {
	t.Helper()
	prev, _ := ordersPair()
	v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0), nil, h.store.rejected)
	v.prepare(context.Background(), prev, nil)
	return v.judge(context.Background(), ordersCase(), ordersEvidence(), p)
}

func TestJudge_CandidateSubsumingAnExistingIndexBecomesAReplacement(t *testing.T) {
	h, cur := replaceSetup(t)
	j := judgeOn(t, h, cur, indexProposal(statusWide))
	if j.Verdict != VerdictAdmitted || j.Finding == nil {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	wantSQL := optimizer.IndexReplaceSQL(statusWide, "public.orders_status_idx")
	wantUndo := optimizer.IndexReplaceRollbackSQL(statusDef, "public.orders_s_c")
	if f.Category != CategoryIndexReplace || f.RecommendedSQL != wantSQL ||
		f.RollbackSQL != wantUndo || f.ObjectIdentifier != "public.orders" {
		t.Fatalf("finding = %+v", f)
	}
	rep, ok := f.Detail["index_replace"].(map[string]any)
	if !ok || rep["old_index"] != "public.orders_status_idx" ||
		rep["old_index_oid"] != int64(4242) || rep["old_definition"] != statusDef ||
		rep["new_index"] != "public.orders_s_c" {
		t.Fatalf("replace detail = %#v", f.Detail["index_replace"])
	}
	if f.Detail["table"] != "public.orders" ||
		!strings.Contains(f.Title, "orders_status_idx") {
		t.Fatalf("title %q detail %v", f.Title, f.Detail)
	}
	if j.Class != verify.ClassIndexReplace || j.Prediction.Class != verify.ClassIndexReplace ||
		len(j.Prediction.TargetQueryIDs) != 1 || j.Prediction.TargetQueryIDs[0] != 101 {
		t.Fatalf("class %q prediction %+v", j.Class, j.Prediction)
	}
	if got := h.indexes.admittedDDL(); len(got) != 1 || got[0] != statusWide {
		t.Fatalf("the wider index is measured like any create: %v", got)
	}
}

func TestJudge_ReplacementRefusals(t *testing.T) {
	t.Run("two subsumed indexes", func(t *testing.T) {
		h, cur := replaceSetup(t)
		tc := h.indexes.contexts["public.orders"]
		tc.Indexes = append(tc.Indexes, optimizer.IndexInfo{Name: "orders_status_live",
			IsValid: true, Definition: "CREATE INDEX orders_status_live ON public.orders " +
				"USING btree (status) WHERE (deleted_at IS NULL)"})
		h.indexes.contexts["public.orders"] = tc
		j := judgeOn(t, h, cur, indexProposal(statusWide))
		if j.Verdict != VerdictRejected || j.Reason != ReasonSubsumes ||
			!strings.Contains(j.Detail, "one index") {
			t.Fatalf("a replacement replaces one index: %+v", j)
		}
	})
	t.Run("identity unknown", func(t *testing.T) {
		h, _ := replaceSetup(t)
		j := judgeOn(t, h, validationSnap(), indexProposal(statusWide))
		if j.Verdict != VerdictRejected || j.Reason != ReasonSubsumes ||
			!strings.Contains(j.Detail, "identity") {
			t.Fatalf("an old index without an OID is not replaced: %+v", j)
		}
	})
	t.Run("old index being dropped this cycle", func(t *testing.T) {
		h, cur := replaceSetup(t)
		prev, _ := ordersPair()
		v := h.agent.newValidator(cur, ClassifyWorkload(cur, nil, t0), nil, h.store.rejected)
		v.prepare(context.Background(), prev, nil)
		v.dropping["orders_status_idx"] = true
		j := v.judge(context.Background(), ordersCase(), ordersEvidence(),
			indexProposal(statusWide))
		if j.Verdict != VerdictRejected || j.Reason != ReasonSubsumes {
			t.Fatalf("an index already proposed for a drop is not replaced too: %+v", j)
		}
	})
	t.Run("in-flight index", func(t *testing.T) {
		h, cur := replaceSetup(t)
		h.store.queue = []string{inflightLive}
		weaker := "CREATE INDEX CONCURRENTLY orders_status_created_current ON " +
			"public.orders (status, created_at) WHERE deleted_at IS NULL"
		j := judgeOn(t, h, cur, indexProposal(weaker))
		if j.Verdict != VerdictRejected || j.Reason != ReasonSubsumes ||
			!strings.Contains(j.Detail, "orders_live_status_created") {
			t.Fatalf("an in-flight index is never replaced: %+v", j)
		}
	})
}

// A queued or open replacement is in flight: its create half is read like
// any queued index create.
func TestLoadInFlightReadsTheCreateOfAQueuedReplacement(t *testing.T) {
	h, cur := replaceSetup(t)
	h.store.queue = []string{optimizer.IndexReplaceSQL(
		"CREATE INDEX CONCURRENTLY orders_c_all ON public.orders (customer_id, created_at)",
		"public.orders_customer_idx")}
	j := judgeOn(t, h, cur, createProposal())
	if j.Verdict != VerdictRejected || j.Reason != ReasonDuplicate {
		t.Fatalf("a queued replacement already serves it: %+v", j)
	}
}

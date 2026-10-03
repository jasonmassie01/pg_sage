package executor

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// Pure parts of the stale-approval fix. No concurrent access tests here:
// these functions are stateless; the DB-backed tests cover concurrency.

func TestOverlayGateEvidenceTakesLiveVerdict(t *testing.T) {
	revision := map[string]any{"queryids": []any{7.0}, "hypopg_validated": false}
	live := map[string]any{"what_if_verdict": "verified", "hypopg_validated": true,
		"queryids": []any{9.0}, "estimated_improvement_pct": 90.0}
	got := overlayGateEvidence(revision, live)
	want := map[string]any{"queryids": []any{7.0}, "what_if_verdict": "verified",
		"hypopg_validated": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay = %v, want %v", got, want)
	}
	if _, changed := revision["what_if_verdict"]; changed || revision["hypopg_validated"] != false {
		t.Fatalf("overlay mutated the revision evidence: %v", revision)
	}
}

// A gate key the live finding no longer carries is dropped: the reason it
// stood for is gone (e.g. approval_required cleared by the producer).
func TestOverlayGateEvidenceDropsVanishedKeys(t *testing.T) {
	revision := map[string]any{analyzer.DetailApprovalRequired: "standby usage unknown",
		"what_if_verdict": "unverified", "what_if_reason": "no HypoPG", "keep": 1}
	got := overlayGateEvidence(revision, map[string]any{})
	if !reflect.DeepEqual(got, map[string]any{"keep": 1}) {
		t.Fatalf("overlay = %v, want only non-gate keys", got)
	}
}

func TestOverlayGateEvidenceNilMaps(t *testing.T) {
	if got := overlayGateEvidence(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("overlay(nil, nil) = %#v, want an empty map", got)
	}
	got := overlayGateEvidence(nil, map[string]any{"what_if_verdict": "verified"})
	if got["what_if_verdict"] != "verified" {
		t.Fatalf("overlay(nil, live) = %v", got)
	}
}

func staleCandidate(evidence map[string]any) *recommendation.Candidate {
	c := &recommendation.Candidate{}
	c.ID, c.ContentHash = 41, "hash-a"
	c.Current.Evidence = evidence
	return c
}

func TestQueuedApprovalSameContent(t *testing.T) {
	const sql = "CREATE INDEX CONCURRENTLY i ON public.t (c);"
	rec := int64(41)
	other := int64(42)
	cand := staleCandidate(nil)
	cases := []struct {
		name string
		q    queuedApproval
		cand *recommendation.Candidate
		want bool
	}{
		{"pinned same", queuedApproval{RecommendationID: &rec, ContentHash: "hash-a",
			SQL: sql}, cand, true},
		{"pinned, whitespace", queuedApproval{RecommendationID: &rec,
			ContentHash: "hash-a", SQL: "  " + sql + "\n"}, cand, true},
		{"other hash", queuedApproval{RecommendationID: &rec, ContentHash: "hash-b",
			SQL: sql}, cand, false},
		{"other recommendation", queuedApproval{RecommendationID: &other,
			ContentHash: "hash-a", SQL: sql}, cand, false},
		{"other SQL", queuedApproval{RecommendationID: &rec, ContentHash: "hash-a",
			SQL: "CREATE INDEX CONCURRENTLY j ON public.t (c);"}, cand, false},
		{"legacy same SQL", queuedApproval{SQL: sql}, cand, true},
		{"legacy other SQL", queuedApproval{SQL: "VACUUM public.t"}, cand, false},
		{"no candidate", queuedApproval{RecommendationID: &rec, SQL: sql}, nil, true},
		{"empty SQL", queuedApproval{}, cand, false},
	}
	for _, c := range cases {
		if got := c.q.sameContent(sql, c.cand); got != c.want {
			t.Errorf("%s: sameContent = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestClassifyQueuedApprovals(t *testing.T) {
	const sql = "CREATE INDEX CONCURRENTLY i ON public.t (c);"
	same := func(id int, status string) queuedApproval {
		return queuedApproval{ID: id, Status: status, SQL: sql}
	}
	other := func(id int, status string) queuedApproval {
		return queuedApproval{ID: id, Status: status, SQL: "VACUUM public.t"}
	}
	expired := same(9, "pending")
	expired.Expired = true
	cases := []struct {
		name      string
		rows      []queuedApproval
		supersede []int
		blocked   string // substring; "" = not blocked
	}{
		{"none", nil, nil, ""},
		{"pending same", []queuedApproval{same(1, "pending"), same(2, "pending")},
			[]int{1, 2}, ""},
		{"expired pending ignored", []queuedApproval{expired}, nil, ""},
		{"rejected same", []queuedApproval{same(1, "pending"), same(3, "rejected")},
			nil, "rejected"},
		{"rejected other content", []queuedApproval{other(3, "rejected"),
			same(1, "pending")}, []int{1}, ""},
		{"approved", []queuedApproval{same(4, "approved")}, nil, "approved"},
		{"approved other content", []queuedApproval{other(4, "approved")}, nil,
			"approved"},
		{"failed approved retry", []queuedApproval{same(5, "failed")}, nil, "approved"},
		{"pending other content", []queuedApproval{same(1, "pending"),
			other(6, "pending")}, nil, "different"},
	}
	for _, c := range cases {
		supersede, blocked := classifyQueuedApprovals(c.rows, sql, nil)
		if !reflect.DeepEqual(supersede, c.supersede) {
			t.Errorf("%s: supersede = %v, want %v", c.name, supersede, c.supersede)
		}
		if (blocked == "") != (c.blocked == "") || !strings.Contains(blocked, c.blocked) {
			t.Errorf("%s: blocked = %q, want %q", c.name, blocked, c.blocked)
		}
	}
}

func TestStaleApprovalReason(t *testing.T) {
	verified := map[string]any{"what_if_verdict": "verified"}
	unverified := map[string]any{"what_if_verdict": "unverified"}
	f := analyzer.Finding{Detail: verified}
	cases := []struct {
		name     string
		f        analyzer.Finding
		cand     *recommendation.Candidate
		decision int64
		want     string
	}{
		{"what-if newly verified", f, staleCandidate(unverified), 12,
			"approval no longer required: what-if verified (decision 12)"},
		{"legacy revision without verdict", f, staleCandidate(map[string]any{}), 0,
			"approval no longer required: what-if verified"},
		{"verified all along", f, staleCandidate(verified), 3,
			"approval no longer required: the standing policy now authorizes it (decision 3)"},
		{"no candidate", f, nil, 0,
			"approval no longer required: the standing policy now authorizes it"},
		{"still unverified", analyzer.Finding{Detail: unverified},
			staleCandidate(unverified), 0,
			"approval no longer required: the standing policy now authorizes it"},
	}
	for _, c := range cases {
		if got := staleApprovalReason(c.f, c.cand, c.decision); got != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOperatorRejectionRequiresApproval(t *testing.T) {
	f := analyzer.Finding{Detail: map[string]any{"what_if_verdict": "verified"}}
	got := requireApprovalAfterRejection(f, 17)
	reason, _ := got.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(reason, "rejected") || !strings.Contains(reason, "17") {
		t.Fatalf("approval reason = %q, want the rejection named", reason)
	}
	if _, mutated := f.Detail[analyzer.DetailApprovalRequired]; mutated {
		t.Fatal("the caller's detail was mutated")
	}
	rejected := withDetail(optimizerFinding("verified"), got.Detail)
	if !hasApprovalGuardrail(findingRequest(rejected, false)) {
		t.Fatal("a rejected verified index must carry the approval guardrail")
	}
}

func withDetail(f analyzer.Finding, detail map[string]any) analyzer.Finding {
	f.Detail = detail
	return f
}

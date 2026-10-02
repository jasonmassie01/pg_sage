package srebench

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Post-test audit additions: the grader's evidence checks on their own.
// A claim resolves only when every evidence id it cites is evidence of
// the investigation whose hash still verifies; any reference outside the
// investigation is a scope finding.

func storedEvidence(id sre.UUID, payload string, tamper bool) sre.Evidence {
	sum := sha256.Sum256([]byte(payload))
	if tamper {
		sum[0] ^= 0xff
	}
	return sre.Evidence{ID: id, Payload: []byte(payload), SHA256: sum[:]}
}

func narrated(claims ...[]sre.UUID) sre.Summary {
	n := &sre.Narrative{Label: sre.NarrativeLabel}
	for _, ids := range claims {
		n.Claims = append(n.Claims, sre.NarrativeClaim{Text: "claim", EvidenceIDs: ids})
	}
	return sre.Summary{Narrative: n}
}

func TestClaimRefs_ResolveOnlyVerifyingOwnEvidence(t *testing.T) {
	ev := []sre.Evidence{storedEvidence("e1", `{"a":1}`, false),
		storedEvidence("e2", `{"b":2}`, true)}
	sum := narrated([]sre.UUID{"e1"}, []sre.UUID{"e1", "e2"}, []sre.UUID{"e9"},
		[]sre.UUID{})
	claims, resolved := claimRefs(sum, ev)
	if claims != 4 || resolved != 1 {
		t.Fatalf("claims %d resolved %d, want 4 and 1 (tampered, foreign and uncited "+
			"claims do not resolve)", claims, resolved)
	}
	if c, r := claimRefs(sre.Summary{}, ev); c != 0 || r != 0 {
		t.Fatalf("no narrative: %d %d", c, r)
	}
}

func TestScopeFindings_ForeignReferences(t *testing.T) {
	ev := []sre.Evidence{storedEvidence("e1", "{}", false)}
	sum := narrated([]sre.UUID{"e1", "other-db-evidence"})
	sum.Observed = []sre.Fact{{EvidenceID: "e1", Text: "ok"}, {EvidenceID: "x2"}}
	sum.ModelProbe = &sre.ModelProbe{EvidenceID: "x3"}
	hs := []sre.HypothesisRecord{{Node: "idle_in_tx_holder",
		Support:    []sre.Fact{{EvidenceID: "e1"}},
		Contradict: []sre.Fact{{EvidenceID: "x4"}}}}
	got := scopeFindings(sum, hs, ev)
	if len(got) != 4 {
		t.Fatalf("findings %q, want 4", got)
	}
	for _, want := range []string{"hypothesis idle_in_tx_holder cites evidence x4",
		"observed fact cites evidence x2", "narrative claim cites evidence other-db-evidence",
		"model probe cites evidence x3"} {
		if !strings.Contains(strings.Join(got, "|"), want) {
			t.Errorf("findings %q lack %q", got, want)
		}
	}
	if got := scopeFindings(sre.Summary{}, nil, ev); len(got) != 0 {
		t.Fatalf("an empty diagnosis has findings %q", got)
	}
}

func TestRankedFirst(t *testing.T) {
	if got := rankedFirst(sre.Summary{}); got != "" {
		t.Fatalf("no ranking -> %q", got)
	}
	sum := sre.Summary{ModelRanking: &sre.ModelRanking{Nodes: []string{"a", "b"}}}
	if got := rankedFirst(sum); got != "a" {
		t.Fatalf("ranking -> %q", got)
	}
}

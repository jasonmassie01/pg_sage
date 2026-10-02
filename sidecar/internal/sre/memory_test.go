package sre

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Incident memory (AI-SRE-SPEC §4 R2): similar past investigations of the
// same database are found by trigger/family and overlapping graph nodes
// and evidence features, and offered to the model turn as bounded,
// redacted, fenced context that can never be cited as evidence.

func TestDiagnosisFeatures_MatchStoredRecordFeatures(t *testing.T) {
	_, _, d := idleChainFixture(t)
	got := diagnosisFeatures(d)
	for _, want := range []string{"root:idle_in_tx_holder", "open:idle_in_tx_holder",
		"open:ddl_lock_queue"} {
		if !contains(got, want) {
			t.Errorf("features %v lack %s", got, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("features %v are not sorted and unique", got)
		}
	}
	c := conclusionOf(d)
	if stored := recordFeatures(c.Hypotheses, c.Summary); !reflect.DeepEqual(stored, got) {
		t.Fatalf("stored features %v != live features %v; a past incident and the "+
			"current one must be described the same way", stored, got)
	}
	_, _, unknown := unknownLockFixture(t)
	if f := diagnosisFeatures(unknown); !contains(f, "missing:lock_graph") {
		t.Fatalf("inconclusive features %v lack the missing lock_graph probe", f)
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func TestSimilarity_Jaccard(t *testing.T) {
	cases := []struct {
		a, b []string
		want float64
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, 1},
		{[]string{"a"}, []string{"b"}, 0},
		{nil, nil, 0},
		{[]string{"a"}, nil, 0},
		{[]string{"a", "b"}, []string{"b", "c"}, 0.33},
	}
	for _, c := range cases {
		if got := similarity(c.a, c.b); got != c.want {
			t.Errorf("similarity(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRankSimilar_OrdersAndBounds(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mk := func(id string, age time.Duration, f ...string) SimilarIncident {
		return SimilarIncident{InvestigationID: UUID(id), ConcludedAt: now.Add(-age),
			features: f}
	}
	target := []string{"open:a", "root:a", "open:b"}
	items := []SimilarIncident{
		mk("00000000-0000-4000-8000-000000000001", time.Hour, "open:z"),
		mk("00000000-0000-4000-8000-000000000002", 3*time.Hour, "open:a", "root:a", "open:b"),
		mk("00000000-0000-4000-8000-000000000003", time.Hour, "open:a"),
		mk("00000000-0000-4000-8000-000000000004", 2*time.Hour, "open:a"),
		mk("00000000-0000-4000-8000-000000000005", 2*time.Hour, "open:a"),
	}
	got := rankSimilar(items, target, 3)
	var ids []string
	for _, s := range got {
		ids = append(ids, string(s.InvestigationID)[35:])
	}
	if strings.Join(ids, ",") != "2,3,4" {
		t.Fatalf("ranked %v, want 2 (identical), 3 (newer tie), 4 (id tie-break)", ids)
	}
	if got[0].Score != 1 || got[1].Score != 0.33 {
		t.Fatalf("scores %v/%v, want 1 and 0.33", got[0].Score, got[1].Score)
	}
	if len(rankSimilar(items, target, 0)) != 0 || len(rankSimilar(nil, target, 3)) != 0 {
		t.Fatal("a zero limit or no candidates must rank nothing")
	}
}

func pastIncidents(n int) []SimilarIncident {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var out []SimilarIncident
	for i := 0; i < n; i++ {
		s := SimilarIncident{Label: SimilarLabel, InvestigationID: NewUUID(),
			TriggerKind: TriggerLock, Family: "lock_blocking", State: StateConcluded,
			Root: "idle_in_tx_holder", Open: []string{"idle_in_tx_holder", "ddl_lock_queue"},
			RuledOut: []string{"hot_row_contention"}, ConcludedAt: now.Add(-50 * time.Hour),
			Score: 0.8}
		if i == 0 {
			s.Outcome = &Outcome{Verdict: OutcomeConfirmed, RecordedAt: now}
		}
		if i == 1 {
			s.Outcome = &Outcome{Verdict: OutcomeRefuted, ActualNode: "ddl_lock_queue",
				RecordedAt: now}
		}
		out = append(out, s)
	}
	return out
}

func TestMemoryBlock_BoundedLabeledAndDeterministic(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	items := pastIncidents(10)
	for i := range items {
		items[i].Open = append(items[i].Open, strings.Repeat("x", 300))
	}
	block := memoryBlock(items, at)
	if len(block) > maxMemoryBytes {
		t.Fatalf("memory block is %d bytes, over %d", len(block), maxMemoryBytes)
	}
	if strings.Count(block, "\nP") > maxMemoryExamples || strings.Contains(block, "P4:") {
		t.Fatalf("memory block has more than %d examples:\n%s", maxMemoryExamples, block)
	}
	for _, want := range []string{"P1:", "idle_in_tx_holder", "operator confirmed",
		"operator refuted", "ddl_lock_queue", "2d before"} {
		if !strings.Contains(block, want) {
			t.Errorf("memory block lacks %q:\n%s", want, block)
		}
	}
	if memoryBlock(items, at) != block {
		t.Fatal("memory block is not deterministic")
	}
	if memoryBlock(nil, at) != "" {
		t.Fatal("no past incidents must give no block")
	}
	unverified := pastIncidents(3)[2:]
	if b := memoryBlock(unverified, at); !strings.Contains(b, "unverified") {
		t.Fatalf("an incident without an outcome is not marked unverified:\n%s", b)
	}
}

func TestMemoryBlock_RedactsStoredText(t *testing.T) {
	items := pastIncidents(1)
	items[0].Root = "password=hunter2"
	block := memoryBlock(items, time.Now())
	if strings.Contains(block, "hunter2") {
		t.Fatalf("memory block leaks a credential:\n%s", block)
	}
}

func TestReviewPrompt_FencesPastIncidentsAsContext(t *testing.T) {
	inv, ev, d := idleChainFixture(t)
	scope := newReviewScope(d, ev, false)
	plain := reviewMessages(inv, scope, "", true)
	if strings.Contains(plain[1].Content, "past_incidents") {
		t.Fatal("a turn without memory shows a past-incidents block")
	}
	scope.memory = memoryBlock(pastIncidents(2), time.Now())
	msgs := reviewMessages(inv, scope, "", true)
	user := msgs[1].Content
	start := strings.Index(user, `<data label="past_incidents">`)
	if start < 0 || !strings.Contains(user[start:], "P1:") {
		t.Fatalf("past incidents are not fenced as data:\n%s", user)
	}
	if !strings.Contains(strings.ToLower(user[:start]), "cannot be cited") {
		t.Fatal("the prompt does not say past incidents cannot be cited")
	}
	if !strings.Contains(msgs[0].Content, llm.UntrustedDataRule) {
		t.Fatal("the system prompt lacks the untrusted-data rule")
	}
}

func TestReviewCheck_PastIncidentsCannotBeCited(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	scope := newReviewScope(d, ev, false)
	scope.memory = memoryBlock(pastIncidents(1), time.Now())
	_, rej := scope.check(modelOutput{Ranking: []string{"idle_in_tx_holder",
		"ddl_lock_queue"}, Claims: []Claim{{Text: "The same holder pattern as before.",
		EvidenceIDs: []string{"P1"}}}})
	rejection(t, rej, RejectUnknownEvidence)
}

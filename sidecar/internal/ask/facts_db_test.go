package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/facts"
)

// Owner decision (2026-10-04): Ask Sage may PROPOSE a binding fact. It
// stays proposed until a person confirms it, under the same restrictions
// as the other writes: only for callers who may propose, at most once per
// question, and citing evidence this run read. Ask Sage never confirms,
// rejects or re-opens a fact.

func factArgs(subject string, evidence ...string) string {
	ids := `"` + strings.Join(evidence, `","`) + `"`
	if len(evidence) == 0 {
		ids = ""
	}
	return fmt.Sprintf(`{"type":"owned_by_app_migrations","subject_kind":"index",`+
		`"subject":%q,"rationale":"recreated after every drop","evidence_ids":[%s]}`,
		subject, ids)
}

func TestFact_ProposedWithCitedEvidenceAndNeverConfirmed(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.idx_thesis", Title: "Index dropped and back"})
	evID := "finding:" + itoa(id)
	m := newFakeLLM(t, calls(toolCall{"get_finding", `{"id":` + itoa(id) + `}`}),
		calls(toolCall{"propose_fact", factArgs("public.idx_thesis", evID)}),
		calls(toolCall{"propose_fact", factArgs("public.idx_other", evID)}),
		answer(func(string) answerArgs {
			return answerArgs{NotObserved: []string{"A person confirms the fact."}}
		}))
	a := mustAsk(t, f.service(f.deps(m)), operator, "Is idx_thesis owned by the app?", "")
	got, err := facts.NewStore(f.pool).List(f.ctx, facts.Filter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("facts = %+v (%v)", got, err)
	}
	fact := got[0]
	if fact.Status != facts.StatusProposed || fact.Source != facts.SourceModel ||
		fact.ProposedBy != "ask:user:1" || fact.DecidedBy != "" ||
		fact.Subject != "public.idx_thesis" {
		t.Fatalf("fact = %+v", fact)
	}
	if len(fact.Evidence) == 0 || !strings.Contains(fact.Evidence[0].Ref, evID) {
		t.Fatalf("the fact does not cite the evidence read: %+v", fact.Evidence)
	}
	if a.Transcript.Rejected[agentloop.RejectInvalidArgs] != 1 {
		t.Fatalf("a second fact in one question was not refused: %+v", a.Transcript.Rejected)
	}
	if len(a.Actions) != 1 || a.Actions[0].Kind != ActionFact ||
		a.Actions[0].Status != ActionProposedFact || a.Actions[0].ID != itoa(fact.ID) ||
		a.Actions[0].EvidenceID != "fact:"+itoa(fact.ID) {
		t.Fatalf("actions = %+v", a.Actions)
	}
}

func TestFact_EvidenceMustHaveBeenReadThisRun(t *testing.T) {
	f := newFixture(t)
	d := f.deps(nil)
	s := f.service(d)
	for _, args := range []string{factArgs("public.idx_a"), factArgs("public.idx_a",
		"finding:999"), factArgs("public.idx_a", "E1"), `{"type":"test_fixture",` +
		`"subject_kind":"schema","subject":"t_*","evidence_ids":["x"],"confirm":true}`} {
		wantInvalid(t, s, operator, "propose_fact", args)
	}
	if n := f.count(`SELECT count(*) FROM sage.facts`); n != 0 {
		t.Fatalf("refused proposals wrote %d facts", n)
	}
}

func TestFact_ReadOnlyCallersAreNotOffered(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	if toolNames(s.newSession(viewer).tools())["propose_fact"] {
		t.Fatal("a viewer was offered propose_fact")
	}
	if !toolNames(s.newSession(agent).tools())["propose_fact"] {
		t.Fatal("an agent with the propose scope was not offered propose_fact")
	}
}

func TestFact_ProtectedAndRejectedSubjectsAreRefused(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.t", Title: "x"})
	evID := "finding:" + itoa(id)
	st := facts.NewStore(f.pool)
	rejected, _, err := st.Propose(f.ctx, facts.Proposal{Type: facts.TypeAppMigrations,
		Kind: facts.KindIndex, Subject: "public.idx_rej", Source: facts.SourceDetector,
		Evidence: []facts.Citation{{Kind: "catalog", Ref: "x", Detail: "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decide(f.ctx, rejected.ID, facts.Decision{Confirm: false,
		Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"protected": `{"type":"test_fixture","subject_kind":"schema","subject":"pg_catalog",` +
			`"evidence_ids":["` + evID + `"]}`,
		"rejected": factArgs("public.idx_rej", evID),
	}
	for name, args := range cases {
		s := f.service(f.deps(nil))
		ss := s.newSession(operator)
		if _, err := runSessionTool(t, ss, "get_finding", `{"id":`+itoa(id)+`}`); err != nil {
			t.Fatal(err)
		}
		out, err := runSessionTool(t, ss, "propose_fact", args)
		_, acts := ss.snapshot()
		if err != nil || out.Evidence != nil || len(acts) != 1 ||
			acts[0].Status != ActionRefused {
			t.Errorf("%s: out %+v err %v actions %+v", name, out, err, acts)
		}
	}
	if fact, err := st.Get(f.ctx, rejected.ID); err != nil ||
		fact.Status != facts.StatusRejected {
		t.Fatalf("Ask Sage changed a rejected fact: %+v (%v)", fact, err)
	}
}

// runSessionTool runs a tool of an existing session, so a write can rely
// on what the session read before.
func runSessionTool(t *testing.T, ss *session, name, args string) (agentloop.Output, error) {
	t.Helper()
	for _, tool := range ss.tools() {
		if tool.Name == name {
			return tool.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("tool %s is not offered", name)
	return agentloop.Output{}, nil
}

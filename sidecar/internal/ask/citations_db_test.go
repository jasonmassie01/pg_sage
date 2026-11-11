package ask

import (
	"context"
	"regexp"
	"testing"
)

// mcp-05 on real Postgres: one question reads the open findings twice
// with different limits (one durable id, findings:open, two reads). Each
// claim cites one read; the answer and the stored conversation keep two
// citations, each with the digest of the read it names.

// aliasesFor is every alias the prompt gave evidence with this id, in
// the order shown.
func aliasesFor(t *testing.T, body, evidenceID string) []string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(evidenceID) + `[\] ]`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(prompt(t, body), -1) {
		out = append(out, m[1])
	}
	if len(out) != 2 {
		t.Errorf("the prompt shows %d reads of %s, want 2:\n%s", len(out), evidenceID,
			prompt(t, body))
		return []string{"E0", "E0"}
	}
	return out
}

func TestAsk_ReReadOfOneListKeepsEachReadsDigest(t *testing.T) {
	f := newFixture(t)
	f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders"})
	f.finding(findingSeed{Object: "public.users", Title: "Missing index on users"})
	var al []string
	m := newFakeLLM(t, calls(toolCall{"list_findings", `{"limit":1}`},
		toolCall{"list_findings", `{"limit":2}`}),
		answer(func(body string) answerArgs {
			al = aliasesFor(t, body, "findings:open")
			return answerArgs{Claims: []claimArg{
				{Text: "The newest open finding is about public.users.",
					EvidenceIDs: []string{al[0]}},
				{Text: "The open findings include public.orders.",
					EvidenceIDs: []string{al[1]}}}}
		}))
	s := f.service(f.deps(m))
	a := mustAsk(t, s, viewer, "Which findings are open?", "")
	if a.Status != StatusAnswered || len(a.Statements) != 2 || a.Transcript == nil {
		t.Fatalf("answer = %+v", a)
	}
	steps := map[string]string{}
	for _, st := range a.Transcript.Steps {
		if st.Alias != "" {
			steps[st.Alias] = st.Digest
		}
	}
	if len(steps) != 2 || len(a.Citations) != 2 {
		t.Fatalf("steps %v citations %+v, want two reads cited", steps, a.Citations)
	}
	first, second := a.Citations[0], a.Citations[1]
	if first.ID != "findings:open" || second.ID != "findings:open" ||
		first.Digest == second.Digest || len(first.Digest) != 64 {
		t.Fatalf("citations = %+v, want two distinct digests of findings:open", a.Citations)
	}
	if first.Digest != steps[al[0]] || second.Digest != steps[al[1]] {
		t.Fatalf("citation digests %s / %s, want the reads' %v", first.Digest, second.Digest,
			steps)
	}
	thread, err := s.Thread(context.Background(), viewer, a.ConversationID)
	if err != nil || len(thread.Answers) != 1 || len(thread.Answers[0].Citations) != 2 ||
		thread.Answers[0].Citations[0].Digest != first.Digest ||
		thread.Answers[0].Citations[1].Digest != second.Digest {
		t.Fatalf("stored thread = %+v (%v)", thread, err)
	}
}

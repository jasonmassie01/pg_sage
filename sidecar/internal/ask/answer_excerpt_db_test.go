package ask

import (
	"strings"
	"testing"
)

// mcp-05b on real Postgres: one Ask run stores its conversation with each
// citation's excerpt in sage.ask_messages.answer, the excerpt is exactly
// the text the model read (its digest is the citation's), the actor's
// thread returns it, and deleting the conversation removes it.

func TestAsk_StoresTheCitationExcerptWithTheAnswer(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders",
		Detail: map[string]any{"seq_scans": 4200}})
	m := newFakeLLM(t, calls(toolCall{"get_finding", `{"id":` + itoa(id) + `}`}),
		answer(func(body string) answerArgs {
			return answerArgs{Claims: []claimArg{{Text: "Finding " + itoa(id) +
				" reports 4200 sequential scans on public.orders.",
				EvidenceIDs: []string{aliasFor(t, body, "finding:"+itoa(id))}}}}
		}))
	s := f.service(f.deps(m))
	a := mustAsk(t, s, viewer, "Why is public.orders slow?", "")
	if a.Status != StatusAnswered || len(a.Citations) != 1 {
		t.Fatalf("answer = %+v", a)
	}
	c := a.Citations[0]
	if c.Excerpt == "" || digestOf(c.Excerpt) != c.Digest || c.ExcerptTruncated ||
		!strings.Contains(c.Excerpt, "Missing index on orders") {
		t.Fatalf("citation = %+v, want the full text the model read", c)
	}
	var stored string
	err := f.pool.QueryRow(f.ctx, `SELECT answer->'citations'->0->>'excerpt'
		FROM sage.ask_messages WHERE conversation_id = $1::uuid`,
		a.ConversationID).Scan(&stored)
	if err != nil || stored != c.Excerpt {
		t.Fatalf("stored excerpt = %q (%v), want %q", stored, err, c.Excerpt)
	}
	thread, err := s.Thread(f.ctx, viewer, a.ConversationID)
	if err != nil || len(thread.Answers) != 1 ||
		thread.Answers[0].Citations[0].Excerpt != c.Excerpt {
		t.Fatalf("thread = %+v (%v), want the excerpt back", thread, err)
	}
	if _, err := s.Thread(f.ctx, operator, a.ConversationID); err == nil {
		t.Fatal("another actor read the conversation and its excerpts")
	}
	f.exec(`DELETE FROM sage.ask_conversations WHERE id = $1::uuid`, a.ConversationID)
	left := f.count(`SELECT count(*) FROM sage.ask_messages
		WHERE conversation_id = $1::uuid`, a.ConversationID)
	if left != 0 {
		t.Fatalf("%d messages left after deleting the conversation", left)
	}
}

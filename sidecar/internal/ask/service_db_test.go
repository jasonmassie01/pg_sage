package ask

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// Ask Sage end to end against a real sage schema and a fake
// OpenAI-compatible server playing scripted tool-call transcripts: a
// grounded answer, "not observed", and what happens when the model
// misbehaves (hallucinated citations, ungrounded numbers) or the provider
// does (429, timeout, malformed replies, no tool calling).

func mustAsk(t *testing.T, s *Service, c Caller, q, conv string) Answer {
	t.Helper()
	a, err := s.Ask(context.Background(), c, Request{Question: q, ConversationID: conv})
	if err != nil {
		t.Fatalf("Ask(%q): %v", q, err)
	}
	return a
}

func TestAsk_GroundedAnswerCitesTheEvidenceItRead(t *testing.T) {
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
	if a.Status != StatusAnswered || len(a.Statements) != 1 || a.ConversationID == "" ||
		a.ID == 0 || a.Question != "Why is public.orders slow?" || a.Database != "testdb" {
		t.Fatalf("answer = %+v", a)
	}
	st := a.Statements[0]
	if len(st.Citations) != 1 || st.Citations[0] != "finding:"+itoa(id) ||
		!strings.Contains(st.Text, "4200") {
		t.Fatalf("statement = %+v", st)
	}
	if len(a.Citations) != 1 || a.Citations[0].APIPath != "/api/v1/findings/"+itoa(id)+
		"?database=testdb" || len(a.Citations[0].Digest) != 64 {
		t.Fatalf("citations = %+v", a.Citations)
	}
	if a.Tokens <= 0 || a.Transcript == nil || a.Transcript.ToolCalls != 1 {
		t.Fatalf("tokens %d transcript %+v", a.Tokens, a.Transcript)
	}
	thread, err := s.Thread(context.Background(), viewer, a.ConversationID)
	if err != nil || len(thread.Answers) != 1 || thread.Answers[0].Text != a.Text ||
		thread.Answers[0].Statements[0].Citations[0] != "finding:"+itoa(id) {
		t.Fatalf("thread = %+v (%v)", thread, err)
	}
	b, _ := s.BudgetStatus(context.Background(), viewer)
	if b.UserUsed != int64(a.Tokens) || b.DatabaseUsed != int64(a.Tokens) {
		t.Fatalf("budget %+v does not match the answer's %d tokens", b, a.Tokens)
	}
}

func TestAsk_NotObservedIsAValidAnswer(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, calls(toolCall{"list_findings", `{}`}),
		answer(func(string) answerArgs {
			return answerArgs{NotObserved: []string{"No open finding mentions public.invoices."}}
		}))
	a := mustAsk(t, f.service(f.deps(m)), viewer, "Anything wrong with invoices?", "")
	if a.Status != StatusNotObserved || len(a.Statements) != 0 ||
		len(a.NotVerified) != 1 || !strings.Contains(a.Text, "public.invoices") {
		t.Fatalf("answer = %+v", a)
	}
}

func TestAsk_HallucinatedAndUngroundedClaimsAreDropped(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders",
		Detail: map[string]any{"seq_scans": 4200}})
	m := newFakeLLM(t, calls(toolCall{"get_finding", `{"id":` + itoa(id) + `}`}),
		answer(func(body string) answerArgs {
			alias := aliasFor(t, body, "finding:"+itoa(id))
			return answerArgs{Claims: []claimArg{
				{Text: "Finding " + itoa(id) + " is about public.orders.",
					EvidenceIDs: []string{alias}},
				// A number no timestamp, id or count in the evidence can contain: "37"
				// matched a timestamp minute on CI (PR #127).
				{Text: "The index will make it 8675309 times faster.", EvidenceIDs: []string{alias}},
				{Text: "Autovacuum is broken.", EvidenceIDs: []string{"E9"}},
				{Text: "Replication lag is high.", EvidenceIDs: nil},
			}}
		}))
	a := mustAsk(t, f.service(f.deps(m)), viewer, "What is going on?", "")
	if a.Status != StatusAnswered || len(a.Statements) != 1 ||
		!strings.Contains(a.Statements[0].Text, "is about public.orders") {
		t.Fatalf("statements = %+v", a.Statements)
	}
	reasons := map[string]bool{}
	for _, d := range a.Dropped {
		reasons[d.Reason] = true
	}
	for _, r := range []string{agentloop.DropUngrounded, agentloop.DropUnknownEvidence,
		agentloop.DropUncited} {
		if !reasons[r] {
			t.Errorf("no %s drop in %+v", r, a.Dropped)
		}
	}
	for _, bad := range []string{"8675309 times", "Autovacuum is broken", "Replication lag"} {
		if strings.Contains(a.Text, bad) {
			t.Errorf("dropped claim %q reached the answer text", bad)
		}
	}
	if !strings.Contains(a.Text, "3 statements were dropped") {
		t.Fatalf("text does not say what was dropped: %q", a.Text)
	}
}

func TestAsk_ConversationCarriesHistoryAsData(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders"})
	first := func(body string) answerArgs {
		return answerArgs{Claims: []claimArg{{Text: "Finding " + itoa(id) +
			" is about public.orders.", EvidenceIDs: []string{aliasFor(t, body,
			"finding:"+itoa(id))}}}}
	}
	m := newFakeLLM(t, calls(toolCall{"get_finding", `{"id":` + itoa(id) + `}`}),
		answer(first), answer(func(string) answerArgs {
			return answerArgs{NotObserved: []string{"Nothing new was read."}}
		}))
	s := f.service(f.deps(m))
	a1 := mustAsk(t, s, viewer, "What is finding about?", "")
	a2 := mustAsk(t, s, viewer, "And what should I do?", a1.ConversationID)
	if a2.ConversationID != a1.ConversationID {
		t.Fatalf("follow-up started conversation %s", a2.ConversationID)
	}
	p := prompt(t, m.body(t, 2))
	if !strings.Contains(p, "What is finding about?") || !strings.Contains(p,
		"is about public.orders") || !strings.Contains(p, `<data label="conversation">`) {
		t.Fatalf("history not in the follow-up prompt as data:\n%s", p)
	}
	thread, err := s.Thread(context.Background(), viewer, a1.ConversationID)
	if err != nil || len(thread.Answers) != 2 || thread.Answers[0].ID != a1.ID ||
		thread.Answers[1].ID != a2.ID || thread.Conversation.Messages != 2 ||
		thread.Conversation.Title != "What is finding about?" {
		t.Fatalf("thread = %+v (%v)", thread, err)
	}
	list, err := s.Conversations(context.Background(), viewer, 10)
	if err != nil || len(list) != 1 || list[0].ID != a1.ConversationID {
		t.Fatalf("conversations = %+v (%v)", list, err)
	}
	if other, _ := s.Conversations(context.Background(), operator, 10); len(other) != 0 {
		t.Fatalf("another user sees %d conversations", len(other))
	}
}

func TestAsk_ConversationsArePrivateAndBounded(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing."}}
	}))
	s := f.service(f.deps(m))
	a := mustAsk(t, s, viewer, "Hello?", "")
	if _, err := s.Ask(context.Background(), operator, Request{Question: "x",
		ConversationID: a.ConversationID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's conversation: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Thread(context.Background(), operator, a.ConversationID); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("another user's thread: err = %v", err)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if _, err := s.Ask(context.Background(), viewer, Request{Question: "x",
			ConversationID: id}); !errors.Is(err, ErrNotFound) {
			t.Errorf("conversation %q: err = %v, want ErrNotFound", id, err)
		}
	}
	f.exec(`INSERT INTO sage.ask_messages (conversation_id, question, answer, status)
		SELECT $1::uuid, 'q', '{}'::jsonb, 'answered' FROM generate_series(1, $2)`,
		a.ConversationID, MaxMessagesPerConversation-1)
	if _, err := s.Ask(context.Background(), viewer, Request{Question: "one more",
		ConversationID: a.ConversationID}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "full") {
		t.Fatalf("full conversation: err = %v", err)
	}
	if m.calls() != 1 {
		t.Fatalf("refused asks reached the model: %d calls", m.calls())
	}
}

func TestAsk_InvalidQuestions(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t)
	s := f.service(f.deps(m))
	for _, q := range []string{"", "   \n\t", strings.Repeat("é", MaxQuestionRunes+1)} {
		if _, err := s.Ask(context.Background(), viewer, Request{Question: q}); !errors.Is(err,
			ErrInvalid) {
			t.Errorf("question of %d runes: err = %v", len([]rune(q)), err)
		}
	}
	if _, err := s.Ask(context.Background(), Caller{}, Request{Question: "x"}); !errors.Is(err,
		ErrInvalid) {
		t.Errorf("anonymous caller: err = %v", err)
	}
	if m.calls() != 0 || f.count(`SELECT count(*) FROM sage.ask_conversations`) != 0 {
		t.Fatal("an invalid ask reached the model or the store")
	}
}

func TestAsk_DisabledAndNoModel(t *testing.T) {
	f := newFixture(t)
	d := f.deps(nil)
	d.Config.Enabled = false
	if _, err := f.service(d).Ask(context.Background(), viewer,
		Request{Question: "x"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: err = %v", err)
	}
	a := mustAsk(t, f.service(f.deps(nil)), viewer, "Why is it slow?", "")
	if a.Status != StatusNoModel || a.Tokens != 0 || !strings.Contains(a.Text, "LLM") {
		t.Fatalf("no model = %+v", a)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_budget_day`); n != 0 {
		t.Fatalf("no model spent budget: %d rows", n)
	}
}

func TestAsk_ProviderFailuresAreTypedStops(t *testing.T) {
	cases := map[string]struct {
		script []fakeReply
		stop   string
	}{
		"429": {[]fakeReply{statusReply(http.StatusTooManyRequests)},
			agentloop.StopRateLimited},
		"malformed": {[]fakeReply{contentReply(fixedText("{")), contentReply(fixedText("")),
			contentReply(fixedText("not json"))}, agentloop.StopMalformed},
		"timeout": {[]fakeReply{slowReply(1500*time.Millisecond, answer(func(string) answerArgs {
			return answerArgs{}
		}))}, agentloop.StopTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			m := newFakeLLM(t, tc.script...)
			d := f.deps(m)
			d.Budget = DefaultBudget()
			d.Budget.StepTimeout = 400 * time.Millisecond
			a := mustAsk(t, f.service(d), viewer, "Why?", "")
			if a.Status != StatusIncomplete || a.Stop != tc.stop || len(a.Statements) != 0 {
				t.Fatalf("answer = %+v", a)
			}
			if f.count(`SELECT count(*) FROM sage.ask_messages WHERE status = 'incomplete'`) != 1 {
				t.Fatal("the incomplete answer was not stored")
			}
		})
	}
}

func fixedText(s string) func(string) string { return func(string) string { return s } }

func TestAsk_MalformedThenRecovered(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, contentReply(fixedText("")), contentReply(fixedText(`{"tool": "x"`)),
		answer(func(string) answerArgs { return answerArgs{NotObserved: []string{"Nothing."}} }))
	a := mustAsk(t, f.service(f.deps(m)), viewer, "Why?", "")
	if a.Status != StatusNotObserved || a.Transcript.Rejected[agentloop.RejectEmptyReply] != 1 ||
		a.Transcript.Rejected[agentloop.RejectMalformedReply] != 1 {
		t.Fatalf("answer = %+v transcript %+v", a, a.Transcript)
	}
}

func TestAsk_ProviderWithoutToolCallingFallsBackToJSON(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders"})
	m := newFakeLLM(t, statusReply(http.StatusBadRequest),
		contentReply(fixedText(`{"tool":"get_finding","args":{"id":`+itoa(id)+`}}`)),
		contentReply(func(body string) string {
			return "```json\n{\"tool\":\"answer\",\"args\":" + answerArgs{Claims: []claimArg{{
				Text:        "Finding " + itoa(id) + " is about public.orders.",
				EvidenceIDs: []string{aliasFor(t, body, "finding:"+itoa(id))}}}}.json() +
				"}\n```"
		}))
	a := mustAsk(t, f.service(f.deps(m)), viewer, "What is wrong?", "")
	if a.Status != StatusAnswered || a.Transcript.Protocol != agentloop.ProtocolJSON {
		t.Fatalf("answer = %+v protocol %s", a, a.Transcript.Protocol)
	}
	if strings.Contains(m.body(t, 1), `"tools"`) {
		t.Fatal("the JSON-protocol request still offered native tools")
	}
}

func TestAsk_DeadlineStopsTheRunAndStillStoresTheAnswer(t *testing.T) {
	f := newFixture(t)
	slow := slowReply(3*time.Second, answer(func(string) answerArgs { return answerArgs{} }))
	m := newFakeLLM(t, slow, slow)
	s := f.service(f.deps(m))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	a, err := s.Ask(ctx, viewer, Request{Question: "Why?"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("the ask outlived its caller's deadline: %s", time.Since(start))
	}
	if a.Status != StatusIncomplete || a.ID == 0 {
		t.Fatalf("answer = %+v", a)
	}
}

func TestAsk_CancelledCallerStoresNothing(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, slowReply(2*time.Second, answer(func(string) answerArgs {
		return answerArgs{}
	})))
	s := f.service(f.deps(m))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err := s.Ask(ctx, viewer, Request{Question: "Why?"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_messages`); n != 0 {
		t.Fatalf("a cancelled ask stored %d answers", n)
	}
}

func TestAsk_ConcurrentUsersKeepConsistentBudgets(t *testing.T) {
	f := newFixture(t)
	reply := answer(func(string) answerArgs { return answerArgs{NotObserved: []string{"No."}} })
	m := newFakeLLM(t, reply, reply, reply, reply, reply, reply)
	s := f.service(f.deps(m))
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		c := viewer
		if i%2 == 1 {
			c = operator
		}
		go func() {
			_, err := s.Ask(context.Background(), c, Request{Question: "Status?"})
			errs <- err
		}()
	}
	for i := 0; i < 6; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent ask: %v", err)
		}
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_messages`); n != 6 {
		t.Fatalf("%d answers stored, want 6", n)
	}
	if f.count(`SELECT tokens FROM sage.ask_budget_day WHERE actor = '*'`) != f.count(
		`SELECT sum(tokens) FROM sage.ask_budget_day WHERE actor <> '*'`) {
		t.Fatal("the database total is not the sum of the users' usage")
	}
}

func TestAsk_BudgetExhaustedBeforeAnyCall(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs { return answerArgs{} }))
	d := f.deps(m)
	d.Config.DailyTokensPerUser = 500
	a := mustAsk(t, f.service(d), viewer, "Why?", "")
	if a.Status != StatusBudget || m.calls() != 0 || a.Stop != agentloop.StopBudget {
		t.Fatalf("answer = %+v after %d calls", a, m.calls())
	}
	if !strings.Contains(a.Text, "budget") {
		t.Fatalf("text = %q", a.Text)
	}
}

func TestAsk_BudgetExhaustedMidRun(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders"})
	big := func(w http.ResponseWriter, body string) {
		writeCompletionUsage(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": "c1", "type": "function",
				"function": map[string]any{"name": "get_finding",
					"arguments": `{"id":` + itoa(id) + `}`}}}}, 49_000)
	}
	m := newFakeLLM(t, big, answer(func(string) answerArgs { return answerArgs{} }))
	d := f.deps(m)
	// The per-question cap is raised so the run reaches the daily budget
	// rather than its own token cap.
	d.Config.DailyTokensPerUser, d.Config.MaxTokensPerQuestion = 50_000, 200_000
	s := f.service(d)
	a := mustAsk(t, s, viewer, "Why?", "")
	if a.Status != StatusBudget || m.calls() != 1 {
		t.Fatalf("answer = %+v after %d calls", a, m.calls())
	}
	b, _ := s.BudgetStatus(context.Background(), viewer)
	if b.UserUsed != 49_000 {
		t.Fatalf("usage = %+v, want the reported 49000", b)
	}
	// The next question is refused before any model call.
	again := mustAsk(t, s, viewer, "And now?", "")
	if again.Status != StatusBudget || m.calls() != 1 {
		t.Fatalf("second ask = %+v after %d calls", again, m.calls())
	}
}

func TestAsk_BudgetIsSeparateFromTheLLMClientBudget(t *testing.T) {
	// The LLM client's own daily budget is shared with the investigator and
	// the tuning agent; Ask Sage's refusal happens before the client is
	// called, so it spends none of theirs.
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs { return answerArgs{} }))
	client := m.client()
	d := f.deps(nil)
	d.Model, d.Config.DailyTokensPerUser = client, 1
	a := mustAsk(t, f.service(d), viewer, "Why?", "")
	if a.Status != StatusBudget || client.TokensUsedToday() != 0 {
		t.Fatalf("answer = %+v, client used %d", a, client.TokensUsedToday())
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_budget_day WHERE tokens > 0`); n != 0 {
		t.Fatalf("a refused reservation charged %d rows", n)
	}
}

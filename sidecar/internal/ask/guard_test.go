package ask

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// Owner decision 3: Ask Sage can open an investigation or queue a typed
// proposal, and nothing else: it can never execute, approve, confirm
// facts or bypass the gate, whatever the user or retrieved text asks.
// These tests pin that structurally: the exact tool set per caller, and
// the package's imports and calls.

var readTools = []string{"describe_table", "explain", "get_action", "get_finding",
	"investigations", "list_actions", "list_approvals", "list_facts", "list_findings",
	"list_incidents", "top_queries", "trust_ledger"}

func fullDeps(f *fixture) Deps {
	d := f.deps(nil)
	d.Investigations, d.Trust = &fakeInvestigations{}, fakeTrust(`{}`)
	d.Queries = fakeQueries{}
	d.Proposer, d.Starter = &fakeProposer{}, &fakeStarter{}
	return d
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestGuard_ReadCallersGetOnlyReadTools(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	got := sortedNames(toolNames(s.newSession(viewer).tools()))
	if strings.Join(got, ",") != strings.Join(readTools, ",") {
		t.Fatalf("viewer tools = %v, want %v", got, readTools)
	}
}

func TestGuard_ProposersGetExactlyThreeWriteTools(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	for _, c := range []Caller{operator, agent} {
		names := toolNames(s.newSession(c).tools())
		want := append(append([]string{}, readTools...), "open_investigation", "propose_action",
			"propose_fact")
		sort.Strings(want)
		if got := sortedNames(names); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s tools = %v, want %v", c.Actor, got, want)
		}
	}
}

func TestGuard_NoToolCanExecuteApproveOrConfirm(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	forbidden := []string{"exec", "run_sql", "approve", "reject", "confirm", "decide",
		"apply", "drop", "alter", "set_", "grant", "rollback", "cancel", "terminate"}
	for _, tool := range s.newSession(operator).tools() {
		for _, word := range forbidden {
			if strings.Contains(tool.Name, word) {
				t.Errorf("tool %s looks like a mutation (%s)", tool.Name, word)
			}
		}
	}
	if FinalTool != "answer" {
		t.Fatalf("final tool = %s", FinalTool)
	}
}

// The package may not reach any execution or approval path: it imports
// neither the executor, the policy gate, the API nor MCP, and calls no
// method by those names. Writes go only through the Proposer and
// InvestigationStarter interfaces the wiring supplies.
func TestGuard_PackageCannotReachExecutionOrApproval(t *testing.T) {
	banned := []string{"/internal/executor", "/internal/policy", "/internal/api",
		"/internal/mcp", "/internal/store"}
	bannedCalls := map[string]bool{"Authorize": true, "Apply": true, "Execute": true,
		"Approve": true, "Decide": true, "DecideFact": true, "ExecuteConcrete": true,
		"RequestExecution": true, "Confirm": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, b := range banned {
				if strings.HasSuffix(path, b) {
					t.Errorf("%s imports %s", name, path)
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && bannedCalls[sel.Sel.Name] {
				t.Errorf("%s refers to %s at %s", name, sel.Sel.Name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("checked %d files; the guard is not looking at the package", checked)
	}
}

// The loop refuses more tools than the LLM client accepts (llm.MaxTools);
// the fullest tool set, a proposer's with every source, must fit.
func TestGuard_FullToolsetFitsTheLoop(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	cfg := agentloop.Config{Task: "q", Tools: s.newSession(operator).tools(),
		Final: finalTool(), Budget: DefaultBudget(), Protocol: agentloop.ProtocolAuto}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the full tool set does not fit the loop: %v", err)
	}
}

// offeredTools is the tool names one recorded model request offered,
// without the final answer tool.
func offeredTools(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, tool := range decodeRequest(t, body).Tools {
		if tool.Function.Name != FinalTool {
			out = append(out, tool.Function.Name)
		}
	}
	sort.Strings(out)
	return out
}

func notObservedReply() fakeReply {
	return answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing about that was observed."}}
	})
}

// Explain a case (mcp-01): a read-only request is answer-only whoever
// asks. The write tools are not offered, the system prompt does not invite
// a proposal, and the stored answer says it was read-only.
func TestGuard_ReadOnlyRequestOffersOnlyReadTools(t *testing.T) {
	for _, c := range []Caller{viewer, operator, agent} {
		t.Run(c.Actor, func(t *testing.T) {
			f := newFixture(t)
			m := newFakeLLM(t, notObservedReply())
			d := fullDeps(f)
			d.Model = m.client()
			s := f.service(d)
			a, err := s.Ask(f.ctx, c, Request{Question: "Explain this case", ReadOnly: true})
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			body := m.body(t, 0)
			if got := offeredTools(t, body); strings.Join(got, ",") !=
				strings.Join(readTools, ",") {
				t.Fatalf("read-only %s was offered %v, want %v", c.Actor, got, readTools)
			}
			if strings.Contains(prompt(t, body), "you may open one investigation") {
				t.Fatal("the read-only system prompt invites a proposal")
			}
			if !a.ReadOnly || len(a.Actions) != 0 {
				t.Fatalf("answer read_only=%v actions=%+v", a.ReadOnly, a.Actions)
			}
			thread, err := s.Thread(f.ctx, c, a.ConversationID)
			if err != nil || len(thread.Answers) != 1 || !thread.Answers[0].ReadOnly {
				t.Fatalf("stored answer does not record read_only: %+v (%v)", thread, err)
			}
		})
	}
}

// State transition: read_only is per question. The next question in the
// same conversation without it is offered the operator's write tools
// again, and its answer is not marked read-only.
func TestGuard_ReadOnlyIsPerQuestion(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, notObservedReply(), notObservedReply(), notObservedReply())
	d := fullDeps(f)
	d.Model = m.client()
	s := f.service(d)
	first := mustAsk(t, s, operator, "Why is it slow?", "")
	second, err := s.Ask(f.ctx, operator, Request{Question: "Explain it only",
		ConversationID: first.ConversationID, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Ask: %v", err)
	}
	third := mustAsk(t, s, operator, "Now propose the fix", first.ConversationID)
	want := append(append([]string{}, readTools...), "open_investigation", "propose_action",
		"propose_fact")
	sort.Strings(want)
	for i, w := range [][]string{want, readTools, want} {
		if got := offeredTools(t, m.body(t, i)); strings.Join(got, ",") !=
			strings.Join(w, ",") {
			t.Fatalf("question %d offered %v, want %v", i+1, got, w)
		}
	}
	if first.ReadOnly || !second.ReadOnly || third.ReadOnly {
		t.Fatalf("read_only flags = %v %v %v", first.ReadOnly, second.ReadOnly,
			third.ReadOnly)
	}
}

// No concurrent access test: read_only is a value of one request and
// Caller.For returns a copy; no state is shared between questions.

// Nil/zero: the zero Request is not read-only, so older clients that omit
// the field keep today's behaviour (operators may propose).
func TestGuard_CallerForKeepsTheCallerUnlessReadOnly(t *testing.T) {
	if got := operator.For(Request{}); got != operator {
		t.Fatalf("zero request changed the caller: %+v", got)
	}
	got := agent.For(Request{ReadOnly: true})
	if got.MayPropose || got.Actor != agent.Actor {
		t.Fatalf("read-only agent caller = %+v", got)
	}
	if got := viewer.For(Request{ReadOnly: true}); got != viewer {
		t.Fatalf("read-only viewer = %+v", got)
	}
}

// mcp-02: the loop tells the model to cite only by aliases, so every
// tool that takes evidence says so, and none asks for durable ids.
func TestGuard_CitingToolsAskForAliases(t *testing.T) {
	s := &Service{d: Deps{Investigations: &fakeInvestigations{}, Trust: fakeTrust(`{}`),
		Queries: fakeQueries{}, Proposer: &fakeProposer{}, Starter: &fakeStarter{}}}
	citing := 0
	for _, tl := range s.newSession(operator).tools() {
		if strings.Contains(tl.Description, "evidence ids (e.g.") {
			t.Errorf("%s asks for durable evidence ids: %q", tl.Name, tl.Description)
		}
		if strings.Contains(string(tl.Parameters), `"evidence_ids"`) {
			citing++
			if !strings.Contains(tl.Description, "aliases (E1, E2, ...)") {
				t.Errorf("%s takes evidence_ids but does not ask for aliases: %q", tl.Name,
					tl.Description)
			}
		}
	}
	if citing == 0 {
		t.Fatal("no citing tool found; propose_fact takes evidence_ids")
	}
}

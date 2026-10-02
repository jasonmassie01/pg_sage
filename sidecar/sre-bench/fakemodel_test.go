package srebench

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The fake adversarial model (fake mode of the LLM-on arm): an in-process
// OpenAI-compatible server that answers the investigator's model
// contract validly but against the graph (it ranks the graph's last open
// hypothesis first, asks for a next probe whenever one is offered and
// narrates claims citing the prompt's real evidence ids) and, on a
// deterministic subset of calls seeded by the scenario id, returns the
// failure modes the investigator handles: fenced JSON, an unknown node,
// an ungrounded number and HTTP 429.

// samplePrompt follows the investigator's prompt format.
const samplePrompt = "Investigation: lock_blocking incident (trigger lock), causal graph " +
	"causal-v2.\n<data label=\"subject\">\nincident 1 / pid 4242\n</data>\n" +
	"Graph result: root cause idle_in_tx_holder (conclusive).\n" +
	"Open hypotheses (rank all of these):\n" +
	"- idle_in_tx_holder [root_cause, graph score 0.85]: idle-in-transaction holder. x\n" +
	"- ddl_lock_queue [contributing, graph score 0.85]: DDL queued. y\n" +
	"- sage_own_action [unproven, graph score 0.10]: pg_sage's own change. z\n" +
	"Ruled out (do not rank):\n" +
	"- hot_row_contention [ruled_out, graph score 0.00]: hot-row contention. w\n" +
	"Evidence (cite by id):\n<data label=\"evidence\">\n" +
	"E1 [lock_graph ok]: root blocker pid 4242 is idle in transaction; it blocks 2 sessions\n" +
	"E2 [prepared_xacts empty]: no prepared transactions in this database\n" +
	"E3 [lock_chains error] statement_timeout\n" +
	"missing: lock_chains error statement_timeout\n</data>\n" +
	"Catalog probes you may propose as next_probe (id: args):\n" +
	"- lock_graph: {}\n" +
	"- backend_identity: {\"pid\": integer, \"backend_start\": RFC 3339 timestamp}\n" +
	"- sage_actions: {} or {\"window_seconds\": 60-604800}\n"

func TestParseFakePrompt(t *testing.T) {
	p := parseFakePrompt(samplePrompt)
	if strings.Join(p.open, ",") != "idle_in_tx_holder,ddl_lock_queue,sage_own_action" {
		t.Fatalf("open = %v", p.open)
	}
	if strings.Join(p.probes, ",") != "lock_graph,sage_actions" {
		t.Fatalf("probes taking no args = %v", p.probes)
	}
	if len(p.evidence) != 3 || p.evidence[0].alias != "E1" ||
		p.evidence[0].probe != "lock_graph" || !strings.Contains(p.evidence[0].text, "4242") ||
		p.evidence[2].alias != "E3" {
		t.Fatalf("evidence = %+v", p.evidence)
	}
	noMenu := parseFakePrompt(strings.Split(samplePrompt, "Catalog probes")[0] +
		"No probes are offered: omit next_probe.\n")
	if len(noMenu.probes) != 0 || len(noMenu.open) != 3 {
		t.Fatalf("without a menu: %+v", noMenu)
	}
	if empty := parseFakePrompt("nothing here"); len(empty.open)+len(empty.evidence) != 0 {
		t.Fatalf("unrelated text parsed as %+v", empty)
	}
}

func TestAdversarialReview_AgainstTheGraphButValid(t *testing.T) {
	r := adversarialReview(parseFakePrompt(samplePrompt), seedOf("lock-idle"))
	if strings.Join(r.Ranking, ",") != "sage_own_action,ddl_lock_queue,idle_in_tx_holder" {
		t.Fatalf("ranking = %v, want the graph's order reversed", r.Ranking)
	}
	if r.NextProbe == nil || (r.NextProbe.Probe != "lock_graph" &&
		r.NextProbe.Probe != "sage_actions") || len(r.NextProbe.Args) != 0 ||
		r.NextProbe.Rationale == "" || strings.Contains(r.NextProbe.Rationale, "\n") {
		t.Fatalf("next probe = %+v", r.NextProbe)
	}
	if len(r.Claims) == 0 || len(r.Claims) > 5 {
		t.Fatalf("claims = %+v", r.Claims)
	}
	aliases := map[string]string{"E1": "4242 2", "E2": "", "E3": ""}
	grounded := false
	for _, c := range r.Claims {
		for _, id := range c.EvidenceIDs {
			if _, ok := aliases[id]; !ok {
				t.Fatalf("claim cites %q, not an evidence id of the prompt", id)
			}
		}
		for _, n := range regexp.MustCompile(`\b\d+\b`).FindAllString(
			regexp.MustCompile(`\bE\d+\b`).ReplaceAllString(c.Text, ""), -1) {
			if !strings.Contains(aliases[c.EvidenceIDs[0]], n) {
				t.Fatalf("claim %q states %s, not in its evidence", c.Text, n)
			}
			grounded = true
		}
	}
	if !grounded {
		t.Fatalf("no claim quotes a number from its evidence: %+v", r.Claims)
	}
	noMenu := parseFakePrompt(strings.Split(samplePrompt, "Catalog probes")[0])
	if adversarialReview(noMenu, 1).NextProbe != nil {
		t.Fatal("asked for a probe that was not offered")
	}
}

func TestFakeModes_DeterministicPerScenarioAndCoverEveryFailure(t *testing.T) {
	seen := map[FakeMode]int{}
	differs := false
	first := FakeModeFor(Scenarios()[0].ID, 0)
	for _, sc := range Scenarios() {
		for call := 0; call < 4; call++ {
			m := FakeModeFor(sc.ID, call)
			if m != FakeModeFor(sc.ID, call) {
				t.Fatalf("%s call %d is not deterministic", sc.ID, call)
			}
			seen[m]++
		}
		differs = differs || FakeModeFor(sc.ID, 0) != first
	}
	for _, m := range []FakeMode{FakeAdversarial, FakeFenced, FakeUnknownNode,
		FakeUngrounded, FakeRateLimited} {
		if seen[m] == 0 {
			t.Errorf("mode %s never occurs over the bench's scenarios: %v", m, seen)
		}
	}
	if !differs || seen[FakeAdversarial] <= seen[FakeRateLimited] {
		t.Fatalf("modes do not vary by scenario or are mostly failures: %v", seen)
	}
}

func chatRequest(t *testing.T, tools bool) []byte {
	t.Helper()
	req := map[string]any{"model": FakeModelName, "messages": []map[string]any{
		{"role": "system", "content": "rules"}, {"role": "user", "content": samplePrompt}}}
	if tools {
		req["tools"] = []map[string]any{{"type": "function", "function": map[string]any{
			"name": "submit_review", "parameters": map[string]any{"type": "object"}}}}
	}
	raw, _ := json.Marshal(req)
	return raw
}

type wireReply struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		Total int `json:"total_tokens"`
	} `json:"usage"`
}

// post sends one chat request to a fake for scenarioID, after skipping
// calls to reach the first call in mode want.
func post(t *testing.T, scenarioID string, want FakeMode, tools bool) (*http.Response,
	wireReply) {
	t.Helper()
	f := NewFakeModel(scenarioID)
	t.Cleanup(f.Close)
	for call := 0; FakeModeFor(scenarioID, call) != want; call++ {
		if call > 200 {
			t.Fatalf("%s never answers in mode %s", scenarioID, want)
		}
		f.skip()
	}
	resp, err := http.Post(f.URL()+"/chat/completions", "application/json",
		bytes.NewReader(chatRequest(t, tools)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out wireReply
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func TestFakeModel_HTTPReplies(t *testing.T) {
	const sc = "lock-idle"
	resp, out := post(t, sc, FakeAdversarial, true)
	calls := out.Choices[0].Message.ToolCalls
	if resp.StatusCode != http.StatusOK || len(calls) != 1 ||
		calls[0].Function.Name != "submit_review" ||
		!strings.Contains(calls[0].Function.Arguments, `"ranking"`) || out.Usage.Total <= 0 {
		t.Fatalf("tool reply: status %d %+v", resp.StatusCode, out)
	}
	_, out = post(t, sc, FakeAdversarial, false)
	if c := out.Choices[0].Message.Content; !strings.HasPrefix(c, "{") ||
		len(out.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("content reply without tools: %+v", out)
	}
	_, out = post(t, sc, FakeFenced, false)
	if c := out.Choices[0].Message.Content; !strings.HasPrefix(c, "```json") {
		t.Fatalf("fenced reply: %q", c)
	}
	_, out = post(t, sc, FakeUnknownNode, false)
	if !strings.Contains(out.Choices[0].Message.Content, "not_a_graph_node") {
		t.Fatalf("unknown-node reply: %+v", out)
	}
	_, out = post(t, sc, FakeUngrounded, false)
	if !strings.Contains(out.Choices[0].Message.Content, "987654") {
		t.Fatalf("ungrounded reply: %+v", out)
	}
	if resp, _ := post(t, sc, FakeRateLimited, false); resp.StatusCode !=
		http.StatusTooManyRequests {
		t.Fatalf("rate-limited reply: status %d", resp.StatusCode)
	}
}

func TestFakeModel_BadRequestAndCalls(t *testing.T) {
	f := NewFakeModel("lock-idle")
	defer f.Close()
	resp, err := http.Post(f.URL()+"/chat/completions", "application/json",
		strings.NewReader("{nope"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || f.Calls() != 1 {
		t.Fatalf("status %d calls %d, want 400 and the call counted", resp.StatusCode,
			f.Calls())
	}
}

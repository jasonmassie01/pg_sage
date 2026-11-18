package modelpass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// Fixtures: a model-tier pack of four unused indexes, one drop candidate
// per index, a scripted model that answers each call from a function and
// records what it was sent, and a recorder of stored runs. Nothing here
// reaches a provider.

var indexNames = []string{"t_a", "t_b", "t_c", "t_d"}

// modelPack is the model tier of a pack with index facts
// index/idx/public.t_a .. t_d, of 8192, 16384, 24576 and 32768 bytes.
func modelPack(t *testing.T, database string) *workpack.Pack {
	t.Helper()
	var facts []workpack.IndexFact
	for i, name := range indexNames {
		facts = append(facts, workpack.IndexFact{ID: "index/idx/public." + name,
			Schema: "public", Table: "t", Index: name, Method: "btree",
			Definition: "CREATE INDEX " + name + " ON public.t USING btree (a)",
			SizeBytes:  int64(8192 * (i + 1)), Valid: true, Scans: 0, TableWrites: "unknown",
			DuplicateOf: []string{}, PrefixOf: []string{}})
	}
	local, err := reviewtest.Build(reviewtest.Spec{Database: database,
		Window:  reviewtest.Window(90),
		Indexes: &workpack.Indexes{ScansStatus: "ok", Indexes: facts}})
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	tier, err := workpack.ForModel(local, workpack.ModelOptions{})
	if err != nil {
		t.Fatalf("derive the model tier: %v", err)
	}
	p, err := workpack.Decode(tier.Body, tier.Manifest)
	if err != nil {
		t.Fatalf("open the model tier: %v", err)
	}
	return p
}

// dropOf is the drop candidate of an index of indexNames.
func dropOf(name string) candidate.Candidate {
	c := candidate.Candidate{Generator: "indexes", Dimension: "indexes", Kind: "unused",
		Target: "public." + name, ActionType: "drop_unused_index",
		ForwardSQL:  `DROP INDEX CONCURRENTLY "public"."` + name + `";`,
		RollbackSQL: "CREATE INDEX " + name + " ON public.t USING btree (a);",
		EvidenceIDs: []string{"index/idx/public." + name}, Reversible: true,
		Approval: candidate.ApprovalAutomatic, Reason: "no scans in 90 days",
		Prediction: candidate.Prediction{Metric: "index_bytes", Direction: "decrease",
			Tier: candidate.TierNone, Source: "pack"}}
	c.ID = candidate.NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL)
	return c
}

func fourDrops() []candidate.Candidate {
	out := make([]candidate.Candidate, 0, len(indexNames))
	for _, n := range indexNames {
		out = append(out, dropOf(n))
	}
	return out
}

func testGrade() *rubric.Grade {
	return &rubric.Grade{RubricVersion: "pg_sage-rubric/1", Overall: "C",
		Dimensions: []rubric.DimensionGrade{{Dimension: "indexes", Title: "Indexes",
			Letter: "C", Score: 60, Coverage: 100}}}
}

func input(t *testing.T, database string) Input {
	t.Helper()
	return Input{RunID: 7, Database: database, Pack: modelPack(t, database),
		Candidates: fourDrops(), Grade: testGrade()}
}

// aliasOf is the alias under which in's seed holds evidence id.
func aliasOf(t *testing.T, in Input, id string) string {
	t.Helper()
	seed, _, err := buildSeed(in)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i, e := range seed {
		if e.ID == id {
			return fmt.Sprintf("E%d", i+1)
		}
	}
	t.Fatalf("%s is not in the seed", id)
	return ""
}

type call struct {
	ctx  context.Context
	msgs []llm.Message
}

// scripted answers call n with reply(n, msgs); it is safe for parallel runs.
type scripted struct {
	mu    sync.Mutex
	calls []call
	reply func(n int, msgs []llm.Message) (llm.ToolResult, error)
}

func (m *scripted) ChatWithTools(ctx context.Context, msgs []llm.Message,
	_ []llm.ToolSpec, _ llm.ToolOptions) (llm.ToolResult, error) {
	m.mu.Lock()
	n := len(m.calls)
	m.calls = append(m.calls, call{ctx: ctx, msgs: append([]llm.Message(nil), msgs...)})
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return llm.ToolResult{}, err
	}
	return m.reply(n, msgs)
}

func (m *scripted) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// answer is a model's submit_review.
type answer struct {
	Ranking    []rankItem      `json:"ranking"`
	Links      []linkItem      `json:"links,omitempty"`
	Hypotheses []hypothesis    `json:"hypotheses,omitempty"`
	Report     []reportItem    `json:"report,omitempty"`
	Extra      json.RawMessage `json:"sql,omitempty"`
}

func claim(text string, aliases ...string) agentloop.Claim {
	return agentloop.Claim{Text: text, EvidenceIDs: aliases}
}

// keepRanked keeps the named indexes in the given order, each with one
// grounded claim on its index fact.
func keepRanked(t *testing.T, in Input, names ...string) answer {
	t.Helper()
	var a answer
	for i, n := range names {
		fact := "index/idx/public." + n
		a.Ranking = append(a.Ranking, rankItem{CandidateID: dropOf(n).ID, Rank: i + 1,
			Decision: report.DecisionKeep, Attack: "it has 0 scans",
			Claims: []agentloop.Claim{claim("public."+n+" had 0 scans in the window",
				aliasOf(t, in, fact))}})
	}
	return a
}

func native(a any) llm.ToolResult {
	args, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	return llm.ToolResult{ToolCalls: []llm.ToolCall{{ID: "c1", Name: finalName,
		Arguments: args}}, PromptTokens: 1000, CompletionTokens: 200}
}

// always answers every call with a.
func always(a any) *scripted {
	return &scripted{reply: func(int, []llm.Message) (llm.ToolResult, error) {
		return native(a), nil
	}}
}

// failing fails every call with err.
func failing(err error) *scripted {
	return &scripted{reply: func(int, []llm.Message) (llm.ToolResult, error) {
		return llm.ToolResult{}, err
	}}
}

// memStore records runs in memory; err fails every write.
type memStore struct {
	mu   sync.Mutex
	runs []RunRecord
	err  error
}

func (s *memStore) RecordRun(_ context.Context, r RunRecord) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, r)
	return nil
}

func (s *memStore) all() []RunRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RunRecord(nil), s.runs...)
}

func settings(runs int, consensus string) Settings {
	return Settings{Mode: "auto", Runs: runs, Consensus: consensus}
}

func deps(m agentloop.Model, store Recorder) Deps {
	return Deps{Model: m, ModelName: "test-model", Store: store,
		Budget: agentloop.Budget{MaxSteps: 4, MaxCalls: 4, MaxCost: 4, Wall: 10 * time.Second,
			MaxTokens: 100000, StepTokens: 2000, StepTimeout: 5 * time.Second},
		Protocol: agentloop.ProtocolNative}
}

var errTest = errors.New("scripted failure")

func ids(r []report.Ranked) string {
	var out []string
	for _, x := range r {
		out = append(out, strings.TrimPrefix(x.Target, "public."))
	}
	return strings.Join(out, ",")
}

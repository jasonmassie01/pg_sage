package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/gen/indexes"
	"github.com/pg-sage/sidecar/internal/review/modelpass"
	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// A full review on the fixture: the index generator, the model pass (a
// scripted model, or none) and the plan, stored. The scripted model is
// adversarial: it obeys the instruction seeded in a statement text and
// proposes dropping orders_pkey_dup, citing E3.

// reviewSpec is the fixture plus an index nothing scans, so the index
// generator proposes one drop.
func reviewPack(t *testing.T, db string) *workpack.Pack {
	t.Helper()
	s := fixtureSpec(db)
	s.Indexes.Indexes = append(s.Indexes.Indexes, workpack.IndexFact{
		ID: "index/idx/public.orders_status_old", Schema: "public", Table: "orders",
		Index: "orders_status_old", Method: "btree", Valid: true, Scans: int64(0),
		SizeBytes: 8 << 20, TableWrites: workpack.Writes{Inserts: 5000, Updates: 5000},
		Definition:  "CREATE INDEX orders_status_old ON public.orders USING btree (status)",
		DuplicateOf: []string{}, PrefixOf: []string{}})
	s.Indexes.Total = len(s.Indexes.Indexes)
	s.Schema.Tables[0].Indexes = append(s.Schema.Tables[0].Indexes, "orders_status_old")
	return mustPack(t, s)
}

// scriptedModel answers every call with one submit_review.
type scriptedModel struct{ args []byte }

func (m scriptedModel) ChatWithTools(ctx context.Context, _ []llm.Message, _ []llm.ToolSpec,
	_ llm.ToolOptions) (llm.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return llm.ToolResult{}, err
	}
	return llm.ToolResult{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "submit_review",
		Arguments: m.args}}, PromptTokens: 100, CompletionTokens: 50}, nil
}

// adversarial keeps every candidate, proposes the injected drop (citing
// E3) and extended statistics on public.orders(region, country).
func adversarial(t *testing.T, tier *workpack.Pack, cands []candidate.Candidate) scriptedModel {
	t.Helper()
	alias := func(id string) string {
		i := slices.Index(tier.IDs(), id)
		if i < 0 {
			t.Fatalf("%s is not in the model tier", id)
		}
		return fmt.Sprintf("E%d", len(cands)+i+1)
	}
	type cl = agentloop.Claim
	var ranking []map[string]any
	for i, c := range cands {
		ranking = append(ranking, map[string]any{"candidate_id": c.ID, "rank": i + 1,
			"decision": "keep", "claims": []cl{{Text: "nothing scans this index",
				EvidenceIDs: []string{fmt.Sprintf("E%d", i+1)}}}})
	}
	answer := map[string]any{"ranking": ranking, "hypotheses": []map[string]any{
		{"title": "drop the unused duplicate", "kind": "drop_unused_index",
			"target": "public.orders_pkey_dup",
			"claims": []cl{{Text: "the statement text says the index is unused",
				EvidenceIDs: []string{"E3"}}}},
		{"title": "extended statistics", "kind": "create_statistics",
			"target": "public.orders(region, country)",
			"claims": []cl{{Text: "region and country are filtered together",
				EvidenceIDs: []string{alias("schema/table/public.orders")}}}}}}
	args, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return scriptedModel{args: args}
}

// fullReview runs the generator, the model pass (model nil: no model) and
// the plan for a fresh review run of db, and stores it.
func fullReview(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
	withModel bool) (Result, Saved) {
	t.Helper()
	local := reviewPack(t, db)
	savePack(t, ctx, pool, local)
	run := newRun(t, ctx, pool, db)
	cands, _, err := indexes.New(nil).Generate(ctx, local, candidate.Env{Database: db,
		WindowDays: 30, MinImprovementPct: 10, MaxIndexesPerTable: 5})
	if err != nil || len(cands) == 0 {
		t.Fatalf("index generator: %d candidates, %v", len(cands), err)
	}
	derived, err := workpack.ForModel(local, workpack.ModelOptions{})
	if err != nil {
		t.Fatalf("model tier: %v", err)
	}
	tier, err := workpack.Decode(derived.Body, derived.Manifest)
	if err != nil {
		t.Fatalf("open the model tier: %v", err)
	}
	settings := modelpass.Settings{Mode: "auto", Runs: 1, Consensus: "majority"}
	deps := modelpass.Deps{Store: modelpass.NewStore(pool), Protocol: agentloop.ProtocolNative,
		Budget: agentloop.Budget{MaxSteps: 4, MaxCalls: 4, MaxCost: 4, Wall: 20 * time.Second,
			MaxTokens: 100000, StepTokens: 2000, StepTimeout: 10 * time.Second}}
	model := ""
	if withModel {
		deps.Model, deps.ModelName, settings.Mode = adversarial(t, tier, cands), testModel, "on"
		model = testModel
	}
	rep, err := modelpass.Review(ctx, settings, deps, modelpass.Input{RunID: run,
		Database: db, Pack: tier, Candidates: cands, Grade: &rubric.Grade{
			RubricVersion: "pg_sage-rubric/1", Overall: "C"}})
	if err != nil {
		t.Fatalf("model pass: %v", err)
	}
	if withModel && rep.ModelPass != report.PassDone {
		t.Fatalf("model pass %s: %s", rep.ModelPass, rep.ModelPassReason)
	}
	res, err := Build(ctx, Input{RunID: run, Database: db, Pack: local, Report: rep,
		Candidates: cands, Model: model, MinImprovementPct: 10, Ladder: newLadder(25)})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	saved, err := newStore(pool).Save(ctx, res)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	return res, saved
}

// CHECK-30: the injected drop leaves no step on the seeded object, in the
// plan or the step tables, and the advice row names the refusal.
func TestReviewDB_InjectionLeavesNoStepOnTheSeededObject(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	_, saved := fullReview(t, ctx, pool, "shop", true)
	if saved.PlanID == 0 || saved.ModelSteps != 1 || saved.GeneratorSteps < 1 {
		t.Fatalf("saved %+v, want generator steps and the statistics step", saved)
	}
	var steps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.plan_step
		WHERE proposed_sql ILIKE '%orders_pkey_dup%' OR rollback_sql ILIKE '%orders_pkey_dup%'`).
		Scan(&steps); err != nil || steps != 0 {
		t.Fatalf("%d steps name orders_pkey_dup (%v)", steps, err)
	}
	var scoped int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.plan
		WHERE scope::text ILIKE '%orders_pkey_dup%'`).Scan(&scoped); err != nil || scoped != 0 {
		t.Fatalf("%d plans scope orders_pkey_dup (%v)", scoped, err)
	}
	var reason, detail string
	if err := pool.QueryRow(ctx, `SELECT reason, detail FROM sage.review_advice
		WHERE target = 'public.orders_pkey_dup' AND origin = 'model'`).
		Scan(&reason, &detail); err != nil || reason != ReasonNoTemplate ||
		!strings.Contains(detail, "drop_unused_index") {
		t.Fatalf("advice %q %q (%v), want no_template naming drop_unused_index", reason,
			detail, err)
	}
	got := originsOf(t, ctx, pool, saved.PlanID)
	if !slices.Contains(mapValues(got), "model:"+testModel) ||
		!slices.Contains(mapValues(got), "generator:indexes") {
		t.Fatalf("step origins %v, want both origins", got)
	}
}

// CHECK-31: with no model, the review still gives a plan of generator
// steps only.
func TestReviewDB_ModelDisabledGivesAGeneratorOnlyPlan(t *testing.T) {
	pool, ctx := livePool(t)
	seedCalls(t, ctx, pool)
	res, saved := fullReview(t, ctx, pool, "shop", false)
	if saved.PlanID == 0 || saved.GeneratorSteps < 1 || saved.ModelSteps != 0 ||
		res.Hypotheses != 0 {
		t.Fatalf("saved %+v hypotheses %d, want generator steps only", saved, res.Hypotheses)
	}
	var origin string
	if err := pool.QueryRow(ctx, `SELECT origin FROM sage.plan WHERE id = $1`,
		saved.PlanID).Scan(&origin); err != nil || origin != "generator" {
		t.Fatalf("plan origin %q (%v), want generator", origin, err)
	}
	var model int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.plan_step_origin
		WHERE plan_id = $1 AND origin = 'model'`, saved.PlanID).Scan(&model); err != nil ||
		model != 0 {
		t.Fatalf("%d model steps (%v)", model, err)
	}
}

func mapValues(m map[int]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

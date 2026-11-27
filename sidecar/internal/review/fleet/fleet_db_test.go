package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/trigger"
)

// On PostgreSQL: the control database's ledger (sage.review_fleet_pass and
// sage.review_fleet_member) and a full tiered pass over five fixture
// databases (meta mode: one control database, five members), with an
// httptest OpenAI-compatible model.

func TestStoreDB_LedgerFollowsTheStateRules(t *testing.T) {
	pool, ctx := livePool(t, "ledger")
	s := NewStore(pool, "meta")
	pass, err := s.StartPass(ctx, "2026-11", 3)
	if err != nil || pass <= 0 {
		t.Fatalf("start: %d %v", pass, err)
	}
	rs := rollupsOf(synth(3, 1, 2))
	if err := s.SaveRollups(ctx, pass, rs); err != nil {
		t.Fatalf("save rollups: %v", err)
	}
	os := DeterministicTriage(rs)
	if len(os) != 2 {
		t.Fatalf("outliers %+v", os)
	}
	if err := s.MarkTriaged(ctx, pass, os); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if err := s.MarkTriaged(ctx, pass, os[:1]); !errors.Is(err, ErrState) {
		t.Fatalf("re-triage: %v, want ErrState", err)
	}
	d := Deep{Database: "db0001", PassID: pass, Tokens: 1234, Period: "2026-11"}
	if err := s.MarkScheduled(ctx, d, 41); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if err := s.MarkScheduled(ctx, d, 41); !errors.Is(err, ErrState) {
		t.Fatalf("schedule twice: %v, want ErrState", err)
	}
	checkScheduleAndCarry(t, s, pass)
	checkAwaitAndReview(t, s, pass)
	if err := s.FinishPass(ctx, pass, PassSummary{Status: PassDone, Members: 3,
		Outliers: 2, Scheduled: 1, Deferred: 1, DeepTokens: 1234,
		Triage: Triage{Mode: ModeDeterministic}}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if st, ok, err := s.LastPass(ctx, "2026-11"); err != nil || !ok || st != PassDone {
		t.Fatalf("last pass = %q %v %v", st, ok, err)
	}
	if _, ok, err := NewStore(pool, "other").LastPass(ctx, "2026-11"); err != nil || ok {
		t.Fatalf("another fleet scope saw the pass (%v)", err)
	}
}

func checkScheduleAndCarry(t *testing.T, s *Store, pass int64) {
	t.Helper()
	ctx := t.Context()
	if err := s.MarkDeferred(ctx, pass, "db0002", "2026-12"); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if got, err := s.ScheduledIn(ctx, "2026-11"); err != nil || len(got) != 1 ||
		got[0] != "db0001" {
		t.Fatalf("scheduled in 2026-11 = %v (%v), want db0001", got, err)
	}
	carried, err := s.Carried(ctx, "2026-11")
	if err != nil || len(carried) != 0 {
		t.Fatalf("carried into 2026-11 = %+v (%v), want none", carried, err)
	}
	carried, err = s.Carried(ctx, "2026-12")
	if err != nil || len(carried) != 1 || carried[0].Database != "db0002" ||
		carried[0].PassID != pass || !carried[0].Carried || carried[0].Score <= 0 {
		t.Fatalf("carried into 2026-12 = %+v (%v), want db0002", carried, err)
	}
}

func checkAwaitAndReview(t *testing.T, s *Store, pass int64) {
	t.Helper()
	ctx := t.Context()
	await, err := s.AwaitingReview(ctx)
	if err != nil || len(await) != 1 || await[0].Database != "db0001" || await[0].Ref != 41 ||
		await[0].Tokens != 1234 {
		t.Fatalf("awaiting = %+v (%v)", await, err)
	}
	if err := s.MarkReviewed(ctx, pass, "db0001", 0); err != nil {
		t.Fatalf("review: %v", err)
	}
	if err := s.MarkReviewed(ctx, pass, "db0001", 0); !errors.Is(err, ErrState) {
		t.Fatalf("review twice: %v, want ErrState", err)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM sage.review_fleet_member
		WHERE pass_id = $1 AND database_name = 'db0001'`, pass).Scan(&state); err != nil ||
		state != string(StateReviewed) {
		t.Fatalf("stored state %q (%v)", state, err)
	}
}

func TestStoreDB_SpendByTierFromTheUsageLedger(t *testing.T) {
	pool, ctx := livePool(t, "spend")
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	_, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage (day, database_key, role, purpose,
		model, prompt_tokens, completion_tokens, calls, est_cost_usd) VALUES
		($1, 'fleet', 'general', 'review_triage', 'm', 1000, 100, 2, 0.5),
		($1, 'fleet', 'general', 'review_fleet', 'm', 200, 50, 1, NULL),
		($1, 'db1', 'review', 'review', 'm', 9000, 900, 3, 4.25),
		($1, 'db1', 'general', 'briefing', 'm', 7, 7, 1, 1)`, day)
	if err != nil {
		t.Fatalf("seed sage.llm_usage: %v", err)
	}
	got, err := NewStore(pool, "meta").Spend(ctx, day, day.AddDate(0, 1, 0))
	if err != nil || len(got) != 3 {
		t.Fatalf("spend %+v (%v), want the three review tiers", got, err)
	}
	tri, fl, deep := got[PurposeTriage], got[PurposeFleet], got[PurposeDeep]
	if tri.PromptTokens != 1000 || tri.Calls != 2 || tri.CostUSD == nil || *tri.CostUSD != 0.5 ||
		fl.CostUSD != nil || fl.CompletionTokens != 50 || deep.PromptTokens != 9000 {
		t.Fatalf("spend triage %+v fleet %+v deep %+v", tri, fl, deep)
	}
}

// modelServer answers a triage call by naming the members whose rollup
// line shows a grade drop, and a fleet call with a summary.
func modelServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		content := "Two members need a deep review."
		if len(req.Messages) == 2 && strings.Contains(req.Messages[0].Content, "outliers") {
			resp, _ := namesDrops(t).fn(req.Messages[0].Content, req.Messages[1].Content)
			content = resp.Content
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content}}},
			"usage": map[string]int{"prompt_tokens": 2000, "completion_tokens": 100,
				"total_tokens": 2100}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFleetDB_FullTieredPassOnFiveDatabases(t *testing.T) {
	control, ctx := livePool(t, "control")
	pools := map[string]*pgxpool.Pool{}
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("fx%d", i)
		p, _ := livePool(t, name)
		pools[name] = p
		first, second := 85.0, 85.0
		if i == 2 || i == 4 {
			first, second = 92, 61
		}
		gradedRun(t, ctx, p, name, first)
		gradedRun(t, ctx, p, name, second)
	}
	pick := func(db string) *pgxpool.Pool { return pools[db] }
	srv := modelServer(t)
	model := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "test-key",
		Model: "haiku-class", TimeoutSeconds: 5}, func(string, string, ...any) {})
	store := NewStore(control, "meta")
	runner := &Runner{Ledger: store, Source: &PoolSource{Pool: pick},
		Deep: &TriggerScheduler{Pool: pick}, Triage: model, Review: model,
		Settings: Settings{DeepMax: 30, Workers: 3}}
	res, err := runner.Run(ctx, []string{"fx1", "fx2", "fx3", "fx4", "fx5"})
	if err != nil || res.Triage.Mode != ModeModel || outlierNames(res.Triage.Outliers) !=
		"fx2,fx4" || len(res.Scheduled) != 2 || res.Fleet.SummarySource != ModeModel {
		t.Fatalf("pass %+v (%v)", res, err)
	}
	checkTriggers(t, pools, res.PassID)
	checkPassRow(t, store, res.PassID)
	consumeTrigger(t, pools["fx2"], "fx2")
	adv, err := runner.Advance(ctx)
	if err != nil || adv.Reviewed != 1 {
		t.Fatalf("advance %+v (%v), want fx2 reviewed", adv, err)
	}
	again, err := runner.Run(ctx, []string{"fx1", "fx2", "fx3", "fx4", "fx5"})
	if err != nil || len(again.Scheduled) != 0 {
		t.Fatalf("a second pass in the month scheduled %+v (%v): the trigger key dedupes",
			again.Scheduled, err)
	}
}

func checkTriggers(t *testing.T, pools map[string]*pgxpool.Pool, pass int64) {
	t.Helper()
	ctx := t.Context()
	for name, p := range pools {
		var n int
		var tokens float64
		err := p.QueryRow(ctx, `SELECT count(*), coalesce(max((detail->>'tokens')::float8), 0)
			FROM sage.review_trigger WHERE kind = 'fleet_outlier'
			AND (detail->>'pass_id')::bigint = $1`, pass).Scan(&n, &tokens)
		want := 0
		if name == "fx2" || name == "fx4" {
			want = 1
		}
		if err != nil || n != want || (want == 1 && tokens < DeepTokensPerReview/2) {
			t.Errorf("%s: %d fleet_outlier triggers with %v tokens (%v), want %d", name, n,
				tokens, err, want)
		}
	}
}

func checkPassRow(t *testing.T, s *Store, pass int64) {
	t.Helper()
	var status, mode string
	var tiers []byte
	err := s.pool.QueryRow(t.Context(), `SELECT status, triage_mode, tier_tokens
		FROM sage.review_fleet_pass WHERE id = $1`, pass).Scan(&status, &mode, &tiers)
	if err != nil || status != PassDone || mode != ModeModel {
		t.Fatalf("pass row %s %s (%v)", status, mode, err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(tiers, &got); err != nil {
		t.Fatalf("tier_tokens %s: %v", tiers, err)
	}
	num := func(tier, key string) float64 {
		v, _ := got[tier][key].(float64)
		return v
	}
	if num(PurposeTriage, "prompt_tokens") != 2000 || num(PurposeFleet, "calls") != 1 ||
		num(PurposeDeep, "allocated_tokens") < DeepTokensPerReview {
		t.Fatalf("tier_tokens %s, want triage, fleet and deep counts", tiers)
	}
}

// consumeTrigger plays the trigger loop: a review run consumes db's fleet
// trigger.
func consumeTrigger(t *testing.T, pool *pgxpool.Pool, db string) {
	t.Helper()
	ctx := t.Context()
	run := gradedRun(t, ctx, pool, db, 88)
	tag, err := pool.Exec(ctx, `UPDATE sage.review_trigger SET state = 'consumed',
		run_id = $1, done_at = now() WHERE kind = $2`, run, string(trigger.KindFleetOutlier))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("consume the fleet trigger of %s: %v", db, err)
	}
}

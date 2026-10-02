package srebench

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Plan regression fault programs: query_store histories with a plan_hash
// flip at the slowdown, a slowdown on an unchanged plan, the flip among
// other queries' histories (noise), a flip without a slowdown (decoy)
// and a steady query. Each scenario uses its own query ids.

type sample struct {
	ago   time.Duration
	calls int64
	total float64
	hash  string
}

// history is one query's query_store samples.
type history struct {
	qid     int64
	samples []sample
}

func planScenarios() []Scenario {
	flip := func(a, b string) []sample {
		return []sample{{30 * time.Minute, 0, 0, a}, {20 * time.Minute, 20, 10, a},
			{10 * time.Minute, 40, 210, b}}
	}
	same := []sample{{40 * time.Minute, 0, 0, "v1:c"}, {30 * time.Minute, 20, 20, "v1:c"},
		{20 * time.Minute, 40, 40, "v1:c"}, {10 * time.Minute, 60, 140, "v1:c"}}
	steady := func(h string) []sample {
		return []sample{{40 * time.Minute, 0, 0, h}, {30 * time.Minute, 20, 20, h},
			{20 * time.Minute, 40, 40, h}, {10 * time.Minute, 60, 61, h}}
	}
	// The noise: another query's much larger regression and a steady one.
	bigger := []sample{{30 * time.Minute, 0, 0, "v1:x"}, {20 * time.Minute, 20, 10, "v1:x"},
		{10 * time.Minute, 40, 410, "v1:x"}}
	flatFlip := []sample{{30 * time.Minute, 0, 0, "v1:e"}, {20 * time.Minute, 20, 10, "v1:e"},
		{10 * time.Minute, 40, 20.5, "v1:f"}}
	flipGold := Gold{Root: "plan_flip_regression"}
	return []Scenario{
		planScenario(1, "plan-flip", ClassPositive, flipGold, flip("v1:a", "v1:b")),
		planScenario(2, "plan-same-plan-slowdown", ClassPositive,
			Gold{Root: "same_plan_latency_regression"}, same),
		planScenario(3, "plan-flip-among-other-regressions", ClassNoise, flipGold,
			flip("v1:g", "v1:h"), bigger, steady("v1:i")),
		planScenario(4, "plan-flip-without-slowdown", ClassDecoy,
			Gold{Lookalike: "plan_flip_regression"}, flatFlip),
		planScenario(5, "plan-steady", ClassBenign, Gold{}, steady("v1:d")),
	}
}

// planBase spaces query ids by run, so concurrent runs never share one.
var planBase = 7_000_000_000_000_000_000 + time.Now().UnixNano()%1_000_000_000*100

// planScenario investigates the first history's query; the others are
// background queries. n numbers the scenario's query ids.
func planScenario(n int64, id, class string, gold Gold, subject []sample,
	others ...[]sample) Scenario {
	hs := []history{{qid: planBase + n*10, samples: subject}}
	for i, o := range others {
		hs = append(hs, history{qid: planBase + n*10 + int64(i) + 1, samples: o})
	}
	return Scenario{ID: id, Family: sre.TriggerPlan, Class: class, Gold: gold,
		Subject: fmt.Sprintf("queryid %d", hs[0].qid), Program: queryHistories(hs)}
}

func queryHistories(hs []history) program {
	qids := make([]int64, 0, len(hs))
	for _, h := range hs {
		qids = append(qids, h.qid)
	}
	return program{
		inject: func(ctx context.Context, e *Env) error {
			for _, h := range hs {
				for _, s := range h.samples {
					if _, err := e.Pool.Exec(ctx, `INSERT INTO sage.query_store
						(captured_at, queryid, calls, total_exec_time, mean_exec_time,
						 plan_hash)
						VALUES (now() - make_interval(secs => $1), $2, $3, $4, 0, $5)`,
						s.ago.Seconds(), h.qid, s.calls, s.total, s.hash); err != nil {
						return err
					}
				}
			}
			return nil
		},
		manifest: func(ctx context.Context, e *Env) error {
			for _, h := range hs {
				n, err := e.count(ctx,
					"SELECT count(*) FROM sage.query_store WHERE queryid = $1", h.qid)
				if err == nil && n != len(h.samples) {
					err = fmt.Errorf("query %d has %d samples, want %d", h.qid, n,
						len(h.samples))
				}
				if err != nil {
					return err
				}
			}
			return nil
		},
		recover: func(ctx context.Context, e *Env) error {
			_, err := e.Pool.Exec(ctx,
				"DELETE FROM sage.query_store WHERE queryid = ANY($1)", qids)
			return err
		},
	}
}

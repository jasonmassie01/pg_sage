package srebench

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Plan regression fault programs: query_store histories with a plan_hash
// flip at the slowdown, a slowdown on an unchanged plan, and a steady
// query. Each uses its own query id.

type sample struct {
	ago   time.Duration
	calls int64
	total float64
	hash  string
}

func planScenarios() []Scenario {
	flip := []sample{{30 * time.Minute, 0, 0, "v1:a"}, {20 * time.Minute, 20, 10, "v1:a"},
		{10 * time.Minute, 40, 210, "v1:b"}}
	same := []sample{{40 * time.Minute, 0, 0, "v1:c"}, {30 * time.Minute, 20, 20, "v1:c"},
		{20 * time.Minute, 40, 40, "v1:c"}, {10 * time.Minute, 60, 140, "v1:c"}}
	steady := []sample{{40 * time.Minute, 0, 0, "v1:d"}, {30 * time.Minute, 20, 20, "v1:d"},
		{20 * time.Minute, 40, 40, "v1:d"}, {10 * time.Minute, 60, 61, "v1:d"}}
	return []Scenario{
		planScenario("plan-flip", ClassPositive, Gold{Root: "plan_flip_regression"}, flip),
		planScenario("plan-same-plan-slowdown", ClassPositive,
			Gold{Root: "same_plan_latency_regression"}, same),
		planScenario("plan-steady", ClassBenign, Gold{}, steady),
	}
}

func planScenario(id, class string, gold Gold, history []sample) Scenario {
	qid := 7_000_000_000_000_000_000 + time.Now().UnixNano()%1_000_000_000
	qid += int64(len(id)) // distinct per scenario within one run
	return Scenario{ID: id, Family: sre.TriggerPlan, Class: class, Gold: gold,
		Subject: fmt.Sprintf("queryid %d", qid), Program: queryHistory(qid, history)}
}

func queryHistory(qid int64, history []sample) program {
	return program{
		inject: func(ctx context.Context, e *Env) error {
			for _, s := range history {
				if _, err := e.Pool.Exec(ctx, `INSERT INTO sage.query_store
					(captured_at, queryid, calls, total_exec_time, mean_exec_time, plan_hash)
					VALUES (now() - make_interval(secs => $1), $2, $3, $4, 0, $5)`,
					s.ago.Seconds(), qid, s.calls, s.total, s.hash); err != nil {
					return err
				}
			}
			return nil
		},
		manifest: func(ctx context.Context, e *Env) error {
			n, err := e.count(ctx, "SELECT count(*) FROM sage.query_store WHERE queryid = $1",
				qid)
			if err == nil && n != len(history) {
				err = fmt.Errorf("query %d has %d samples, want %d", qid, n, len(history))
			}
			return err
		},
		recover: func(ctx context.Context, e *Env) error {
			_, err := e.Pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid = $1", qid)
			return err
		},
	}
}

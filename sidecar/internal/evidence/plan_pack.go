package evidence

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// planStepKey is the identity key of a plan step's queue item (plan-02a):
// plan_step:<plan id>:<step id>.
var planStepKey = regexp.MustCompile(`^plan_step:([1-9][0-9]{0,18}):[1-9][0-9]{0,18}$`)

// planFiles are the plan files of an action pack, with the chain that
// proves each.
var planFiles = []struct {
	name, table string
	spec        auditchain.Spec
}{
	{"plan_events.json", "sage.plan_event", auditchain.PlanEvents},
	{"plan_model_records.json", "sage.plan_model_record", auditchain.PlanModelRecords},
}

// plans adds, for an action a plan step ran (plan-01b), the plan's events
// and the model passes it was built from, as plan_events.json and
// plan_model_records.json, with their chain proofs in plan_chains.json.
// An action no plan step ran gets none of these files.
func (b *builder) plans(ctx context.Context, actionID int64) error {
	planIDs, err := b.planIDs(ctx, actionID)
	if err != nil || len(planIDs) == 0 {
		return err
	}
	proofs := make([]chainProof, 0, len(planFiles))
	for _, f := range planFiles {
		q := rowQuery{name: f.name, table: f.table, where: "t.plan_id = ANY($1)", src: b.db,
			args: []any{planIDs}}
		_, exists, err := b.rows(ctx, q)
		if err != nil {
			return err
		}
		proof := chainProof{Chain: f.spec.Chain, Source: b.db.name,
			Note: "table not installed"}
		if exists {
			if proof, err = b.planProof(ctx, f.table, f.spec, planIDs); err != nil {
				return err
			}
		}
		proofs = append(proofs, proof)
	}
	return b.add("plan_chains.json", fileMeta{}, proofs)
}

// planProof proves the rows of table that belong to planIDs.
func (b *builder) planProof(ctx context.Context, table string, spec auditchain.Spec,
	planIDs []int64) (chainProof, error) {
	var ids []int64
	err := b.db.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT coalesce(array_agg(t.id
		ORDER BY t.id), '{}') FROM `+table+` t WHERE t.plan_id = ANY($1)`,
		planIDs).Scan(&ids)
	if err != nil {
		return chainProof{}, fmt.Errorf("evidence: %s of plans %v: %w", table, planIDs, err)
	}
	return b.rowsProof(ctx, b.db, spec, ids, "the plan has no "+spec.Chain+" links")
}

// planIDs lists the plans whose step's queue item ran action id, read from
// the item's identity key.
func (b *builder) planIDs(ctx context.Context, id int64) ([]int64, error) {
	var keys []string
	err := b.db.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT coalesce(array_agg(
		DISTINCT t.identity_key), '{}') FROM sage.action_queue t
		WHERE t.action_log_id = $1 AND t.identity_key LIKE 'plan_step:%'`, id).Scan(&keys)
	if err != nil {
		return nil, fmt.Errorf("evidence: plan steps of action %d: %w", id, err)
	}
	var out []int64
	for _, k := range keys {
		m := planStepKey.FindStringSubmatch(k)
		if m == nil {
			continue // not a plan step's key
		}
		plan, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue // beyond int64: no plan has this id
		}
		out = append(out, plan)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

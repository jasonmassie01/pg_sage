package changeplan

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NeedingPerson is one plan, or one step of a plan, waiting on a person:
// a plan proposed or halted, or a step awaiting its operator (plan-02d).
// StepOrdinal is 0 for the plan itself.
type NeedingPerson struct {
	PlanID      int64     `json:"plan_id"`
	Goal        string    `json:"goal"`
	Status      Status    `json:"status"`
	StepOrdinal int       `json:"step_ordinal,omitempty"`
	StepState   StepState `json:"step_state,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Since       time.Time `json:"since"`
}

// maxNeedingPerson bounds ListNeedingPerson.
const maxNeedingPerson = 200

// ListNeedingPerson returns what waits on a person, oldest first: plans
// proposed or halted, and steps awaiting their operator. It is the inbox's
// plan source (a halt is one inbox item).
func (s *Store) ListNeedingPerson(ctx context.Context) ([]NeedingPerson, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT p.id, p.goal, p.status, 0, '',
		'', p.updated_at FROM sage.plan p WHERE p.status IN ('proposed', 'halted')
		UNION ALL
		SELECT p.id, p.goal, p.status, st.ordinal, st.state, st.state_reason, p.updated_at
		  FROM sage.plan_step st JOIN sage.plan p ON p.id = st.plan_id
		 WHERE st.state = 'awaiting_operator' AND p.status NOT IN ('completed', 'reverted',
		       'rejected', 'voided', 'cancelled')
		ORDER BY 7, 1, 4 LIMIT $1`, maxNeedingPerson)
	if err != nil {
		return nil, fmt.Errorf("changeplan: list what needs a person: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (NeedingPerson, error) {
		var n NeedingPerson
		var status, state string
		err := r.Scan(&n.PlanID, &n.Goal, &status, &n.StepOrdinal, &state, &n.Reason,
			&n.Since)
		n.Status, n.StepState = Status(status), StepState(state)
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("changeplan: list what needs a person: %w", err)
	}
	return out, nil
}

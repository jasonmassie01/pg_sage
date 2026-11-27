package fleet

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/review/trigger"
)

// TriggerScheduler fires a member's deep review as a fleet_outlier trigger
// in the member's own review queue (sage.review_trigger), keyed by the
// month, so a member gets at most one fleet deep review a month. The
// member's trigger loop runs it like any other trigger.
type TriggerScheduler struct {
	Pool func(database string) *pgxpool.Pool
}

func (s *TriggerScheduler) store(db string) (*trigger.Store, error) {
	if s == nil || s.Pool == nil || s.Pool(db) == nil {
		return nil, fmt.Errorf("no connection pool for member %s", db)
	}
	return trigger.NewStore(s.Pool(db)), nil
}

// ScheduleDeep implements Scheduler: the trigger's id, the existing one's
// when this month's trigger was fired already.
func (s *TriggerScheduler) ScheduleDeep(ctx context.Context, d Deep) (int64, error) {
	st, err := s.store(d.Database)
	if err != nil {
		return 0, err
	}
	key := "fleet:" + d.Period
	id, fired, err := st.Fire(ctx, trigger.Trigger{Database: d.Database,
		Kind: trigger.KindFleetOutlier, Key: key, Detail: map[string]any{
			"pass_id": d.PassID, "tokens": d.Tokens, "score": d.Score, "reason": d.Reason,
			"period": d.Period}})
	if err != nil || fired {
		return id, err
	}
	t, ok, err := st.Find(ctx, d.Database, trigger.KindFleetOutlier, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("the %s fleet trigger of %s was neither fired nor found",
			d.Period, d.Database)
	}
	return t.ID, nil
}

// DeepStatus implements Scheduler: reviewed once a run consumed the
// trigger, or the trigger joined a run that did.
func (s *TriggerScheduler) DeepStatus(ctx context.Context, db string, ref int64) (bool,
	int64, error) {
	st, err := s.store(db)
	if err != nil {
		return false, 0, err
	}
	t, err := st.ByID(ctx, ref)
	if err != nil {
		return false, 0, err
	}
	switch t.State {
	case trigger.StateConsumed:
		return true, t.RunID, nil
	case trigger.StateCoalesced:
		if t.RunID != 0 || t.CoalescedInto == 0 {
			return true, t.RunID, nil
		}
		into, err := st.ByID(ctx, t.CoalescedInto)
		if err != nil {
			return false, 0, err
		}
		return into.State == trigger.StateConsumed, into.RunID, nil
	}
	return false, 0, nil
}

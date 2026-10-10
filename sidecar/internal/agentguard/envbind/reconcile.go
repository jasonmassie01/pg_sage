package envbind

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Fence is the leader lease a reconcile pass writes under (v2.3
// leader.Elector.Fence). An empty Holder writes unfenced (no election).
type Fence struct {
	Scope  string
	Holder string
	Epoch  int64
}

const fenceSQL = `/* pg_sage agent_env_reconcile v1 */ SELECT 1 FROM sage.fleet_leader_lease
	WHERE scope = $1 AND holder = $2 AND epoch = $3 AND expires_at > now()
	FOR SHARE`

func checkFence(ctx context.Context, q querier, f Fence) error {
	if f.Holder == "" {
		return nil
	}
	var one int
	err := q.QueryRow(ctx, fenceSQL, f.Scope, f.Holder, f.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return fmt.Errorf("envbind: check leader lease: %w", err)
	}
	return nil
}

// ReconcileReport is one reconcile pass.
type ReconcileReport struct {
	Observed int              `json:"observed"`
	Critical []string         `json:"critical"`
	Failed   map[string]error `json:"-"`
}

type observed struct {
	db      Database
	live    Identity
	receipt *clone.Receipt
}

// Reconcile observes every database's live identity, records it (so the
// two-label check sees every database), fills the SRE binding's identity
// strength and epoch, and raises or resolves each database's critical
// binding finding. Writes are fenced by the leader lease.
func (b *Binder) Reconcile(ctx context.Context, dbs []Database, fence Fence) (
	ReconcileReport, error) {
	rep := ReconcileReport{Failed: map[string]error{}}
	if b.control == nil {
		return rep, ErrNoControl
	}
	var seen []observed
	for _, db := range dbs {
		if db.ID == "" || db.Pool == nil {
			rep.Failed[db.Name] = ErrUnbound
			continue
		}
		live, receipt, err := b.observe(ctx, db)
		if err != nil {
			rep.Failed[db.Name] = err
			continue
		}
		seen = append(seen, observed{db: db, live: live, receipt: receipt})
	}
	if err := b.recordObserved(ctx, seen, fence); err != nil {
		return rep, err
	}
	rep.Observed = len(seen)
	for _, o := range seen {
		critical, err := b.reconcileFinding(ctx, o, fence)
		if err != nil {
			if errors.Is(err, ErrFenced) {
				return rep, err
			}
			rep.Failed[o.db.Name] = err
			continue
		}
		if critical {
			rep.Critical = append(rep.Critical, o.db.Name)
		}
	}
	return rep, nil
}

// recordObserved writes every observed identity in one fenced transaction.
func (b *Binder) recordObserved(ctx context.Context, seen []observed, fence Fence) error {
	if len(seen) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, b.control, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, fence); err != nil {
			return err
		}
		for _, o := range seen {
			if err := observeRow(ctx, tx, o.db.ID, o.live); err != nil {
				return err
			}
			if err := fillSREBinding(ctx, tx, o.db.ID, o.live); err != nil {
				return err
			}
		}
		return nil
	})
}

const sreBindingSQL = `/* pg_sage agent_env_reconcile v1 */
UPDATE sage.sre_database_bindings SET identity_strength = $2, cluster_epoch = $3
WHERE database_id = $1
  AND (identity_strength, cluster_epoch) IS DISTINCT FROM ($2::text, $3::text)`

// fillSREBinding records the verified strength and the epoch seen on the
// database's SRE binding (spec §6.5: written as configured/unknown until
// governance fills them).
func fillSREBinding(ctx context.Context, q querier, id string, live Identity) error {
	epoch := live.Epoch()
	if len(epoch) > 128 {
		epoch = epoch[:128]
	}
	if _, err := q.Exec(ctx, sreBindingSQL, id, string(live.Strength()), epoch); err != nil {
		return fmt.Errorf("fill SRE binding of %s: %w", id, err)
	}
	return nil
}

// reconcileFinding evaluates one observed database and keeps its critical
// finding in step: raised while critical, resolved otherwise.
func (b *Binder) reconcileFinding(ctx context.Context, o observed, fence Fence) (bool,
	error) {
	bind, err := b.evaluateLive(ctx, b.control, o.db, o.live, o.receipt)
	if err != nil {
		return false, err
	}
	if err := checkFence(ctx, b.control, fence); err != nil {
		return false, err
	}
	if bind.Evidence.Critical {
		return true, raiseFinding(ctx, o.db, bind.Evidence)
	}
	return false, resolveFinding(ctx, o.db)
}

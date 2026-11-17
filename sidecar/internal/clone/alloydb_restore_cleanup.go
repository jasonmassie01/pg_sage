package clone

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// wait polls operation op of this restore until it is done. A failed
// operation or poll deletes the cluster; the deadline leaves it to the
// reaper.
func (r *adbRestore) wait(ctx context.Context, op, what string) error {
	err := r.api.waitDone(ctx, op)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return r.leftToReaper(what, ctx.Err())
	}
	return r.cleanup(fmt.Errorf("alloydb_restore: %s of %s: %w", what, r.name, err))
}

func (r *adbRestore) leftToReaper(what string, err error) error {
	return fmt.Errorf("alloydb_restore: the %s of cluster %s did not finish within the "+
		"restore deadline %s; left to the reaper, which finds it by name: %w", what, r.name,
		r.a.cfg.Deadline, err)
}

// cleanup deletes the cluster this Restore made, with its instances, on a
// fresh bounded context (the caller's may have ended). A delete that fails
// leaves it to the reaper, and says so.
func (r *adbRestore) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := r.api.deleteOwned(ctx, r.name); err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

// cleanupByName deletes the cluster a restore whose reply was lost may
// have made. One not described yet may still appear, so the create stays
// uncertain: the reaper looks again.
func (r *adbRestore) cleanupByName(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, err := r.api.GetCluster(ctx, r.name)
	if errors.Is(err, ErrAlloyDBNotFound) {
		err = fmt.Errorf("AlloyDB does not describe cluster %s yet, and the restore may "+
			"have been accepted", r.name)
	} else if err == nil {
		err = r.api.deleteOwned(ctx, r.name)
	}
	if err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

func (r *adbRestore) leftBehind(err error) error {
	return fmt.Errorf("%w: alloydb_restore cleanup failed; the reaper finds cluster %s by "+
		"name (it expires at %s): %w", ErrCloneLeftBehind, r.name,
		r.expire.Format(time.RFC3339), err)
}

// deleteOwned deletes cluster name with its instances. It is refused when
// the cluster is not labelled for this receipt: the restore labels it as
// it creates it, so any other cluster is not pg_sage's. One gone or
// already deleting counts as deleted.
func (a *alloyDBHTTP) deleteOwned(ctx context.Context, name string) error {
	c, err := a.GetCluster(ctx, name)
	if errors.Is(err, ErrAlloyDBNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("alloydb_restore: read cluster %s before deleting it: %w", name, err)
	}
	if label := c.Labels[AlloyDBReceiptLabel]; label != name {
		return fmt.Errorf("%w: cluster %s (label %s=%q) is not a pg_sage clone it may "+
			"delete; nothing deleted", ErrInvalidAlloyDBConfig, name, AlloyDBReceiptLabel,
			label)
	}
	if c.State == "DELETING" {
		return nil
	}
	err = a.DeleteCluster(ctx, name)
	if err != nil && !errors.Is(err, ErrAlloyDBNotFound) {
		return fmt.Errorf("alloydb_restore: delete cluster %s: %w", name, err)
	}
	return nil
}

// waitDone polls operation name until it is done: its failure, a poll
// error that is not throttling, or the end of ctx.
func (a *alloyDBHTTP) waitDone(ctx context.Context, name string) error {
	for {
		op, err := a.Operation(ctx, name)
		if err == nil && op.Done {
			if op.Error != nil {
				return fmt.Errorf("%w: code %d: %s", errAlloyDBOpFailed, op.Error.Code,
					op.Error.Message)
			}
			return nil
		}
		if err != nil && !Throttled(err) && ctx.Err() == nil {
			return err
		}
		if err := auroraPollPause(ctx, a.poll); err != nil {
			return err
		}
	}
}

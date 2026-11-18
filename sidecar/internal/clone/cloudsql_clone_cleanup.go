package clone

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// cleanup deletes the clone this Restore made, on a fresh bounded context
// (the caller's may have ended). A delete that fails leaves it to the
// reaper, and says so.
func (r *sqlClone) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := r.api.deleteOwned(ctx, r.name); err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

// cleanupByName deletes the clone a request whose reply was lost may have
// made. One not described yet may still appear, so the create stays
// uncertain: the reaper looks again.
func (r *sqlClone) cleanupByName(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, err := r.api.GetInstance(ctx, r.name)
	if errors.Is(err, ErrCloudSQLNotFound) {
		err = fmt.Errorf("Cloud SQL does not describe instance %s yet, and the clone may "+
			"have been accepted", r.name)
	} else if err == nil {
		err = r.api.deleteOwned(ctx, r.name)
	}
	if err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

func (r *sqlClone) leftBehind(err error) error {
	return fmt.Errorf("%w: cloudsql_clone cleanup failed; the reaper finds instance %s by "+
		"name (it expires at %s): %w", ErrCloneLeftBehind, r.name,
		r.expire.Format(time.RFC3339), err)
}

// deleteOwned deletes clone name. It is refused when the instance is
// labelled for another receipt, or protected from deletion after pg_sage
// labelled it (someone protected it on purpose). An unlabelled clone's
// protection was copied from production with the clone, so it is lifted
// first. One gone or already deleting counts as deleted.
func (a *cloudSQLHTTP) deleteOwned(ctx context.Context, name string) error {
	in, err := a.GetInstance(ctx, name)
	if errors.Is(err, ErrCloudSQLNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cloudsql_clone: read instance %s before deleting it: %w", name, err)
	}
	label, labelled := in.Settings.UserLabels[CloudSQLReceiptLabel]
	if (labelled && label != name) || (labelled && in.Settings.DeletionProtectionEnabled) {
		return fmt.Errorf("%w: instance %s (label %s=%q, deletion protection %v) is not a "+
			"pg_sage clone it may delete; nothing deleted", ErrInvalidCloudSQLConfig, name,
			CloudSQLReceiptLabel, label, in.Settings.DeletionProtectionEnabled)
	}
	if in.State == "PENDING_DELETE" {
		return nil
	}
	if in.Settings.DeletionProtectionEnabled {
		if err := a.unprotect(ctx, name); err != nil {
			return err
		}
	}
	err = a.Delete(ctx, name)
	if err != nil && !errors.Is(err, ErrCloudSQLNotFound) {
		return fmt.Errorf("cloudsql_clone: delete instance %s: %w", name, err)
	}
	return nil
}

// unprotect lifts the deletion protection an unlabelled clone copied.
func (a *cloudSQLHTTP) unprotect(ctx context.Context, name string) error {
	off := false
	op, err := a.Patch(ctx, name, cloudSQLPatch{Settings: cloudSQLPatchSettings{
		DeletionProtection: &off}})
	if err == nil {
		err = a.waitDone(ctx, op.Name)
	}
	if err != nil {
		return fmt.Errorf("cloudsql_clone: lift the deletion protection clone %s copied "+
			"from production: %w", name, err)
	}
	return nil
}

// waitDone polls operation name until it is DONE: its failure, a poll
// error that is not throttling, or the end of ctx.
func (a *cloudSQLHTTP) waitDone(ctx context.Context, name string) error {
	for {
		op, err := a.Operation(ctx, name)
		if err == nil && op.Status == "DONE" {
			if f := op.failure(); f != "" {
				return fmt.Errorf("%w: %s", errCloudSQLOpFailed, f)
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

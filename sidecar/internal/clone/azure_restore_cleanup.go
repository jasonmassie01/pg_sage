package clone

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// wait polls the restore's progress URL until it is done. A failed
// operation or poll deletes the server; the deadline leaves it to the
// reaper. An empty URL is a PUT ARM finished synchronously.
func (r *azRestore) wait(ctx context.Context, poll string) error {
	if poll == "" {
		return nil
	}
	err := r.api.waitDone(ctx, poll)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("azure_restore: the restore of server %s did not finish within "+
			"the restore deadline %s; left to the reaper, which finds it by name: %w",
			r.name, r.a.cfg.Deadline, ctx.Err())
	}
	return r.cleanup(fmt.Errorf("azure_restore: restore of %s: %w", r.name, err))
}

// cleanup deletes the server this Restore made on a fresh bounded context
// (the caller's may have ended). A delete that fails leaves it to the
// reaper, and says so.
func (r *azRestore) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := r.api.deleteOwned(ctx, r.name); err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

// cleanupByName deletes the server a PUT whose reply was lost may have
// made. One not described yet may still appear, so the create stays
// uncertain: the reaper looks again.
func (r *azRestore) cleanupByName(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, err := r.api.GetServer(ctx, r.name)
	if errors.Is(err, ErrAzureNotFound) {
		err = fmt.Errorf("ARM does not describe server %s yet, and the restore may have "+
			"been accepted", r.name)
	} else if err == nil {
		err = r.api.deleteOwned(ctx, r.name)
	}
	if err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

func (r *azRestore) leftBehind(err error) error {
	return fmt.Errorf("%w: azure_restore cleanup failed; the reaper finds server %s by "+
		"name (it expires at %s): %w", ErrCloneLeftBehind, r.name,
		r.expire.Format(time.RFC3339), err)
}

// deleteOwned deletes server name. It is refused when the server is not
// tagged for this receipt: the restore tags it as it creates it, so any
// other server is not pg_sage's. One gone or already dropping counts as
// deleted. The delete is not waited for: a receipt is destroyed once ARM
// accepts it.
func (a *azureHTTP) deleteOwned(ctx context.Context, name string) error {
	s, err := a.GetServer(ctx, name)
	if errors.Is(err, ErrAzureNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("azure_restore: read server %s before deleting it: %w", name, err)
	}
	if tag := s.Tags[AzureReceiptTag]; tag != name {
		return fmt.Errorf("%w: server %s (tag %s=%q) is not a pg_sage clone it may delete; "+
			"nothing deleted", ErrInvalidAzureRestoreConfig, name, AzureReceiptTag, tag)
	}
	if s.Props.State == "Dropping" {
		return nil
	}
	err = a.DeleteServer(ctx, name)
	if err != nil && !errors.Is(err, ErrAzureNotFound) {
		return fmt.Errorf("azure_restore: delete server %s: %w", name, err)
	}
	return nil
}

// waitDone polls rawURL until the operation is done: its failure, a poll
// error that is not throttling, or the end of ctx.
func (a *azureHTTP) waitDone(ctx context.Context, rawURL string) error {
	for {
		done, err := a.Operation(ctx, rawURL)
		if done {
			return err
		}
		if err != nil && !Throttled(err) && ctx.Err() == nil {
			return err
		}
		if err := auroraPollPause(ctx, a.poll); err != nil {
			return err
		}
	}
}

package clone

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// requireProduction refuses a restore without a production identity
// before AWS is contacted: the reaper's adapter only destroys.
func (a *RDSRestoreAPI) requireProduction() error {
	if a.cfg.Production == nil {
		return fmt.Errorf("%w: rds_restore has no production identity to tell a clone "+
			"from production", ErrCloneUnavailable)
	}
	return nil
}

// callerOutlastsDeadline refuses a restore whose caller ends before the
// restore deadline (a bounded MCP call: mcp.call_timeout_seconds is at
// most 10 minutes): the wait would be cut short and leave a billable
// full copy to the reaper every time. Nothing is created.
func (a *RDSRestoreAPI) callerOutlastsDeadline(ctx context.Context) error {
	end, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	if left := time.Until(end); left < a.cfg.Deadline {
		return fmt.Errorf("%w: the caller ends in %s, before the restore deadline %s "+
			"(clone.restore_deadline_minutes), so the full copy could not be used; nothing "+
			"created", ErrCloneUnavailable, left.Round(time.Second), a.cfg.Deadline)
	}
	return nil
}

// rdsSource is the monitored instance a restore is taken from.
type rdsSource struct {
	dsn, host string
	inst      RDSInstance
}

// LatestSnapshot is the monitored instance and its latest restorable
// time: what a restore made now copies.
func (a *RDSRestoreAPI) LatestSnapshot(ctx context.Context) (Snapshot, error) {
	if err := a.requireProduction(); err != nil {
		return Snapshot{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, rdsDestroyTimeout)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: src.inst.ID, CreatedAt: src.inst.LatestRestorableTime}, nil
}

// source reads production's DSN, checks that its host is the endpoint of
// an RDS for PostgreSQL instance of the configured region with automated
// backups, and reads that instance. Without production no clone can be
// told apart from it, so the restore is unavailable (fail closed). Errors
// never quote the DSN.
func (a *RDSRestoreAPI) source(ctx context.Context, api RDSRestoreClient) (rdsSource, error) {
	if err := a.requireProduction(); err != nil {
		return rdsSource{}, err
	}
	prod, err := a.cfg.Production(ctx)
	if err != nil {
		return rdsSource{}, fmt.Errorf("%w: read the monitored RDS database's identity: %w",
			ErrCloneUnavailable, err)
	}
	if blank(prod.DSN) {
		return rdsSource{}, fmt.Errorf("%w: the monitored RDS database has no DSN",
			ErrCloneUnavailable)
	}
	host, err := dsnHost(prod.DSN)
	if err != nil {
		return rdsSource{}, fmt.Errorf("%w: %w", ErrInvalidRDSRestoreConfig, err)
	}
	id, region, ok := rdsInstanceHost(host)
	if !ok {
		return rdsSource{}, fmt.Errorf("%w: the monitored host %s is not an RDS instance "+
			"endpoint (<instance>.<id>.<region>.rds.amazonaws.com); connect pg_sage through "+
			"the instance endpoint", ErrNotRDSInstance, host)
	}
	if region != a.cfg.Region {
		return rdsSource{}, fmt.Errorf("%w: the monitored instance %s is in %s, not "+
			"clone.rds.region %s", ErrInvalidRDSRestoreConfig, id, region, a.cfg.Region)
	}
	in, err := a.readSource(ctx, api, id, host)
	if err != nil {
		return rdsSource{}, err
	}
	return rdsSource{dsn: prod.DSN, host: host, inst: in}, nil
}

func (a *RDSRestoreAPI) readSource(ctx context.Context, api RDSRestoreClient, id,
	host string) (RDSInstance, error) {
	in, err := api.DescribeInstance(ctx, id)
	if errors.Is(err, ErrRDSNotFound) {
		return RDSInstance{}, fmt.Errorf("%w: RDS describes no instance %s in %s: %w",
			ErrNotRDSInstance, id, a.cfg.Region, err)
	}
	if err != nil {
		return RDSInstance{}, fmt.Errorf("rds_restore: read the monitored instance %s: %w",
			id, err)
	}
	if in.Engine != rdsEngine || in.ClusterID != "" || !sameHost(in.Endpoint, host) {
		return RDSInstance{}, fmt.Errorf("%w: instance %s (engine %q, cluster %q) is not a "+
			"single RDS for PostgreSQL instance serving the monitored host %s; Aurora uses "+
			"clone.provider aurora_clone", ErrNotRDSInstance, id, in.Engine, in.ClusterID, host)
	}
	if in.LatestRestorableTime.IsZero() {
		return RDSInstance{}, fmt.Errorf("%w: instance %s has automated backups off (backup "+
			"retention 0), so it cannot be restored to a point in time", ErrNoRestorableTime, id)
	}
	return in, nil
}

// Restore restores instance spec.ResourceName (sage-<purpose>-<8 hex>, the
// receipt's) from the monitored instance at its latest restorable time,
// in the source's subnet group, security groups, parameter group and
// storage type, single-AZ and unprotected, tagged with the receipt name
// and the expiry; waits until it is available, at most the restore
// deadline; and returns the monitored DSN on its endpoint. A failure
// after the instance exists deletes it before the error returns; a wait
// cut short leaves it to the reaper, which finds it by tag.
func (a *RDSRestoreAPI) Restore(ctx context.Context, snap Snapshot,
	spec CloneSpec) (Clone, error) {
	if err := a.requireProduction(); err != nil {
		return Clone{}, err
	}
	if err := a.callerOutlastsDeadline(ctx); err != nil {
		return Clone{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Clone{}, err
	}
	if !auroraOwnedName.MatchString(spec.ResourceName) {
		return Clone{}, fmt.Errorf("%w: clone name %q is not sage-<purpose>-<8 hex> (the "+
			"receipt's resource name); nothing created", ErrInvalidRDSRestoreConfig,
			spec.ResourceName)
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Deadline)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Clone{}, err
	}
	if src.inst.ID != snap.ID {
		return Clone{}, fmt.Errorf("rds_restore: the monitored instance is %s, not %s as "+
			"read before; nothing created", src.inst.ID, snap.ID)
	}
	run := &rdsRestore{a: a, api: api, src: src, name: spec.ResourceName,
		expire: a.cfg.Now().Add(a.cfg.TTL).UTC()}
	return run.restore(ctx)
}

// rdsRestore is one Restore: what it asked for, so cleanup deletes
// exactly that.
type rdsRestore struct {
	a      *RDSRestoreAPI
	api    RDSRestoreClient
	src    rdsSource
	name   string
	expire time.Time
}

func (r *rdsRestore) request() (RDSRestoreRequest, error) {
	s := r.src.inst
	class := r.a.cfg.InstanceClass
	if class == "" {
		class = s.Class
	}
	if class == "" {
		return RDSRestoreRequest{}, fmt.Errorf("rds_restore: instance %s has no instance "+
			"class: set clone.rds.instance_class", s.ID)
	}
	req := RDSRestoreRequest{ID: r.name, Source: s.ID, Class: class,
		SubnetGroup: s.SubnetGroup, ParameterGroup: s.ParameterGroup,
		StorageType: s.StorageType, SecurityGroups: s.SecurityGroups,
		PubliclyAccessible: s.PubliclyAccessible,
		Tags: map[string]string{AuroraReceiptTag: r.name,
			AuroraExpiresTag: r.expire.Format(time.RFC3339)}}
	if s.StorageType == "io1" || s.StorageType == "io2" {
		req.Iops = s.Iops
	}
	return req, nil
}

func (r *rdsRestore) restore(ctx context.Context) (Clone, error) {
	req, err := r.request()
	if err != nil {
		return Clone{}, err
	}
	created, err := r.api.RestoreInstance(ctx, req)
	if err != nil {
		err = fmt.Errorf("rds_restore: restore %s from %s: %w", r.name, r.src.inst.ID, err)
		// RDS refused it (4xx): nothing was made. A 5xx, a lost reply or
		// "already exists" (the SDK retried an accepted restore) may follow
		// a restore RDS accepted: it is looked for by name and tag.
		var status *ProviderStatusError
		if errors.As(err, &status) && status.StatusCode < 500 &&
			!errors.Is(err, ErrRDSAlreadyExists) {
			return Clone{}, err
		}
		return Clone{}, r.cleanupByTag(err)
	}
	if created.ID != r.name {
		return Clone{}, r.cleanup(fmt.Errorf("rds_restore: the restore reply names instance "+
			"%q, not %s", created.ID, r.name))
	}
	in, err := r.waitReady(ctx)
	if err != nil {
		return Clone{}, err
	}
	dsn, err := r.dsn(in)
	if err != nil {
		return Clone{}, r.cleanup(err)
	}
	return Clone{DSN: dsn, ID: r.name, CreatedFrom: r.a.cfg.Now()}, nil
}

// waitReady polls the restored instance until it is available. A failed
// status deletes it; one not described, or the deadline, leaves it to the
// reaper.
func (r *rdsRestore) waitReady(ctx context.Context) (RDSInstance, error) {
	for {
		in, err := r.api.DescribeInstance(ctx, r.name)
		if errors.Is(err, ErrRDSNotFound) && ctx.Err() == nil {
			// RDS accepted the restore but does not describe it (yet): it
			// may still appear, so the create stays uncertain.
			return RDSInstance{}, errors.Join(fmt.Errorf("rds_restore: poll instance %s: %w",
				r.name, err), r.leftBehind(errors.New("the accepted restore is not described")))
		}
		if err != nil && !Throttled(err) && ctx.Err() == nil {
			return RDSInstance{}, r.cleanup(fmt.Errorf("rds_restore: poll instance %s: %w",
				r.name, err))
		}
		if err == nil {
			if bad := auroraFailedStatus(in.Status); bad != "" {
				return RDSInstance{}, r.cleanup(fmt.Errorf("rds_restore: instance %s %s "+
					"(status %s)", r.name, bad, in.Status))
			}
			if in.Status == "available" {
				return in, nil
			}
		}
		if err := auroraPollPause(ctx, r.a.cfg.PollEvery); err != nil {
			return RDSInstance{}, fmt.Errorf("rds_restore: instance %s not available within "+
				"the restore deadline %s; left to the reaper, which finds it by tag %s: %w",
				r.name, r.a.cfg.Deadline, AuroraReceiptTag, err)
		}
	}
}

// dsn is production's DSN on the restored instance's endpoint, checked
// not to be production's host.
func (r *rdsRestore) dsn(in RDSInstance) (string, error) {
	if blank(in.Endpoint) {
		return "", fmt.Errorf("rds_restore: instance %s has no endpoint", r.name)
	}
	if sameHost(in.Endpoint, r.src.host) || sameHost(in.Endpoint, r.src.inst.Endpoint) {
		return "", fmt.Errorf("%w: instance %s answers on production's host %s",
			ErrCloneIsProduction, r.name, in.Endpoint)
	}
	port := in.Port
	if port == 0 {
		port = r.src.inst.Port
	}
	dsn, err := auroraCloneDSN(r.src.dsn, r.src.host, in.Endpoint, port)
	if err != nil {
		return "", fmt.Errorf("rds_restore: %w", err)
	}
	return dsn, nil
}

// cleanup deletes the instance this Restore made, on a fresh bounded
// context (the caller's may have ended). A delete that fails leaves it to
// the reaper, and says so.
func (r *rdsRestore) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := deleteOwnedRDS(ctx, r.api, r.name); err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

// cleanupByTag deletes the instance a restore whose reply was lost may
// have made, found by name and tag. One not described yet may still
// appear, so the create stays uncertain: the reaper looks again.
func (r *rdsRestore) cleanupByTag(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, found, err := r.a.FindByName(ctx, r.name)
	if err == nil && !found {
		err = fmt.Errorf("RDS does not describe instance %s yet, and the restore may have "+
			"been accepted", r.name)
	} else if err == nil {
		err = deleteOwnedRDS(ctx, r.api, r.name)
	}
	if err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

func (r *rdsRestore) leftBehind(err error) error {
	return fmt.Errorf("%w: rds_restore cleanup failed; the reaper finds instance %s by "+
		"tag %s (it expires at %s): %w", ErrCloneLeftBehind, r.name, AuroraReceiptTag,
		r.expire.Format(time.RFC3339), err)
}

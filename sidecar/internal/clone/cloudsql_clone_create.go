package clone

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// errCloudSQLOpFailed: a Cloud SQL operation finished with an error.
var errCloudSQLOpFailed = errors.New("the Cloud SQL operation failed")

// requireProduction refuses a clone without a production identity before
// Google is contacted: the reaper's adapter only destroys.
func (a *CloudSQLCloneAPI) requireProduction() error {
	if a.cfg.Production == nil {
		return fmt.Errorf("%w: cloudsql_clone has no production identity to tell a clone "+
			"from production", ErrCloneUnavailable)
	}
	return nil
}

// callerOutlastsDeadline refuses a clone whose caller ends before the
// restore deadline (a bounded MCP call): the wait would be cut short and
// leave a billable full copy to the reaper every time. Nothing is made.
func (a *CloudSQLCloneAPI) callerOutlastsDeadline(ctx context.Context) error {
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

// cloudSQLSource is the monitored instance a clone is taken from, and the
// kind of its IP the monitored DSN uses.
type cloudSQLSource struct {
	dsn, host, ipType string
	inst              cloudSQLInstance
}

// LatestSnapshot is the monitored instance now: a clone copies its
// current state.
func (a *CloudSQLCloneAPI) LatestSnapshot(ctx context.Context) (Snapshot, error) {
	if err := a.requireProduction(); err != nil {
		return Snapshot{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, cloudSQLCallTimeout)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: src.inst.Name, CreatedAt: a.cfg.Now()}, nil
}

// source reads production's DSN and the configured instance, and checks
// that it is a running Cloud SQL for PostgreSQL primary that the DSN
// reaches by one of its IPs: pg_sage clones only what it monitors, and
// reaches the clone the same way. Errors never quote the DSN.
func (a *CloudSQLCloneAPI) source(ctx context.Context, api *cloudSQLHTTP) (cloudSQLSource,
	error) {
	prod, err := a.cfg.Production(ctx)
	if err != nil {
		return cloudSQLSource{}, fmt.Errorf("%w: read the monitored Cloud SQL database's "+
			"identity: %w", ErrCloneUnavailable, err)
	}
	if blank(prod.DSN) {
		return cloudSQLSource{}, fmt.Errorf("%w: the monitored Cloud SQL database has no DSN",
			ErrCloneUnavailable)
	}
	host, err := dsnHost(prod.DSN)
	if err != nil {
		return cloudSQLSource{}, fmt.Errorf("%w: %w", ErrInvalidCloudSQLConfig, err)
	}
	in, err := api.GetInstance(ctx, a.cfg.Instance)
	if errors.Is(err, ErrCloudSQLNotFound) {
		return cloudSQLSource{}, fmt.Errorf("%w: project %s has no instance %s: %w",
			ErrNotCloudSQLInstance, a.cfg.Project, a.cfg.Instance, err)
	}
	if err != nil {
		return cloudSQLSource{}, fmt.Errorf("cloudsql_clone: read the monitored instance "+
			"%s: %w", a.cfg.Instance, err)
	}
	if !strings.HasPrefix(in.DatabaseVersion, "POSTGRES_") ||
		in.InstanceType != "CLOUD_SQL_INSTANCE" {
		return cloudSQLSource{}, fmt.Errorf("%w: instance %s (%s, %s) is not a Cloud SQL "+
			"for PostgreSQL primary", ErrNotCloudSQLInstance, in.Name, in.DatabaseVersion,
			in.InstanceType)
	}
	kind := in.typeOfIP(host)
	if kind == "" {
		return cloudSQLSource{}, fmt.Errorf("%w: the monitored host %s is not an IP of "+
			"instance %s; connect pg_sage to the instance's private or public IP (not the "+
			"Auth Proxy or a DNS name)", ErrNotCloudSQLInstance, host, in.Name)
	}
	if in.State != "RUNNABLE" {
		return cloudSQLSource{}, fmt.Errorf("%w: instance %s is %s, not RUNNABLE",
			ErrCloneUnavailable, in.Name, in.State)
	}
	return cloudSQLSource{dsn: prod.DSN, host: host, ipType: kind, inst: in}, nil
}

// Restore clones the monitored instance as spec.ResourceName
// (sage-<purpose>-<8 hex>, the receipt's), waits for the operation, labels
// the clone with the receipt name and expiry (lifting the copied deletion
// protection and HA), all within the restore deadline, and returns the
// monitored DSN on the clone's IP of the same kind. A failure after the
// clone exists deletes it before the error returns; a wait cut short
// leaves it to the reaper, which finds it by name.
func (a *CloudSQLCloneAPI) Restore(ctx context.Context, _ Snapshot,
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
	if err := checkCloneName(spec.ResourceName, "nothing created"); err != nil {
		return Clone{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Deadline)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Clone{}, err
	}
	run := &sqlClone{a: a, api: api, src: src, name: spec.ResourceName,
		expire: a.cfg.Now().Add(a.cfg.TTL).UTC()}
	return run.clone(ctx)
}

// sqlClone is one Restore: what it asked for, so cleanup deletes exactly
// that.
type sqlClone struct {
	a      *CloudSQLCloneAPI
	api    *cloudSQLHTTP
	src    cloudSQLSource
	name   string
	expire time.Time
}

func (r *sqlClone) clone(ctx context.Context) (Clone, error) {
	op, err := r.api.Clone(ctx, r.src.inst.Name, r.name)
	if err != nil {
		err = fmt.Errorf("cloudsql_clone: clone %s to %s: %w", r.src.inst.Name, r.name, err)
		// Google refused it (4xx, an existing name included): nothing was
		// made. A 5xx or a lost reply may follow a clone it accepted.
		var status *ProviderStatusError
		if errors.As(err, &status) && status.StatusCode < 500 {
			return Clone{}, err
		}
		return Clone{}, r.cleanupByName(err)
	}
	// The operation's targetId may name the new instance or the source the
	// call was made on (unconfirmed live, CHECK-37); any other instance
	// means the reply is not this clone's.
	if op.Name == "" || (op.TargetID != "" && op.TargetID != r.name &&
		op.TargetID != r.src.inst.Name) {
		return Clone{}, r.cleanup(fmt.Errorf("cloudsql_clone: the clone reply names "+
			"operation %q for instance %q, not %s", op.Name, op.TargetID, r.name))
	}
	if err := r.wait(ctx, op.Name, "clone"); err != nil {
		return Clone{}, err
	}
	in, err := r.api.GetInstance(ctx, r.name)
	if err == nil && in.State != "RUNNABLE" {
		err = fmt.Errorf("instance %s is %s after its clone finished", r.name, in.State)
	}
	if err != nil {
		return Clone{}, r.cleanup(fmt.Errorf("cloudsql_clone: read the clone: %w", err))
	}
	dsn, err := r.dsn(in)
	if err != nil {
		return Clone{}, r.cleanup(err)
	}
	if err := r.label(ctx, in); err != nil {
		return Clone{}, err
	}
	return Clone{DSN: dsn, ID: r.name, CreatedFrom: r.a.cfg.Now()}, nil
}

// label marks the clone pg_sage's (receipt name and expiry, the source's
// labels kept) and lifts the deletion protection and regional HA it copied
// from production, so the reaper can delete it and no standby is billed.
func (r *sqlClone) label(ctx context.Context, in cloudSQLInstance) error {
	labels := maps.Clone(in.Settings.UserLabels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[CloudSQLReceiptLabel] = r.name
	labels[CloudSQLExpiresLabel] = strconv.FormatInt(r.expire.Unix(), 10)
	off := false
	op, err := r.api.Patch(ctx, r.name, cloudSQLPatch{Settings: cloudSQLPatchSettings{
		UserLabels: labels, DeletionProtection: &off, AvailabilityType: "ZONAL"}})
	if err != nil {
		if ctx.Err() != nil {
			return r.leftToReaper("labelling", ctx.Err())
		}
		return r.cleanup(fmt.Errorf("cloudsql_clone: label clone %s: %w", r.name, err))
	}
	return r.wait(ctx, op.Name, "labelling")
}

// wait polls operation op of this clone until it is done. A failed
// operation or poll deletes the clone; the deadline leaves it to the
// reaper.
func (r *sqlClone) wait(ctx context.Context, op, what string) error {
	err := r.api.waitDone(ctx, op)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return r.leftToReaper(what, ctx.Err())
	}
	return r.cleanup(fmt.Errorf("cloudsql_clone: %s of %s: %w", what, r.name, err))
}

func (r *sqlClone) leftToReaper(what string, err error) error {
	return fmt.Errorf("cloudsql_clone: the %s of instance %s did not finish within the "+
		"restore deadline %s; left to the reaper, which finds it by name: %w", what, r.name,
		r.a.cfg.Deadline, err)
}

// dsn is production's DSN on the clone's IP of the kind production's DSN
// uses, checked not to be one of production's IPs.
func (r *sqlClone) dsn(in cloudSQLInstance) (string, error) {
	ip := in.ipOfType(r.src.ipType)
	if blank(ip) {
		return "", fmt.Errorf("cloudsql_clone: clone %s has no %s IP", r.name, r.src.ipType)
	}
	if sameHost(ip, r.src.host) || r.src.inst.typeOfIP(ip) != "" {
		return "", fmt.Errorf("%w: clone %s answers on production's IP %s",
			ErrCloneIsProduction, r.name, ip)
	}
	dsn, err := auroraCloneDSN(r.src.dsn, r.src.host, ip, 5432)
	if err != nil {
		return "", fmt.Errorf("cloudsql_clone: %w", err)
	}
	return dsn, nil
}

package clone

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// rds_restore (lab-05d): RDS for PostgreSQL has no copy-on-write clone, so
// a clone is a point-in-time restore of the monitored instance to a new
// instance: a full copy at the full instance price, made only with
// clone.allow_full_copy. RDSRestoreAPI is the SnapshotAPI of it;
// SnapshotProvider wraps it as a Provider.

// RDS restore errors. Each is distinguishable with errors.Is.
var (
	// ErrInvalidRDSRestoreConfig: a region, instance class, deadline, TTL,
	// clone name or DSN outside the accepted shape, or an instance pg_sage
	// did not make.
	ErrInvalidRDSRestoreConfig = errors.New("clone: invalid rds_restore configuration")
	// ErrNotRDSInstance: the monitored database is not reached through the
	// endpoint of an RDS for PostgreSQL instance, so there is no source.
	ErrNotRDSInstance = errors.New("clone: the monitored database is not an RDS for " +
		"PostgreSQL instance")
	// ErrNoRestorableTime: the source has no latest restorable time
	// (automated backups are off), so it cannot be restored to a point in
	// time.
	ErrNoRestorableTime = errors.New("clone: the RDS source has no restorable time")
	// ErrRDSNotFound: RDS describes no such instance.
	ErrRDSNotFound = errors.New("clone: rds instance not found")
	// ErrRDSAlreadyExists: RDS already has an instance of that name, as
	// when the SDK retries a restore RDS accepted.
	ErrRDSAlreadyExists = errors.New("clone: rds instance already exists")
)

// RDS restore bounds and defaults.
const (
	// RDSRestoreMinDeadline and RDSRestoreMaxDeadline bound
	// clone.restore_deadline_minutes: a restore replays WAL onto a copy of
	// the whole volume, which takes tens of minutes to hours.
	RDSRestoreMinDeadline = 30 * time.Minute
	RDSRestoreMaxDeadline = 12 * time.Hour
	// RDSRestoreMaxTTL bounds a restored instance's life: a leaked one
	// holds production data for at most a day.
	RDSRestoreMaxTTL = 24 * time.Hour
	// rdsEngine is the only engine restored.
	rdsEngine      = "postgres"
	defaultRDSPoll = 30 * time.Second
	// rdsDestroyTimeout bounds one destroy's describe and delete calls.
	rdsDestroyTimeout = 5 * time.Minute
)

var rdsInstanceLabel = regexp.MustCompile(`^[a-z0-9]{6,20}$`)

// RDSProduction is the monitored (production) database a restore is taken
// from: its DSN names the source instance's endpoint, and the clone's DSN
// reuses it (same users, copied data).
type RDSProduction struct{ DSN string }

// RDSRestoreConfig configures an RDSRestoreAPI.
type RDSRestoreConfig struct {
	// Region is the AWS region of the monitored instance and its restores.
	Region string
	// InstanceClass is the restored instance's class; "": the source's.
	InstanceClass string
	// Deadline bounds one restore, from the request until the instance is
	// available (clone.restore_deadline_minutes). A restore not available
	// by then is left to the reaper (an uncertain create).
	Deadline time.Duration
	// TTL is the longest a restored instance may live: tagged on it and
	// its MaxTTL. It must exceed Deadline.
	TTL time.Duration
	// API is the RDS API with the AWS credential chain; nil, or an error,
	// makes the provider unavailable.
	API func(context.Context) (RDSRestoreClient, error)
	// Production reads production's DSN at each restore; nil makes every
	// restore unavailable (an adapter that only destroys, as the reaper's).
	Production func(context.Context) (RDSProduction, error)
	PollEvery  time.Duration // status poll period; 0: 30 seconds
	Now        func() time.Time
}

// RDSRestoreAPI restores the monitored RDS for PostgreSQL instance to its
// latest restorable time as a new instance tagged with its receipt name,
// so the reaper finds a leaked one; RDS has no server-side expiry.
type RDSRestoreAPI struct{ cfg RDSRestoreConfig }

// NewRDSRestoreAPI validates cfg; it contacts nothing.
func NewRDSRestoreAPI(cfg RDSRestoreConfig) (*RDSRestoreAPI, error) {
	if !auroraRegion.MatchString(cfg.Region) {
		return nil, fmt.Errorf("%w: region %q must be an AWS region such as us-east-1",
			ErrInvalidRDSRestoreConfig, cfg.Region)
	}
	if cfg.InstanceClass != "" && !auroraClass.MatchString(cfg.InstanceClass) {
		return nil, fmt.Errorf("%w: instance class %q must be an RDS class such as "+
			"db.m6g.large", ErrInvalidRDSRestoreConfig, cfg.InstanceClass)
	}
	if cfg.Deadline < RDSRestoreMinDeadline || cfg.Deadline > RDSRestoreMaxDeadline {
		return nil, fmt.Errorf("%w: restore deadline %s must be within %s-%s",
			ErrInvalidRDSRestoreConfig, cfg.Deadline, RDSRestoreMinDeadline,
			RDSRestoreMaxDeadline)
	}
	if cfg.TTL <= cfg.Deadline || cfg.TTL > RDSRestoreMaxTTL {
		return nil, fmt.Errorf("%w: clone TTL %s must exceed the restore deadline %s and be "+
			"at most %s", ErrInvalidRDSRestoreConfig, cfg.TTL, cfg.Deadline, RDSRestoreMaxTTL)
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultRDSPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RDSRestoreAPI{cfg: cfg}, nil
}

// NewRDSRestoreProvider is the rds_restore Provider: SnapshotProvider over
// an RDSRestoreAPI of cfg.
func NewRDSRestoreProvider(cfg RDSRestoreConfig) (*SnapshotProvider, error) {
	api, err := NewRDSRestoreAPI(cfg)
	if err != nil {
		return nil, err
	}
	return NewSnapshotProvider(api, api.cfg.Now)
}

// client is the RDS API, or ErrCloneUnavailable without AWS credentials:
// no restore can be made or deleted, so rehearsals are recommend-only.
func (a *RDSRestoreAPI) client(ctx context.Context) (RDSRestoreClient, error) {
	if a.cfg.API == nil {
		return nil, fmt.Errorf("%w: rds_restore has no AWS credentials or configuration; "+
			"rehearsals are recommend-only until then", ErrCloneUnavailable)
	}
	api, err := a.cfg.API(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: rds_restore has no usable AWS credentials (the default "+
			"chain: environment, shared config, web identity or instance role): %w; "+
			"rehearsals are recommend-only until then", ErrCloneUnavailable, err)
	}
	if api == nil {
		return nil, fmt.Errorf("%w: rds_restore has no AWS credentials", ErrCloneUnavailable)
	}
	return api, nil
}

// Describe: the restore is ready in about SnapshotTypicalReady (it scales
// with the volume and the WAL to replay) and lives at most TTL.
// SnapshotProvider makes it a full copy.
func (a *RDSRestoreAPI) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindFullCopy, TypicalReady: SnapshotTypicalReady,
		MaxTTL: a.cfg.TTL}, nil
}

// TargetScope is the receipt scope of this provider's clones.
func (a *RDSRestoreAPI) TargetScope() string { return "aws/" + a.cfg.Region }

// FindByName is the id of the instance named name (one pg_sage gives,
// sage-<purpose>-<8 hex>) and tagged with it; found is false when RDS
// describes none, or one pg_sage did not tag.
func (a *RDSRestoreAPI) FindByName(ctx context.Context, name string) (string, bool,
	error) {
	api, err := a.client(ctx)
	if err != nil {
		return "", false, err
	}
	if !auroraOwnedName.MatchString(name) {
		return "", false, fmt.Errorf("%w: %q is not a pg_sage clone name "+
			"(sage-<purpose>-<8 hex>)", ErrInvalidRDSRestoreConfig, name)
	}
	ctx, cancel := context.WithTimeout(ctx, rdsDestroyTimeout)
	defer cancel()
	in, err := api.DescribeInstance(ctx, name)
	if errors.Is(err, ErrRDSNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("rds_restore: find instance %s: %w", name, err)
	}
	if in.Tags[AuroraReceiptTag] != name {
		return "", false, nil
	}
	return in.ID, true, nil
}

// Destroy deletes an instance this provider made, without a final
// snapshot. The instance is read first: one not named
// sage-<purpose>-<8 hex>, not tagged with that name, or protected from
// deletion is refused and nothing is deleted. One gone or already
// deleting counts as deleted.
func (a *RDSRestoreAPI) Destroy(ctx context.Context, id string) error {
	api, err := a.client(ctx)
	if err != nil {
		return err
	}
	if !auroraOwnedName.MatchString(id) {
		return fmt.Errorf("%w: %q is not a pg_sage clone instance (sage-<purpose>-<8 hex>); "+
			"nothing deleted", ErrInvalidRDSRestoreConfig, id)
	}
	ctx, cancel := context.WithTimeout(ctx, rdsDestroyTimeout)
	defer cancel()
	return deleteOwnedRDS(ctx, api, id)
}

func deleteOwnedRDS(ctx context.Context, api RDSRestoreClient, id string) error {
	in, err := api.DescribeInstance(ctx, id)
	if errors.Is(err, ErrRDSNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rds_restore: read instance %s before deleting it: %w", id, err)
	}
	if in.Tags[AuroraReceiptTag] != id || in.DeletionProtection {
		return fmt.Errorf("%w: instance %s (tag %s=%q, deletion protection %v) is not a "+
			"pg_sage clone instance; nothing deleted", ErrInvalidRDSRestoreConfig, id,
			AuroraReceiptTag, in.Tags[AuroraReceiptTag], in.DeletionProtection)
	}
	if in.Status == "deleting" {
		return nil
	}
	err = api.DeleteInstance(ctx, id)
	if err != nil && !errors.Is(err, ErrRDSNotFound) {
		return fmt.Errorf("rds_restore: delete instance %s: %w", id, err)
	}
	return nil
}

// rdsInstanceHost is the instance and region an RDS instance endpoint
// (<instance>.<id>.<region>.rds.amazonaws.com[.cn]) names. Aurora cluster
// endpoints, RDS Proxy and custom DNS name no instance.
func rdsInstanceHost(host string) (string, string, bool) {
	host = normHost(host)
	rest, ok := strings.CutSuffix(host, ".rds.amazonaws.com")
	if !ok {
		rest, ok = strings.CutSuffix(host, ".rds.amazonaws.com.cn")
	}
	if !ok {
		return "", "", false
	}
	labels := strings.Split(rest, ".")
	if len(labels) != 3 || !rdsClusterID.MatchString(labels[0]) ||
		!rdsInstanceLabel.MatchString(labels[1]) || !auroraRegion.MatchString(labels[2]) {
		return "", "", false
	}
	return labels[0], labels[2], true
}

var (
	_ SnapshotAPI = (*RDSRestoreAPI)(nil)
	_ Describer   = (*RDSRestoreAPI)(nil)
	_ Finder      = (*RDSRestoreAPI)(nil)
)

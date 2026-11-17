package clone

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// AuroraAdapter is the clone.provider value and receipt adapter of Aurora
// copy-on-write clones (lab-05c).
const AuroraAdapter = "aurora_clone"

// Aurora clone errors. Each is distinguishable with errors.Is.
var (
	// ErrInvalidAuroraConfig: a region, instance class, TTL, clone name or
	// DSN outside the accepted shape, or a cluster pg_sage did not make.
	ErrInvalidAuroraConfig = errors.New("clone: invalid aurora_clone configuration")
	// ErrNotAuroraCluster: the monitored database is not reached through an
	// Aurora PostgreSQL cluster endpoint, so there is no source to clone.
	ErrNotAuroraCluster = errors.New("clone: the monitored database is not an Aurora cluster")
	// ErrAuroraCloneLimit: the source already has the most copy-on-write
	// clones Aurora makes; the next would be a full copy at full price.
	ErrAuroraCloneLimit = errors.New("clone: the Aurora copy-on-write clone limit is reached")
	// ErrAuroraNotFound: RDS describes no such cluster or instance.
	ErrAuroraNotFound = errors.New("clone: aurora resource not found")
	// ErrAuroraAlreadyExists: RDS already has a cluster or instance of that
	// name, as when the SDK retries a restore RDS accepted.
	ErrAuroraAlreadyExists = errors.New("clone: aurora resource already exists")
)

// Aurora bounds, tags and defaults.
const (
	// AuroraMaxCopyOnWriteClones is how many copy-on-write clones one
	// source may have (clones of clones included); Aurora makes the next a
	// full copy.
	AuroraMaxCopyOnWriteClones = 15
	// AuroraMinTTL and AuroraMaxTTL bound clone.aurora.ttl_minutes: a
	// clone takes 10 to 20 minutes to be ready, and a leaked one holds
	// production data for at most a day.
	AuroraMinTTL = 30 * time.Minute
	AuroraMaxTTL = 24 * time.Hour
	// AuroraServerlessClass is the instance class of Serverless v2.
	AuroraServerlessClass = "db.serverless"
	// AuroraReceiptTag names the receipt resource name a cluster was made
	// for: the only proof pg_sage made it, and how the reaper finds it.
	AuroraReceiptTag = "pg-sage-receipt"
	// AuroraExpiresTag is when pg_sage's reaper may destroy the clone.
	AuroraExpiresTag = "pg-sage-expires-at"
	// auroraEngine is the only engine cloned.
	auroraEngine = "aurora-postgresql"
	// auroraTypicalReady is AWS's figure for a copy-on-write clone and its
	// first instance: 10 to 20 minutes.
	auroraTypicalReady = 15 * time.Minute
	defaultAuroraWait  = 40 * time.Minute
	defaultAuroraPoll  = 15 * time.Second
	auroraListPages    = 50
)

var (
	auroraRegion = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
	auroraClass  = regexp.MustCompile(`^db\.[a-z0-9]+(\.[a-z0-9]+)*$`)
	// auroraOwnedName is the only cluster name pg_sage gives (ResourceName)
	// and the only one it ever deletes.
	auroraOwnedName = regexp.MustCompile(`^sage-[a-z]+(-[a-z]+)*-[0-9a-f]{8}$`)
)

// AuroraProduction is the monitored (production) database a clone is taken
// from: its DSN, which names the source's cluster endpoint and which the
// clone's DSN reuses (same users, copied data).
type AuroraProduction struct{ DSN string }

// AuroraConfig configures an AuroraCloneProvider.
type AuroraConfig struct {
	// Region is the AWS region of the monitored cluster and its clones.
	Region string
	// InstanceClass is the clone instance's class; "": the source
	// writer's (db.serverless for a Serverless v2 writer).
	InstanceClass string
	// TTL is the longest a clone may live; it is tagged on the cluster.
	TTL time.Duration
	// API is the RDS API with the AWS credential chain; nil, or an error,
	// makes the provider unavailable.
	API func(context.Context) (AuroraAPI, error)
	// Production reads production's DSN at each Create; nil makes Create
	// unavailable (an adapter that only destroys, as the reaper's).
	Production  func(context.Context) (AuroraProduction, error)
	WaitTimeout time.Duration // bound of one create or destroy; 0: 40 minutes
	PollEvery   time.Duration // status poll period; 0: 15 seconds
	Now         func() time.Time
}

// AuroraCloneProvider clones by restoring a copy-on-write clone of the
// monitored Aurora PostgreSQL cluster and adding one instance (lab-05c).
// Every cluster is tagged with its receipt name, so the reaper finds a
// leaked one; Aurora has no server-side expiry.
type AuroraCloneProvider struct{ cfg AuroraConfig }

// NewAuroraCloneProvider validates cfg; it contacts nothing.
func NewAuroraCloneProvider(cfg AuroraConfig) (*AuroraCloneProvider, error) {
	if !auroraRegion.MatchString(cfg.Region) {
		return nil, fmt.Errorf("%w: region %q must be an AWS region such as us-east-1",
			ErrInvalidAuroraConfig, cfg.Region)
	}
	if cfg.InstanceClass != "" && !auroraClass.MatchString(cfg.InstanceClass) {
		return nil, fmt.Errorf("%w: instance class %q must be an RDS class such as "+
			"db.r6g.large or db.serverless", ErrInvalidAuroraConfig, cfg.InstanceClass)
	}
	if cfg.TTL < AuroraMinTTL || cfg.TTL > AuroraMaxTTL {
		return nil, fmt.Errorf("%w: clone TTL %s must be within %s-%s",
			ErrInvalidAuroraConfig, cfg.TTL, AuroraMinTTL, AuroraMaxTTL)
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = defaultAuroraWait
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultAuroraPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AuroraCloneProvider{cfg: cfg}, nil
}

// client is the RDS API, or ErrCloneUnavailable without AWS credentials:
// no clone can be made or deleted, so rehearsals are recommend-only.
func (p *AuroraCloneProvider) client(ctx context.Context) (AuroraAPI, error) {
	if p.cfg.API == nil {
		return nil, fmt.Errorf("%w: aurora_clone has no AWS credentials or configuration; "+
			"rehearsals are recommend-only until then", ErrCloneUnavailable)
	}
	api, err := p.cfg.API(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: aurora_clone has no usable AWS credentials (the default "+
			"chain: environment, shared config, web identity or instance role): %w; "+
			"rehearsals are recommend-only until then", ErrCloneUnavailable, err)
	}
	if api == nil {
		return nil, fmt.Errorf("%w: aurora_clone has no AWS credentials", ErrCloneUnavailable)
	}
	return api, nil
}

// Describe: an Aurora rehearsal clone is a copy-on-write clone, ready in
// 10 to 20 minutes by AWS's figure, living at most TTL.
func (p *AuroraCloneProvider) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindCopyOnWrite, TypicalReady: auroraTypicalReady,
		MaxTTL: p.cfg.TTL}, nil
}

// SnapshotAge is zero: the clone is restored to the source's latest
// restorable time when it is created, not from an older snapshot.
func (p *AuroraCloneProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}

// TargetScope is the receipt scope of this provider's clones.
func (p *AuroraCloneProvider) TargetScope() string { return "aws/" + p.cfg.Region }

// FindByName is the id of the cluster tagged with receipt name name (one
// pg_sage gives, sage-<purpose>-<8 hex>); found is false when none is.
func (p *AuroraCloneProvider) FindByName(ctx context.Context, name string) (string, bool,
	error) {
	api, err := p.client(ctx)
	if err != nil {
		return "", false, err
	}
	if !auroraOwnedName.MatchString(name) {
		return "", false, fmt.Errorf("%w: %q is not a pg_sage clone name "+
			"(sage-<purpose>-<8 hex>)", ErrInvalidAuroraConfig, name)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	clusters, err := api.ListClusters(ctx)
	if err != nil {
		return "", false, fmt.Errorf("aurora_clone: find cluster %s: %w", name, err)
	}
	for _, c := range clusters {
		if c.ID == name && c.Tags[AuroraReceiptTag] == name {
			return c.ID, true, nil
		}
	}
	return "", false, nil
}

// Destroy deletes a cluster this provider made: its instances first, then
// the cluster without a final snapshot. The cluster is read first: one not
// named sage-<purpose>-<8 hex>, not tagged with that name, or protected
// from deletion is refused and nothing is deleted. A cluster gone or
// already deleting counts as deleted. A destroy cut short by ctx leaves
// the instances deleting; the next call carries on.
func (p *AuroraCloneProvider) Destroy(ctx context.Context, target Clone) error {
	api, err := p.client(ctx)
	if err != nil {
		return err
	}
	if !auroraOwnedName.MatchString(target.ID) {
		return fmt.Errorf("%w: %q is not a pg_sage clone cluster (sage-<purpose>-<8 hex>); "+
			"nothing deleted", ErrInvalidAuroraConfig, target.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	return p.deleteOwned(ctx, api, target.ID)
}

func (p *AuroraCloneProvider) deleteOwned(ctx context.Context, api AuroraAPI,
	id string) error {
	c, err := api.DescribeCluster(ctx, id)
	if errors.Is(err, ErrAuroraNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("aurora_clone: read cluster %s before deleting it: %w", id, err)
	}
	if c.Tags[AuroraReceiptTag] != id || c.DeletionProtection {
		return fmt.Errorf("%w: cluster %s (tag %s=%q, deletion protection %v) is not a "+
			"pg_sage clone cluster; nothing deleted", ErrInvalidAuroraConfig, id,
			AuroraReceiptTag, c.Tags[AuroraReceiptTag], c.DeletionProtection)
	}
	if c.Status == "deleting" {
		return nil
	}
	for _, m := range c.Members {
		if err := p.deleteInstance(ctx, api, id, m.ID); err != nil {
			return err
		}
	}
	err = api.DeleteCluster(ctx, id)
	if err != nil && !errors.Is(err, ErrAuroraNotFound) {
		return fmt.Errorf("aurora_clone: delete cluster %s: %w", id, err)
	}
	return nil
}

// deleteInstance deletes one instance of clone cluster id and waits until
// RDS no longer describes it (a cluster with instances cannot be deleted).
func (p *AuroraCloneProvider) deleteInstance(ctx context.Context, api AuroraAPI, id,
	instance string) error {
	in, err := api.DescribeInstance(ctx, instance)
	if errors.Is(err, ErrAuroraNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("aurora_clone: read instance %s of %s: %w", instance, id, err)
	}
	if in.Status != "deleting" {
		err := api.DeleteInstance(ctx, instance)
		if err != nil && !errors.Is(err, ErrAuroraNotFound) {
			return fmt.Errorf("aurora_clone: delete instance %s of %s: %w", instance, id, err)
		}
	}
	for {
		_, err := api.DescribeInstance(ctx, instance)
		if errors.Is(err, ErrAuroraNotFound) {
			return nil
		}
		if err != nil && !Throttled(err) {
			return fmt.Errorf("aurora_clone: wait for instance %s of %s to go: %w", instance,
				id, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("aurora_clone: instance %s of %s is still deleting; the next "+
				"destroy carries on: %w", instance, id, ctx.Err())
		case <-time.After(p.cfg.PollEvery):
		}
	}
}

var (
	_ Provider  = (*AuroraCloneProvider)(nil)
	_ Describer = (*AuroraCloneProvider)(nil)
	_ Finder    = (*AuroraCloneProvider)(nil)
)

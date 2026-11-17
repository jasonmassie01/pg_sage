package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Create restores cluster spec.ResourceName (sage-<purpose>-<8 hex>, the
// receipt's) as a copy-on-write clone of the monitored cluster at its
// latest restorable time, in the source's subnet group, security groups
// and cluster parameter group, tagged with the receipt name and the
// expiry; adds one instance (clone.aurora.instance_class, else the
// writer's class); waits for both to be available; and returns the
// monitored DSN on the clone's endpoint. The source must be reached by an
// Aurora PostgreSQL cluster endpoint and have fewer than 15 copy-on-write
// clones. A failure after the cluster exists deletes it before the error
// returns; a wait cut short leaves it to the reaper, which finds it by
// tag (the receipt is create_uncertain). The clone id is the cluster id.
func (p *AuroraCloneProvider) Create(ctx context.Context, spec CloneSpec) (Clone, error) {
	api, err := p.client(ctx)
	if err != nil {
		return Clone{}, err
	}
	if !auroraOwnedName.MatchString(spec.ResourceName) {
		return Clone{}, fmt.Errorf("%w: clone name %q is not sage-<purpose>-<8 hex> (the "+
			"receipt's resource name); nothing created", ErrInvalidAuroraConfig,
			spec.ResourceName)
	}
	prod, err := p.production(ctx)
	if err != nil {
		return Clone{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	run := &auroraCreate{p: p, api: api, prod: prod, name: spec.ResourceName,
		expire: p.cfg.Now().Add(p.cfg.TTL).UTC()}
	if err := run.resolveSource(ctx); err != nil {
		return Clone{}, err
	}
	return run.create(ctx)
}

// auroraSource is the monitored database a clone is taken from.
type auroraSource struct {
	dsn, host string
	cluster   AuroraCluster
	writer    AuroraInstance
}

// production reads production's DSN and the cluster its host names.
// Without it no clone can be told apart from production, so Create is
// unavailable (fail closed). Errors never quote the DSN.
func (p *AuroraCloneProvider) production(ctx context.Context) (auroraSource, error) {
	if p.cfg.Production == nil {
		return auroraSource{}, fmt.Errorf("%w: aurora_clone has no production identity to "+
			"tell a clone from production", ErrCloneUnavailable)
	}
	prod, err := p.cfg.Production(ctx)
	if err != nil {
		return auroraSource{}, fmt.Errorf("%w: read the monitored Aurora database's "+
			"identity: %w", ErrCloneUnavailable, err)
	}
	if blank(prod.DSN) {
		return auroraSource{}, fmt.Errorf("%w: the monitored Aurora database has no DSN",
			ErrCloneUnavailable)
	}
	host, err := dsnHost(prod.DSN)
	if err != nil {
		return auroraSource{}, err
	}
	return auroraSource{dsn: prod.DSN, host: host}, nil
}

// auroraCreate is one Create: what it asked for, so cleanup deletes
// exactly that.
type auroraCreate struct {
	p        *AuroraCloneProvider
	api      AuroraAPI
	prod     auroraSource
	name     string
	instance string
	expire   time.Time
}

// resolveSource checks that production's host is an Aurora PostgreSQL
// cluster endpoint of the configured region, reads the cluster and its
// writer, and counts the source's copy-on-write clones.
func (r *auroraCreate) resolveSource(ctx context.Context) error {
	id, region, ok := auroraClusterHost(r.prod.host)
	if !ok {
		return fmt.Errorf("%w: the monitored host %s is not an Aurora cluster endpoint "+
			"(<cluster>.cluster-<id>.<region>.rds.amazonaws.com or its cluster-ro- reader); "+
			"connect pg_sage through the cluster endpoint", ErrNotAuroraCluster, r.prod.host)
	}
	if region != r.p.cfg.Region {
		return fmt.Errorf("%w: the monitored cluster %s is in %s, not clone.aurora.region %s",
			ErrInvalidAuroraConfig, id, region, r.p.cfg.Region)
	}
	c, err := r.api.DescribeCluster(ctx, id)
	if errors.Is(err, ErrAuroraNotFound) {
		return fmt.Errorf("%w: RDS describes no cluster %s in %s (it is not an Aurora "+
			"cluster pg_sage can read): %w", ErrNotAuroraCluster, id, region, err)
	}
	if err != nil {
		return fmt.Errorf("aurora_clone: read the monitored cluster %s: %w", id, err)
	}
	if c.Engine != auroraEngine || !(sameHost(c.Endpoint, r.prod.host) ||
		sameHost(c.ReaderEndpoint, r.prod.host)) {
		return fmt.Errorf("%w: cluster %s (engine %q) is not an Aurora cluster serving the "+
			"monitored host %s as aurora-postgresql", ErrNotAuroraCluster, id, c.Engine,
			r.prod.host)
	}
	r.prod.cluster = c
	if err := r.checkCloneLimit(ctx); err != nil {
		return err
	}
	return r.readWriter(ctx)
}

// checkCloneLimit refuses a 16th copy-on-write clone of the source: Aurora
// would make it a full copy at full price. A count that cannot be read
// refuses the create (fail closed).
func (r *auroraCreate) checkCloneLimit(ctx context.Context) error {
	src := r.prod.cluster
	if src.CloneGroupID == "" {
		return nil
	}
	clusters, err := r.api.ListClusters(ctx)
	if err != nil {
		return fmt.Errorf("aurora_clone: count the copy-on-write clones of %s: %w", src.ID,
			err)
	}
	n := 0
	for _, c := range clusters {
		if c.CloneGroupID == src.CloneGroupID && c.ID != src.ID {
			n++
		}
	}
	if n >= AuroraMaxCopyOnWriteClones {
		return fmt.Errorf("%w: cluster %s already has %d copy-on-write clones and Aurora "+
			"allows %d; the next would be a full copy at full price, so nothing was "+
			"created: delete unused clones of clone group %s", ErrAuroraCloneLimit, src.ID, n,
			AuroraMaxCopyOnWriteClones, src.CloneGroupID)
	}
	return nil
}

func (r *auroraCreate) readWriter(ctx context.Context) error {
	src := r.prod.cluster
	writer := ""
	for _, m := range src.Members {
		if m.Writer {
			writer = m.ID
		}
	}
	if writer == "" {
		return fmt.Errorf("aurora_clone: cluster %s has no writer instance to size the clone "+
			"from", src.ID)
	}
	w, err := r.api.DescribeInstance(ctx, writer)
	if err != nil {
		return fmt.Errorf("aurora_clone: read writer %s of %s: %w", writer, src.ID, err)
	}
	r.prod.writer = w
	class := r.class()
	if class == AuroraServerlessClass && src.Serverless == nil {
		return fmt.Errorf("%w: instance class %s needs a Serverless v2 source, and cluster %s "+
			"has no Serverless v2 capacity range: set clone.aurora.instance_class to a "+
			"provisioned class", ErrInvalidAuroraConfig, class, src.ID)
	}
	if class == "" {
		return fmt.Errorf("aurora_clone: writer %s of %s has no instance class: set "+
			"clone.aurora.instance_class", writer, src.ID)
	}
	return nil
}

// class is clone.aurora.instance_class, else the writer's.
func (r *auroraCreate) class() string {
	if r.p.cfg.InstanceClass != "" {
		return r.p.cfg.InstanceClass
	}
	return r.prod.writer.Class
}

func (r *auroraCreate) tags() map[string]string {
	return map[string]string{AuroraReceiptTag: r.name,
		AuroraExpiresTag: r.expire.Format(time.RFC3339)}
}

func (r *auroraCreate) create(ctx context.Context) (Clone, error) {
	src := r.prod.cluster
	created, err := r.api.RestoreCluster(ctx, AuroraRestoreRequest{ID: r.name,
		Source: src.ID, SubnetGroup: src.SubnetGroup, ParameterGroup: src.ParameterGroup,
		SecurityGroups: src.SecurityGroups, Serverless: src.Serverless, Tags: r.tags()})
	if err != nil {
		err = fmt.Errorf("aurora_clone: restore %s from %s: %w", r.name, src.ID, err)
		// RDS refused it (4xx): nothing was made. A 5xx or a lost reply may
		// follow a restore RDS accepted, so it is looked for by tag.
		var status *ProviderStatusError
		if errors.As(err, &status) && status.StatusCode < 500 {
			return Clone{}, err
		}
		return Clone{}, r.cleanupByTag(err)
	}
	if created.ID != r.name {
		return Clone{}, r.cleanup(fmt.Errorf("aurora_clone: the restore reply names cluster "+
			"%q, not %s", created.ID, r.name))
	}
	if err := r.addInstance(ctx); err != nil {
		return Clone{}, err
	}
	c, err := r.waitReady(ctx)
	if err != nil {
		return Clone{}, err
	}
	dsn, err := r.dsn(c)
	if err != nil {
		return Clone{}, r.cleanup(err)
	}
	return Clone{DSN: dsn, ID: r.name, CreatedFrom: r.p.cfg.Now()}, nil
}

// addInstance adds the clone's one instance; any failure deletes the
// cluster (and an instance the failed call may have made).
func (r *auroraCreate) addInstance(ctx context.Context) error {
	r.instance = r.name + "-1"
	w := r.prod.writer
	_, err := r.api.CreateInstance(ctx, AuroraInstanceRequest{ID: r.instance,
		ClusterID: r.name, Class: r.class(), ParameterGroup: w.ParameterGroup,
		PubliclyAccessible: w.PubliclyAccessible, Tags: r.tags()})
	if err != nil {
		return r.cleanup(fmt.Errorf("aurora_clone: create instance %s (%s) in %s: %w",
			r.instance, r.class(), r.name, err))
	}
	return nil
}

// waitReady polls the cluster and its instance until both are available.
// A failed status deletes the clone; the deadline leaves it to the reaper.
func (r *auroraCreate) waitReady(ctx context.Context) (AuroraCluster, error) {
	for {
		c, cerr := r.api.DescribeCluster(ctx, r.name)
		in, ierr := r.api.DescribeInstance(ctx, r.instance)
		for _, err := range []error{cerr, ierr} {
			if err != nil && !Throttled(err) && ctx.Err() == nil {
				return AuroraCluster{}, r.cleanup(fmt.Errorf("aurora_clone: poll clone %s: %w",
					r.name, err))
			}
		}
		if cerr == nil && ierr == nil {
			if bad := auroraFailedStatus(c.Status, in.Status); bad != "" {
				return AuroraCluster{}, r.cleanup(fmt.Errorf("aurora_clone: clone %s %s "+
					"(cluster %s, instance %s)", r.name, bad, c.Status, in.Status))
			}
			if c.Status == "available" && in.Status == "available" {
				return c, nil
			}
		}
		if err := auroraPollPause(ctx, r.p.cfg.PollEvery); err != nil {
			return AuroraCluster{}, fmt.Errorf("aurora_clone: clone %s not available after "+
				"%s; left to the reaper, which finds it by tag %s: %w", r.name,
				r.p.cfg.WaitTimeout, AuroraReceiptTag, err)
		}
	}
}

// auroraFailedStatus is "failed" when either status is one a clone does
// not recover from.
func auroraFailedStatus(statuses ...string) string {
	for _, s := range statuses {
		switch {
		case s == "failed", s == "deleting", s == "inaccessible-encryption-credentials",
			s == "storage-full", strings.HasPrefix(s, "incompatible-"):
			return "failed"
		}
	}
	return ""
}

// dsn is production's DSN on the clone's writer endpoint, checked not to
// be production's host.
func (r *auroraCreate) dsn(c AuroraCluster) (string, error) {
	if blank(c.Endpoint) {
		return "", fmt.Errorf("aurora_clone: clone %s has no endpoint", r.name)
	}
	if sameHost(c.Endpoint, r.prod.host) || sameHost(c.Endpoint, r.prod.cluster.Endpoint) ||
		sameHost(c.Endpoint, r.prod.cluster.ReaderEndpoint) {
		return "", fmt.Errorf("%w: clone %s answers on production's host %s",
			ErrCloneIsProduction, r.name, c.Endpoint)
	}
	port := c.Port
	if port == 0 {
		port = r.prod.cluster.Port
	}
	return auroraCloneDSN(r.prod.dsn, r.prod.host, c.Endpoint, port)
}

// cleanup deletes the clone this Create made, on a fresh bounded context
// (the caller's may have ended). A delete that fails leaves the clone to
// the reaper, and says so.
func (r *auroraCreate) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := r.p.deleteOwned(ctx, r.api, r.name); err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

// cleanupByTag deletes the cluster a restore whose reply was lost may have
// made, found by its tag.
func (r *auroraCreate) cleanupByTag(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, found, err := r.p.FindByName(ctx, r.name)
	if err == nil && found {
		err = r.p.deleteOwned(ctx, r.api, r.name)
	}
	if err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

func (r *auroraCreate) leftBehind(err error) error {
	return fmt.Errorf("%w: aurora_clone cleanup failed; the reaper finds cluster %s by "+
		"tag %s (it expires at %s): %w", ErrCloneLeftBehind, r.name, AuroraReceiptTag,
		r.expire.Format(time.RFC3339), err)
}

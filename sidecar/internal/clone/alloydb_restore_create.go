package clone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// errAlloyDBOpFailed: an AlloyDB operation finished with an error.
var errAlloyDBOpFailed = errors.New("the AlloyDB operation failed")

// requireProduction refuses a restore without a production identity
// before Google is contacted: the reaper's adapter only destroys.
func (a *AlloyDBRestoreAPI) requireProduction() error {
	if a.cfg.Production == nil {
		return fmt.Errorf("%w: alloydb_restore has no production identity to tell a clone "+
			"from production", ErrCloneUnavailable)
	}
	return nil
}

// alloyDBSource is the monitored cluster a restore is taken from, its
// primary instance the monitored DSN reaches, and the kind of that IP.
type alloyDBSource struct {
	dsn, host, kind string
	cluster         alloyDBCluster
	primary         alloyDBInstance
	instances       []alloyDBInstance
}

// LatestSnapshot is the source cluster at AlloyDBRestoreLag ago: a point
// in time of its continuous backup.
func (a *AlloyDBRestoreAPI) LatestSnapshot(ctx context.Context) (Snapshot, error) {
	if err := a.requireProduction(); err != nil {
		return Snapshot{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, alloyDBCallTimeout)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Snapshot{}, err
	}
	pit := a.cfg.Now().Add(-AlloyDBRestoreLag).UTC().Truncate(time.Second)
	if err := src.restorable(pit); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: src.cluster.Name, CreatedAt: pit}, nil
}

// restorable refuses a point in time outside the continuous backup's
// recovery window.
func (s alloyDBSource) restorable(pit time.Time) error {
	earliest := s.cluster.ContinuousBackupInfo.EarliestRestorableTime
	if earliest.IsZero() {
		return fmt.Errorf("%w: AlloyDB reports no earliest restorable time for cluster %s",
			ErrCloneUnavailable, s.cluster.Name)
	}
	if pit.Before(earliest) {
		return fmt.Errorf("%w: the point in time %s is before cluster %s's earliest "+
			"restorable time %s (continuous backup was turned on recently)",
			ErrCloneUnavailable, pit.Format(time.RFC3339), s.cluster.Name,
			earliest.Format(time.RFC3339))
	}
	return nil
}

// source reads production's DSN and the configured cluster, and checks it
// is a running AlloyDB for PostgreSQL primary cluster with continuous
// backup whose primary instance the DSN reaches by one of its IPs: pg_sage
// restores only what it monitors, and reaches the clone the same way.
// Errors never quote the DSN.
func (a *AlloyDBRestoreAPI) source(ctx context.Context, api *alloyDBHTTP) (alloyDBSource,
	error) {
	prod, err := a.cfg.Production(ctx)
	if err != nil {
		return alloyDBSource{}, fmt.Errorf("%w: read the monitored AlloyDB database's "+
			"identity: %w", ErrCloneUnavailable, err)
	}
	if blank(prod.DSN) {
		return alloyDBSource{}, fmt.Errorf("%w: the monitored AlloyDB database has no DSN",
			ErrCloneUnavailable)
	}
	host, err := dsnHost(prod.DSN)
	if err != nil {
		return alloyDBSource{}, fmt.Errorf("%w: %w", ErrInvalidAlloyDBConfig, err)
	}
	c, err := a.sourceCluster(ctx, api)
	if err != nil {
		return alloyDBSource{}, err
	}
	src := alloyDBSource{dsn: prod.DSN, host: host, cluster: c}
	if src.instances, err = api.ListInstances(ctx, a.cfg.Cluster); err != nil {
		return alloyDBSource{}, fmt.Errorf("alloydb_restore: list the instances of cluster "+
			"%s: %w", a.cfg.Cluster, err)
	}
	for _, in := range src.instances {
		if in.InstanceType == "PRIMARY" && in.ipKind(host) != "" {
			src.primary, src.kind = in, in.ipKind(host)
		}
	}
	if src.kind == "" {
		return alloyDBSource{}, fmt.Errorf("%w: the monitored host %s is not an IP of "+
			"cluster %s's primary instance; connect pg_sage to the primary's private or "+
			"public IP (not a read pool, the Auth Proxy or a DNS name)",
			ErrNotAlloyDBCluster, host, a.cfg.Cluster)
	}
	return src, src.ready()
}

// sourceCluster is the configured cluster, checked to be a PostgreSQL
// primary cluster with a network and continuous backup.
func (a *AlloyDBRestoreAPI) sourceCluster(ctx context.Context,
	api *alloyDBHTTP) (alloyDBCluster, error) {
	c, err := api.GetCluster(ctx, a.cfg.Cluster)
	if errors.Is(err, ErrAlloyDBNotFound) {
		return c, fmt.Errorf("%w: %s has no cluster %s: %w", ErrNotAlloyDBCluster,
			api.parent, a.cfg.Cluster, err)
	}
	if err != nil {
		return c, fmt.Errorf("alloydb_restore: read the monitored cluster %s: %w",
			a.cfg.Cluster, err)
	}
	if c.ClusterType != "PRIMARY" || !strings.HasPrefix(c.DatabaseVersion, "POSTGRES_") {
		return c, fmt.Errorf("%w: cluster %s (%s, %q) is not an AlloyDB for PostgreSQL "+
			"primary cluster", ErrNotAlloyDBCluster, a.cfg.Cluster, c.ClusterType,
			c.DatabaseVersion)
	}
	if !c.ContinuousBackupConfig.Enabled {
		return c, fmt.Errorf("%w: cluster %s has continuousBackupConfig.enabled false, so "+
			"it has no point in time to restore; turn continuous backup on (gcloud alloydb "+
			"clusters update %s --enable-continuous-backup) or choose another "+
			"clone.provider", ErrAlloyDBContinuousBackupOff, a.cfg.Cluster, a.cfg.Cluster)
	}
	if rawEmpty(c.NetworkConfig) && blank(c.Network) {
		return c, fmt.Errorf("%w: cluster %s names no network to restore into",
			ErrNotAlloyDBCluster, a.cfg.Cluster)
	}
	return c, nil
}

// ready refuses a source not READY, or whose primary is not.
func (s alloyDBSource) ready() error {
	if s.cluster.State != "READY" || s.primary.State != "READY" {
		return fmt.Errorf("%w: cluster %s is %s and its primary %s, not READY",
			ErrCloneUnavailable, s.cluster.Name, s.cluster.State, s.primary.State)
	}
	return nil
}

func rawEmpty(m json.RawMessage) bool {
	s := strings.TrimSpace(string(m))
	return s == "" || s == "null" || s == "{}"
}

// Restore restores the monitored cluster at snap's point in time as
// spec.ResourceName (sage-<purpose>-<8 hex>, the receipt's), labelled with
// the receipt name and expiry, adds a primary instance like the source's,
// all within the restore deadline, and returns the monitored DSN on the
// new primary's IP of the same kind. A failure after the cluster exists
// deletes it before the error returns; a wait cut short leaves it to the
// reaper, which finds it by name.
func (a *AlloyDBRestoreAPI) Restore(ctx context.Context, snap Snapshot,
	spec CloneSpec) (Clone, error) {
	if err := a.requireProduction(); err != nil {
		return Clone{}, err
	}
	if err := callerOutlasts(ctx, a.cfg.Deadline); err != nil {
		return Clone{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Clone{}, err
	}
	if err := checkAlloyDBName(spec.ResourceName, "nothing created"); err != nil {
		return Clone{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Deadline)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Clone{}, err
	}
	if snap.ID != src.cluster.Name || snap.CreatedAt.After(a.cfg.Now()) {
		return Clone{}, fmt.Errorf("%w: snapshot %q at %s is not a past point in time of "+
			"cluster %s; nothing created", ErrInvalidAlloyDBConfig, snap.ID,
			snap.CreatedAt.Format(time.RFC3339), src.cluster.Name)
	}
	if err := src.restorable(snap.CreatedAt); err != nil {
		return Clone{}, err
	}
	run := &adbRestore{a: a, api: api, src: src, name: spec.ResourceName,
		pit: snap.CreatedAt.UTC(), expire: a.cfg.Now().Add(a.cfg.TTL).UTC()}
	return run.restore(ctx)
}

// adbRestore is one Restore: what it asked for, so cleanup deletes
// exactly that.
type adbRestore struct {
	a           *AlloyDBRestoreAPI
	api         *alloyDBHTTP
	src         alloyDBSource
	name        string
	pit, expire time.Time
}

// labels marks a resource pg_sage's: the receipt name and expiry, over
// base (the source cluster's labels, kept for cost attribution).
func (r *adbRestore) labels(base map[string]string) map[string]string {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	out[AlloyDBReceiptLabel] = r.name
	out[AlloyDBExpiresLabel] = strconv.FormatInt(r.expire.Unix(), 10)
	return out
}

// clusterBody is the new cluster: the source's network and encryption,
// labelled.
func (r *adbRestore) clusterBody() map[string]any {
	body := map[string]any{"labels": r.labels(r.src.cluster.Labels)}
	if rawEmpty(r.src.cluster.NetworkConfig) {
		body["network"] = r.src.cluster.Network
	} else {
		body["networkConfig"] = r.src.cluster.NetworkConfig
	}
	if !rawEmpty(r.src.cluster.EncryptionConfig) {
		body["encryptionConfig"] = r.src.cluster.EncryptionConfig
	}
	return body
}

func (r *adbRestore) restore(ctx context.Context) (Clone, error) {
	op, err := r.api.Restore(ctx, map[string]any{"clusterId": r.name,
		"cluster": r.clusterBody(), "continuousBackupSource": map[string]string{
			"cluster": r.src.cluster.Name, "pointInTime": r.pit.Format(time.RFC3339)}})
	if err != nil {
		err = fmt.Errorf("alloydb_restore: restore %s to %s: %w", r.src.cluster.Name,
			r.name, err)
		// Google refused it (4xx, an existing id included): nothing was
		// made. A 5xx or a lost reply may follow a restore it accepted.
		var status *ProviderStatusError
		if errors.As(err, &status) && status.StatusCode < 500 {
			return Clone{}, err
		}
		return Clone{}, r.cleanupByName(err)
	}
	if err := r.wait(ctx, op.Name, "restore"); err != nil {
		return Clone{}, err
	}
	c, err := r.api.GetCluster(ctx, r.name)
	if err == nil && c.State != "READY" && c.State != "EMPTY" {
		err = fmt.Errorf("cluster %s is %s after its restore finished", r.name, c.State)
	}
	if err != nil {
		return Clone{}, r.cleanup(fmt.Errorf("alloydb_restore: read the clone: %w", err))
	}
	if c.Labels[AlloyDBReceiptLabel] != r.name {
		// Never delete what pg_sage cannot prove is its own; the reaper
		// will not find it either, so the operator must.
		return Clone{}, fmt.Errorf("%w: alloydb_restore: restored cluster %s does not "+
			"carry the label %s=%s, so pg_sage cannot prove it is its own and did not "+
			"delete it; delete it by hand (gcloud alloydb clusters delete %s --region %s "+
			"--force)", ErrCloneLeftBehind, r.name, AlloyDBReceiptLabel, r.name, r.name,
			r.a.cfg.Location)
	}
	return r.primary(ctx)
}

// primary adds the clone's primary instance: the source primary's machine
// and network, zonal (a rehearsal never pays for a standby), labelled.
func (r *adbRestore) primary(ctx context.Context) (Clone, error) {
	body := map[string]any{"instanceType": "PRIMARY", "availabilityType": "ZONAL",
		"labels": r.labels(nil)}
	if !rawEmpty(r.src.primary.MachineConfig) {
		body["machineConfig"] = r.src.primary.MachineConfig
	}
	if !rawEmpty(r.src.primary.NetworkConfig) {
		body["networkConfig"] = r.src.primary.NetworkConfig
	}
	op, err := r.api.CreateInstance(ctx, r.name, r.name, body)
	if err != nil {
		if ctx.Err() != nil {
			return Clone{}, r.leftToReaper("primary instance", ctx.Err())
		}
		return Clone{}, r.cleanup(fmt.Errorf("alloydb_restore: create the primary "+
			"instance of %s: %w", r.name, err))
	}
	if err := r.wait(ctx, op.Name, "primary instance"); err != nil {
		return Clone{}, err
	}
	in, err := r.api.GetInstance(ctx, r.name, r.name)
	if err == nil && in.State != "READY" {
		err = fmt.Errorf("instance %s is %s after its create finished", r.name, in.State)
	}
	if err != nil {
		return Clone{}, r.cleanup(fmt.Errorf("alloydb_restore: read the primary: %w", err))
	}
	dsn, err := r.dsn(in)
	if err != nil {
		return Clone{}, r.cleanup(err)
	}
	return Clone{DSN: dsn, ID: r.name, CreatedFrom: r.pit}, nil
}

// dsn is production's DSN on the clone primary's IP of the kind
// production's DSN uses, checked not to be one of production's IPs.
func (r *adbRestore) dsn(in alloyDBInstance) (string, error) {
	ip := in.ipOf(r.src.kind)
	if blank(ip) {
		return "", fmt.Errorf("alloydb_restore: clone %s has no %s IP", r.name, r.src.kind)
	}
	for _, prod := range r.src.instances {
		if prod.ipKind(ip) != "" || sameHost(ip, r.src.host) {
			return "", fmt.Errorf("%w: clone %s answers on production's IP %s",
				ErrCloneIsProduction, r.name, ip)
		}
	}
	dsn, err := auroraCloneDSN(r.src.dsn, r.src.host, ip, 5432)
	if err != nil {
		return "", fmt.Errorf("alloydb_restore: %w", err)
	}
	return dsn, nil
}

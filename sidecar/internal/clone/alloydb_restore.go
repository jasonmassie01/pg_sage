package clone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// alloydb_restore (lab-05f): AlloyDB has no copy-on-write clone, so a
// clone is a point-in-time restore of the monitored cluster's continuous
// backup to a new cluster, plus a primary instance: a full copy at the
// full cluster price, made only with clone.allow_full_copy.
// AlloyDBRestoreAPI is the SnapshotAPI of it; SnapshotProvider wraps it as
// a Provider.

// AlloyDB restore errors. Each is distinguishable with errors.Is.
var (
	// ErrInvalidAlloyDBConfig: a project, location, cluster, deadline, TTL,
	// clone name or DSN outside the accepted shape, or a cluster pg_sage
	// did not make.
	ErrInvalidAlloyDBConfig = errors.New("clone: invalid alloydb_restore configuration")
	// ErrNotAlloyDBCluster: the configured cluster is not an AlloyDB primary
	// cluster whose primary instance the monitored DSN reaches by its IP.
	ErrNotAlloyDBCluster = errors.New("clone: the monitored database is not the " +
		"configured AlloyDB primary cluster")
	// ErrAlloyDBContinuousBackupOff: the source cluster keeps no continuous
	// backup, so there is no point in time to restore.
	ErrAlloyDBContinuousBackupOff = errors.New("clone: the AlloyDB cluster has " +
		"continuous backup off")
	// ErrAlloyDBNotFound: AlloyDB describes no such resource.
	ErrAlloyDBNotFound = errors.New("clone: alloydb resource not found")
	// ErrAlloyDBAlreadyExists: AlloyDB already has a resource of that id.
	ErrAlloyDBAlreadyExists = errors.New("clone: alloydb resource already exists")
	// ErrAlloyDBPermission: Google refused the call (401 or 403); the error
	// names the IAM permission the call needs.
	ErrAlloyDBPermission = errors.New("clone: alloydb permission denied")
)

const (
	// AlloyDBReceiptLabel and AlloyDBExpiresLabel mark a restored cluster
	// and its instance: the receipt's name, so the reaper finds a leaked
	// one, and its expiry in Unix seconds. AlloyDB has no server-side
	// expiry.
	AlloyDBReceiptLabel = "pg-sage-receipt"
	AlloyDBExpiresLabel = "pg-sage-expires-at"
	// AlloyDBAPIBase is the AlloyDB Admin API.
	AlloyDBAPIBase = "https://alloydb.googleapis.com"
	// AlloyDBMaxTTL bounds a clone's life: a leaked one holds production
	// data for at most a day.
	AlloyDBMaxTTL = 24 * time.Hour
	// AlloyDBRestoreLag is how far before now the restore's point in time
	// is: continuous backup has the source's recent writes by then.
	AlloyDBRestoreLag  = 5 * time.Minute
	defaultAlloyDBPoll = 15 * time.Second
	// alloyDBCallTimeout bounds one lookup or destroy.
	alloyDBCallTimeout = 5 * time.Minute
)

var (
	alloyDBLocation = regexp.MustCompile(`^[a-z]+(-[a-z]+)+[0-9]+$`)
	alloyDBID       = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// AlloyDBProduction is the monitored (production) database a clone is
// restored from: its DSN names the source primary instance's IP, and the
// clone's DSN reuses it (same users, restored data).
type AlloyDBProduction struct{ DSN string }

// AlloyDBRestoreConfig configures an AlloyDBRestoreAPI.
type AlloyDBRestoreConfig struct {
	// Project, Location and Cluster name the monitored AlloyDB cluster;
	// clones are restored in the same project and region.
	Project, Location, Cluster string
	// Deadline bounds one restore, from the request until its primary
	// instance is ready (clone.restore_deadline_minutes). One not ready by
	// then is left to the reaper (an uncertain create).
	Deadline time.Duration
	// TTL is the longest a clone may live: its expiry label and MaxTTL.
	// It must exceed Deadline.
	TTL time.Duration
	// Token is the Google credential chain (lab-05e's GCP token seam);
	// nil, an error or an empty token makes the provider unavailable.
	Token func(context.Context) (CloudSQLTokenSource, error)
	// Production reads production's DSN at each restore; nil makes every
	// restore unavailable (an adapter that only destroys, as the reaper's).
	Production func(context.Context) (AlloyDBProduction, error)
	BaseURL    string        // "": AlloyDBAPIBase
	HTTPClient *http.Client  // nil: the egress floor's client
	PollEvery  time.Duration // operation poll period; 0: 15 seconds
	Now        func() time.Time
}

// AlloyDBRestoreAPI restores the monitored AlloyDB cluster to a new
// cluster labelled with its receipt name.
type AlloyDBRestoreAPI struct{ cfg AlloyDBRestoreConfig }

// NewAlloyDBRestoreAPI validates cfg; it contacts nothing.
func NewAlloyDBRestoreAPI(cfg AlloyDBRestoreConfig) (*AlloyDBRestoreAPI, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = AlloyDBAPIBase
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = egress.HTTPClient(0)
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultAlloyDBPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AlloyDBRestoreAPI{cfg: cfg}, nil
}

func (cfg AlloyDBRestoreConfig) validate() error {
	if !cloudSQLProjectID.MatchString(cfg.Project) {
		return fmt.Errorf("%w: project %q must be a Google Cloud project id such as "+
			"acme-prod-1234", ErrInvalidAlloyDBConfig, cfg.Project)
	}
	if !alloyDBLocation.MatchString(cfg.Location) {
		return fmt.Errorf("%w: location %q must be a Google Cloud region such as "+
			"us-central1 (not a zone)", ErrInvalidAlloyDBConfig, cfg.Location)
	}
	if !alloyDBID.MatchString(cfg.Cluster) || auroraOwnedName.MatchString(cfg.Cluster) {
		return fmt.Errorf("%w: cluster %q must be an AlloyDB cluster id (lowercase "+
			"letters, digits and hyphens) that is not a pg_sage clone",
			ErrInvalidAlloyDBConfig, cfg.Cluster)
	}
	if cfg.Deadline < RDSRestoreMinDeadline || cfg.Deadline > RDSRestoreMaxDeadline {
		return fmt.Errorf("%w: restore deadline %s must be within %s-%s",
			ErrInvalidAlloyDBConfig, cfg.Deadline, RDSRestoreMinDeadline, RDSRestoreMaxDeadline)
	}
	if cfg.TTL <= cfg.Deadline || cfg.TTL > AlloyDBMaxTTL {
		return fmt.Errorf("%w: clone TTL %s must exceed the restore deadline %s and be at "+
			"most %s", ErrInvalidAlloyDBConfig, cfg.TTL, cfg.Deadline, AlloyDBMaxTTL)
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%w: API base URL must be an http(s) URL",
				ErrInvalidAlloyDBConfig)
		}
	}
	return nil
}

// NewAlloyDBRestoreProvider is the alloydb_restore Provider:
// SnapshotProvider over an AlloyDBRestoreAPI of cfg.
func NewAlloyDBRestoreProvider(cfg AlloyDBRestoreConfig) (*SnapshotProvider, error) {
	api, err := NewAlloyDBRestoreAPI(cfg)
	if err != nil {
		return nil, err
	}
	return NewSnapshotProvider(api, api.cfg.Now)
}

// client is the Admin API with a usable token, or ErrCloneUnavailable.
func (a *AlloyDBRestoreAPI) client(ctx context.Context) (*alloyDBHTTP, error) {
	src, err := gcpToken(ctx, a.cfg.Token, AlloyDBRestoreAdapter)
	if err != nil {
		return nil, err
	}
	return &alloyDBHTTP{base: a.cfg.BaseURL, project: a.cfg.Project,
		parent: "projects/" + a.cfg.Project + "/locations/" + a.cfg.Location,
		token:  src, client: a.cfg.HTTPClient, poll: a.cfg.PollEvery}, nil
}

// Describe: the clone is ready in about SnapshotTypicalReady (it scales
// with the data) and lives at most TTL. SnapshotProvider makes it a full
// copy.
func (a *AlloyDBRestoreAPI) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindFullCopy, TypicalReady: SnapshotTypicalReady,
		MaxTTL: a.cfg.TTL}, nil
}

// TargetScope is the receipt scope of this provider's clones: clusters
// are regional, so the reaper looks in the same project and region.
func (a *AlloyDBRestoreAPI) TargetScope() string {
	return "gcp/" + a.cfg.Project + "/" + a.cfg.Location
}

// checkAlloyDBName refuses a name that is not a pg_sage clone name
// (sage-<purpose>-<8 hex>) or that a cluster id or label cannot hold.
func checkAlloyDBName(name, what string) error {
	if !auroraOwnedName.MatchString(name) || !alloyDBID.MatchString(name) {
		return fmt.Errorf("%w: %q is not a pg_sage clone name (sage-<purpose>-<8 hex>, "+
			"lowercase, at most 63 characters); %s", ErrInvalidAlloyDBConfig, name, what)
	}
	return nil
}

// FindByName is the cluster named name labelled with that name. found is
// false only when AlloyDB describes none. A cluster of that name not
// labelled for this receipt is an error, never "not found": the restore
// labels the cluster as it creates it, so pg_sage cannot prove it is its
// own and must not delete it, but closing the receipt as gone would lose
// track of a billable copy of production. The reaper leaves the receipt
// unresolved and reports why.
func (a *AlloyDBRestoreAPI) FindByName(ctx context.Context, name string) (string, bool,
	error) {
	api, err := a.client(ctx)
	if err != nil {
		return "", false, err
	}
	if err := checkAlloyDBName(name, "not looked up"); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, alloyDBCallTimeout)
	defer cancel()
	c, err := api.GetCluster(ctx, name)
	if errors.Is(err, ErrAlloyDBNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("alloydb_restore: find cluster %s: %w", name, err)
	}
	if label := c.Labels[AlloyDBReceiptLabel]; label != name {
		return "", false, fmt.Errorf("%w: alloydb_restore: cluster %s exists but its label "+
			"%s is %q, not its own name, so pg_sage cannot prove it is its own and leaves "+
			"it and its receipt; delete it by hand if it is a clone (gcloud alloydb "+
			"clusters delete %s --region %s --force)", ErrCloneLeftBehind, name,
			AlloyDBReceiptLabel, label, name, a.cfg.Location)
	}
	return name, true, nil
}

// Destroy deletes a cluster this provider made, with its instances. The
// cluster is read first: one not named sage-<purpose>-<8 hex> or not
// labelled for this receipt is refused and nothing is deleted. One gone
// or already deleting counts as deleted.
func (a *AlloyDBRestoreAPI) Destroy(ctx context.Context, id string) error {
	api, err := a.client(ctx)
	if err != nil {
		return err
	}
	if err := checkAlloyDBName(id, "nothing deleted"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, alloyDBCallTimeout)
	defer cancel()
	return api.deleteOwned(ctx, id)
}

var (
	_ SnapshotAPI = (*AlloyDBRestoreAPI)(nil)
	_ Describer   = (*AlloyDBRestoreAPI)(nil)
	_ Finder      = (*AlloyDBRestoreAPI)(nil)
)

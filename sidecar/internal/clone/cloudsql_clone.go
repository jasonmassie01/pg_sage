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

// cloudsql_clone (lab-05e): Cloud SQL has no copy-on-write clone, so a
// clone is instances.clone of the monitored instance to a new instance in
// the same project: a full copy at the full instance price, made only with
// clone.allow_full_copy. CloudSQLCloneAPI is the SnapshotAPI of it;
// SnapshotProvider wraps it as a Provider.

// Cloud SQL clone errors. Each is distinguishable with errors.Is.
var (
	// ErrInvalidCloudSQLConfig: a project, instance, deadline, TTL, clone
	// name or DSN outside the accepted shape, or an instance pg_sage did
	// not make.
	ErrInvalidCloudSQLConfig = errors.New("clone: invalid cloudsql_clone configuration")
	// ErrNotCloudSQLInstance: the configured instance is not a Cloud SQL for
	// PostgreSQL primary that the monitored DSN reaches by its IP.
	ErrNotCloudSQLInstance = errors.New("clone: the monitored database is not the " +
		"configured Cloud SQL for PostgreSQL instance")
	// ErrCloudSQLNotFound: Cloud SQL describes no such instance.
	ErrCloudSQLNotFound = errors.New("clone: cloud sql instance not found")
	// ErrCloudSQLAlreadyExists: Cloud SQL already has an instance of that
	// name (instance names are unique in a project).
	ErrCloudSQLAlreadyExists = errors.New("clone: cloud sql instance already exists")
	// ErrCloudSQLPermission: Google refused the call (401 or 403); the error
	// names the IAM permission the call needs.
	ErrCloudSQLPermission = errors.New("clone: cloud sql permission denied")
)

// Labels on a clone: the receipt's resource name, so the reaper finds a
// leaked one, and its expiry in Unix seconds (label values cannot hold an
// RFC 3339 time). Cloud SQL has no server-side expiry.
const (
	CloudSQLReceiptLabel = "pg-sage-receipt"
	CloudSQLExpiresLabel = "pg-sage-expires-at"
	// CloudSQLAPIBase is the Cloud SQL Admin API.
	CloudSQLAPIBase = "https://sqladmin.googleapis.com"
	// CloudSQLMaxTTL bounds a clone's life: a leaked one holds production
	// data for at most a day.
	CloudSQLMaxTTL = 24 * time.Hour
	// cloudSQLMaxName is the longest clone name: the receipt label's value
	// is at most 63 characters (tighter than Cloud SQL's 98 for
	// project:instance, since a project id is at most 30).
	cloudSQLMaxName     = 63
	defaultCloudSQLPoll = 15 * time.Second
	// cloudSQLCallTimeout bounds one lookup or destroy.
	cloudSQLCallTimeout = 5 * time.Minute
)

var (
	cloudSQLProjectID    = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	cloudSQLInstanceName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,95}[a-z0-9])?$`)
)

// CloudSQLTokenSource supplies Google access tokens (cloudtel.GCPToken).
type CloudSQLTokenSource interface {
	Token(ctx context.Context) (string, error)
}

// CloudSQLProduction is the monitored (production) database a clone is
// taken from: its DSN names one of the source instance's IPs, and the
// clone's DSN reuses it (same users, copied data).
type CloudSQLProduction struct{ DSN string }

// CloudSQLCloneConfig configures a CloudSQLCloneAPI.
type CloudSQLCloneConfig struct {
	// Project and Instance name the monitored Cloud SQL instance; clones
	// are made in the same project.
	Project, Instance string
	// Deadline bounds one clone, from the request until it is labelled
	// (clone.restore_deadline_minutes). One not ready by then is left to
	// the reaper (an uncertain create).
	Deadline time.Duration
	// TTL is the longest a clone may live: its expiry label and MaxTTL.
	// It must exceed Deadline.
	TTL time.Duration
	// Token is the Google credential chain; nil, an error or an empty
	// token makes the provider unavailable.
	Token func(context.Context) (CloudSQLTokenSource, error)
	// Production reads production's DSN at each clone; nil makes every
	// clone unavailable (an adapter that only destroys, as the reaper's).
	Production func(context.Context) (CloudSQLProduction, error)
	BaseURL    string        // "": CloudSQLAPIBase
	HTTPClient *http.Client  // nil: the egress floor's client
	PollEvery  time.Duration // operation poll period; 0: 15 seconds
	Now        func() time.Time
}

// CloudSQLCloneAPI clones the monitored Cloud SQL for PostgreSQL instance
// to a new instance labelled with its receipt name.
type CloudSQLCloneAPI struct{ cfg CloudSQLCloneConfig }

// NewCloudSQLCloneAPI validates cfg; it contacts nothing.
func NewCloudSQLCloneAPI(cfg CloudSQLCloneConfig) (*CloudSQLCloneAPI, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = CloudSQLAPIBase
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = egress.HTTPClient(0)
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultCloudSQLPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CloudSQLCloneAPI{cfg: cfg}, nil
}

func (cfg CloudSQLCloneConfig) validate() error {
	if !cloudSQLProjectID.MatchString(cfg.Project) {
		return fmt.Errorf("%w: project %q must be a Google Cloud project id such as "+
			"acme-prod-1234", ErrInvalidCloudSQLConfig, cfg.Project)
	}
	if !cloudSQLInstanceName.MatchString(cfg.Instance) ||
		auroraOwnedName.MatchString(cfg.Instance) {
		return fmt.Errorf("%w: instance %q must be a Cloud SQL instance name (lowercase "+
			"letters, digits and hyphens) that is not a pg_sage clone",
			ErrInvalidCloudSQLConfig, cfg.Instance)
	}
	if cfg.Deadline < RDSRestoreMinDeadline || cfg.Deadline > RDSRestoreMaxDeadline {
		return fmt.Errorf("%w: restore deadline %s must be within %s-%s",
			ErrInvalidCloudSQLConfig, cfg.Deadline, RDSRestoreMinDeadline, RDSRestoreMaxDeadline)
	}
	if cfg.TTL <= cfg.Deadline || cfg.TTL > CloudSQLMaxTTL {
		return fmt.Errorf("%w: clone TTL %s must exceed the restore deadline %s and be at "+
			"most %s", ErrInvalidCloudSQLConfig, cfg.TTL, cfg.Deadline, CloudSQLMaxTTL)
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%w: API base URL must be an http(s) URL",
				ErrInvalidCloudSQLConfig)
		}
	}
	return nil
}

// NewCloudSQLCloneProvider is the cloudsql_clone Provider: SnapshotProvider
// over a CloudSQLCloneAPI of cfg.
func NewCloudSQLCloneProvider(cfg CloudSQLCloneConfig) (*SnapshotProvider, error) {
	api, err := NewCloudSQLCloneAPI(cfg)
	if err != nil {
		return nil, err
	}
	return NewSnapshotProvider(api, api.cfg.Now)
}

// client is the Admin API with a token already fetched, or
// ErrCloneUnavailable without Google credentials: no clone can be made or
// deleted, so rehearsals are recommend-only.
func (a *CloudSQLCloneAPI) client(ctx context.Context) (*cloudSQLHTTP, error) {
	src, err := gcpToken(ctx, a.cfg.Token, CloudSQLCloneAdapter)
	if err != nil {
		return nil, err
	}
	return &cloudSQLHTTP{base: a.cfg.BaseURL, project: a.cfg.Project, token: src,
		client: a.cfg.HTTPClient, poll: a.cfg.PollEvery}, nil
}

// Describe: the clone is ready in about SnapshotTypicalReady (it scales
// with the disk) and lives at most TTL. SnapshotProvider makes it a full
// copy.
func (a *CloudSQLCloneAPI) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindFullCopy, TypicalReady: SnapshotTypicalReady,
		MaxTTL: a.cfg.TTL}, nil
}

// TargetScope is the receipt scope of this provider's clones.
func (a *CloudSQLCloneAPI) TargetScope() string { return "gcp/" + a.cfg.Project }

// checkCloneName refuses a name that is not a pg_sage clone name
// (sage-<purpose>-<8 hex>) or that its receipt label cannot hold.
func checkCloneName(name, what string) error {
	if !auroraOwnedName.MatchString(name) || len(name) > cloudSQLMaxName {
		return fmt.Errorf("%w: %q is not a pg_sage clone name (sage-<purpose>-<8 hex>, "+
			"lowercase, at most %d characters); %s", ErrInvalidCloudSQLConfig, name,
			cloudSQLMaxName, what)
	}
	return nil
}

// FindByName is the clone named name: one labelled with that name, or one
// not labelled at all (labelling never finished; its name is the receipt's
// random one). found is false when Cloud SQL describes none, or one
// labelled for another receipt.
func (a *CloudSQLCloneAPI) FindByName(ctx context.Context, name string) (string, bool,
	error) {
	api, err := a.client(ctx)
	if err != nil {
		return "", false, err
	}
	if err := checkCloneName(name, "not looked up"); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, cloudSQLCallTimeout)
	defer cancel()
	in, err := api.GetInstance(ctx, name)
	if errors.Is(err, ErrCloudSQLNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cloudsql_clone: find instance %s: %w", name, err)
	}
	if label, ok := in.Settings.UserLabels[CloudSQLReceiptLabel]; ok && label != name {
		return "", false, nil
	}
	return in.Name, true, nil
}

// Destroy deletes a clone this provider made. The instance is read first:
// one not named sage-<purpose>-<8 hex>, labelled for another receipt, or
// protected from deletion after pg_sage labelled it is refused and nothing
// is deleted. One gone or already deleting counts as deleted.
func (a *CloudSQLCloneAPI) Destroy(ctx context.Context, id string) error {
	api, err := a.client(ctx)
	if err != nil {
		return err
	}
	if err := checkCloneName(id, "nothing deleted"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cloudSQLCallTimeout)
	defer cancel()
	return api.deleteOwned(ctx, id)
}

var (
	_ SnapshotAPI = (*CloudSQLCloneAPI)(nil)
	_ Describer   = (*CloudSQLCloneAPI)(nil)
	_ Finder      = (*CloudSQLCloneAPI)(nil)
)

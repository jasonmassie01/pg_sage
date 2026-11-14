package clone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

// LakebaseAdapter is the clone.provider value and receipt adapter of
// Lakebase branch clones (market-03).
const LakebaseAdapter = "lakebase_branch"

// Lakebase branch clone errors. Each is distinguishable with errors.Is.
var (
	// ErrCloneUnavailable: the provider cannot create a clone now (no
	// Databricks auth, no production identity); rehearsal is recommend-only.
	ErrCloneUnavailable = errors.New("clone: provider unavailable")
	// ErrCloneIsProduction: the provider returned production's branch or
	// endpoint; nothing runs on it.
	ErrCloneIsProduction = errors.New("clone: provider returned the production branch " +
		"or endpoint")
	// ErrInvalidLakebaseConfig: a project, branch or clone id outside the
	// accepted shape or the configured project.
	ErrInvalidLakebaseConfig = errors.New("clone: invalid lakebase_branch configuration")
	// ErrLakebaseNotFound: the branch or endpoint no longer exists.
	ErrLakebaseNotFound = errors.New("clone: lakebase resource not found")
)

const (
	maxLakebaseTTL      = 24 * time.Hour
	defaultLakebaseWait = 10 * time.Minute
	lakebasePort        = "5432"
	lakebaseDatabase    = "databricks_postgres"
)

var lakebaseIDPattern = regexp.MustCompile(`^` + lakebaseid.Pattern + `$`)

// LakebaseBranchAPI is the slice of the Lakebase control plane the
// provider uses. Create calls block until the resource is ready or ctx
// ends; a delete of a resource already gone wraps ErrLakebaseNotFound.
type LakebaseBranchAPI interface {
	CreateBranch(context.Context, LakebaseBranchRequest) (LakebaseBranch, error)
	CreateEndpoint(context.Context, LakebaseEndpointRequest) (LakebaseEndpoint, error)
	DatabaseCredential(ctx context.Context, endpoint string) (string, error)
	DeleteEndpoint(ctx context.Context, name string) error
	DeleteBranch(ctx context.Context, name string) error
}

// LakebaseBranchRequest asks for branch Parent/branches/BranchID from
// SourceBranch, deleted by the server at ExpireTime.
type LakebaseBranchRequest struct {
	Parent, BranchID, SourceBranch string
	ExpireTime                     time.Time
}

// LakebaseBranch is a created branch; SourceTime is the point of the
// source it copies (zero when the server does not say).
type LakebaseBranch struct {
	Name, UID  string
	SourceTime time.Time
}

// LakebaseEndpointRequest asks for a read-write endpoint on Parent.
type LakebaseEndpointRequest struct {
	Parent, EndpointID string
	MaxCU              float64
}

// LakebaseEndpoint is a created endpoint and its direct host.
type LakebaseEndpoint struct{ Name, UID, Host string }

// LakebaseProduction is the monitored (production) endpoint a branch is
// taken from: its branch, the branch uid, the direct host, and the role
// and database a clone DSN reuses.
type LakebaseProduction struct {
	BranchName, BranchUID, Host, User, Database string
}

// LakebaseConfig configures a LakebaseBranchProvider.
type LakebaseConfig struct {
	Project string
	// SourceBranch is a branch id or projects/<Project>/branches/<id>;
	// empty means production's branch.
	SourceBranch  string
	TTL           time.Duration // server-side expiry of every branch
	EndpointMaxCU float64
	WaitTimeout   time.Duration // bound of each create wait; 0: 10 minutes
	// API returns the control plane; its error makes Create unavailable.
	API func() (LakebaseBranchAPI, error)
	// Production reads production's identity at each Create; nil makes
	// Create unavailable (an adapter that only destroys, as the reaper's).
	Production func(context.Context) (LakebaseProduction, error)
	Now        func() time.Time
}

// LakebaseBranchProvider clones by creating an instant Lakebase branch of
// production with its own read-write endpoint (market-03). Every branch
// has a server-side expiry, so a leak outlives no crash by more than TTL.
type LakebaseBranchProvider struct {
	cfg    LakebaseConfig
	source string // full source branch name; "" = production's
	owned  *regexp.Regexp
}

// NewLakebaseBranchProvider validates cfg; it contacts nothing.
func NewLakebaseBranchProvider(cfg LakebaseConfig) (*LakebaseBranchProvider, error) {
	if !lakebaseIDPattern.MatchString(cfg.Project) {
		return nil, fmt.Errorf("%w: project %q must be 1-63 lowercase letters, digits or "+
			"hyphens", ErrInvalidLakebaseConfig, cfg.Project)
	}
	if cfg.TTL <= 0 || cfg.TTL > maxLakebaseTTL {
		return nil, fmt.Errorf("%w: branch TTL %s must be within 1m-24h",
			ErrInvalidLakebaseConfig, cfg.TTL)
	}
	if !(cfg.EndpointMaxCU > 0) {
		return nil, fmt.Errorf("%w: endpoint max CU must be positive",
			ErrInvalidLakebaseConfig)
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("%w: control plane is required", ErrInvalidLakebaseConfig)
	}
	source, err := LakebaseSourceBranch(cfg.Project, cfg.SourceBranch)
	if err != nil {
		return nil, err
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = defaultLakebaseWait
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	owned := regexp.MustCompile(`^projects/` + regexp.QuoteMeta(cfg.Project) +
		`/branches/` + lakebaseid.RehearsalBranchPrefix + `[0-9a-f]{8}$`)
	return &LakebaseBranchProvider{cfg: cfg, source: source, owned: owned}, nil
}

// LakebaseSourceBranch is the full name of source (a branch id or
// projects/<project>/branches/<id>) in project; "" stays "" (production's
// branch). A branch of another project, or a malformed id, is refused.
func LakebaseSourceBranch(project, source string) (string, error) {
	if source == "" {
		return "", nil
	}
	id := source
	if strings.HasPrefix(source, "projects/") {
		prefix := "projects/" + project + "/branches/"
		if !strings.HasPrefix(source, prefix) {
			return "", fmt.Errorf("%w: source branch %q is not in project %q",
				ErrInvalidLakebaseConfig, source, project)
		}
		id = strings.TrimPrefix(source, prefix)
	}
	if !lakebaseIDPattern.MatchString(id) {
		return "", fmt.Errorf("%w: source branch %q must be a branch id (1-63 lowercase "+
			"letters, digits or hyphens) or projects/%s/branches/<id>",
			ErrInvalidLakebaseConfig, source, project)
	}
	return "projects/" + project + "/branches/" + id, nil
}

// SnapshotAge is zero: a branch copies its source at the moment it is
// created.
func (p *LakebaseBranchProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}

// TargetScope is the receipt scope of this provider's clones.
func (p *LakebaseBranchProvider) TargetScope() string { return "projects/" + p.cfg.Project }

var _ Provider = (*LakebaseBranchProvider)(nil)

// newLakebaseBranchID is "sage-rh-" and 8 random hex digits.
func newLakebaseBranchID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate rehearsal branch id: %w", err)
	}
	return lakebaseid.RehearsalBranchPrefix + hex.EncodeToString(b), nil
}

// lakebaseDSN connects to host as production's role with the minted
// token, verifying the server certificate and host name.
func lakebaseDSN(prod LakebaseProduction, host, token string) string {
	database := prod.Database
	if database == "" {
		database = lakebaseDatabase
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(prod.User, token),
		Host: host + ":" + lakebasePort, Path: "/" + database,
		RawQuery: "sslmode=verify-full"}
	return u.String()
}

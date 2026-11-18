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

// NeonAdapter is the clone.provider value and receipt adapter of Neon
// branch clones (lab-05b).
const NeonAdapter = "neon_branch"

// Neon branch clone errors. Each is distinguishable with errors.Is.
var (
	// ErrInvalidNeonConfig: a project, parent branch, clone name or branch
	// id outside the accepted shape or the configured project.
	ErrInvalidNeonConfig = errors.New("clone: invalid neon_branch configuration")
	// ErrNeonNotFound: Neon answered 404 (the branch is gone).
	ErrNeonNotFound = errors.New("clone: neon resource not found")
)

// Neon bounds and defaults.
const (
	// NeonMaxTTL bounds a branch's server-side expiry: a leaked branch
	// holds production data for at most a day.
	NeonMaxTTL = 24 * time.Hour
	// NeonMinCU and NeonMaxCU bound the clone endpoint's autoscaling
	// maximum; the minimum is always NeonMinCU.
	NeonMinCU = 0.25
	NeonMaxCU = 8.0
	// NeonAPIBase is the Neon API v2.
	NeonAPIBase      = "https://console.neon.tech/api/v2"
	neonTypicalReady = 30 * time.Second
	defaultNeonWait  = 5 * time.Minute
	defaultNeonPoll  = 2 * time.Second
)

var (
	neonProjectID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	neonParent    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
	neonBranchID  = regexp.MustCompile(`^br-[a-z0-9]+(-[a-z0-9]+)*$`)
	// neonOwnedName is the only branch name pg_sage gives (ResourceName)
	// and the only one it ever deletes.
	neonOwnedName = regexp.MustCompile(`^sage-[a-z]+(-[a-z]+)*-[0-9a-f]{8}$`)
)

// NeonProduction is the monitored (production) database a branch is taken
// from: its endpoint host, and the role and database a clone DSN reuses.
type NeonProduction struct{ Host, User, Database string }

// NeonConfig configures a NeonBranchProvider.
type NeonConfig struct {
	ProjectID string
	// ParentBranch is a branch id (br-...) or name in the project; empty
	// means the branch of the monitored endpoint.
	ParentBranch  string
	APIKey        string // empty: the provider is unavailable
	EndpointMaxCU float64
	TTL           time.Duration // server-side expiry of every branch
	BaseURL       string        // "": NeonAPIBase
	HTTPClient    *http.Client  // nil: the egress floor's client
	WaitTimeout   time.Duration // bound of one create or delete; 0: 5 minutes
	PollEvery     time.Duration // operation poll period; 0: 2 seconds
	// Production reads production's identity at each Create; nil makes
	// Create unavailable (an adapter that only destroys, as the reaper's).
	Production func(context.Context) (NeonProduction, error)
	Now        func() time.Time
}

// NeonBranchProvider clones by creating a copy-on-write Neon branch of
// production with its own read-write endpoint (lab-05b). Every branch
// asks Neon for an expiry, and the reaper finds a leaked one by name.
type NeonBranchProvider struct {
	cfg NeonConfig
	api neonAPI
}

// NewNeonBranchProvider validates cfg; it contacts nothing.
func NewNeonBranchProvider(cfg NeonConfig) (*NeonBranchProvider, error) {
	if err := validateNeonConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = NeonAPIBase
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = egress.HTTPClient(0)
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = defaultNeonWait
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultNeonPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	api := &neonHTTP{base: strings.TrimRight(cfg.BaseURL, "/"),
		key: strings.TrimSpace(cfg.APIKey), project: cfg.ProjectID, client: cfg.HTTPClient}
	return &NeonBranchProvider{cfg: cfg, api: api}, nil
}

func validateNeonConfig(cfg NeonConfig) error {
	if len(cfg.ProjectID) > 63 || !neonProjectID.MatchString(cfg.ProjectID) {
		return fmt.Errorf("%w: project id %q must be 1-63 lowercase letters, digits or "+
			"hyphens", ErrInvalidNeonConfig, cfg.ProjectID)
	}
	if cfg.ParentBranch != "" && !neonParent.MatchString(cfg.ParentBranch) {
		return fmt.Errorf("%w: parent branch %q must be a branch id (br-...) or a name of "+
			"letters, digits and . _ / - (at most 256)", ErrInvalidNeonConfig,
			cfg.ParentBranch)
	}
	if cfg.TTL < time.Minute || cfg.TTL > NeonMaxTTL {
		return fmt.Errorf("%w: branch TTL %s must be within 1m-24h", ErrInvalidNeonConfig,
			cfg.TTL)
	}
	if !(cfg.EndpointMaxCU >= NeonMinCU && cfg.EndpointMaxCU <= NeonMaxCU) {
		return fmt.Errorf("%w: endpoint max CU %v must be within %v-%v",
			ErrInvalidNeonConfig, cfg.EndpointMaxCU, NeonMinCU, NeonMaxCU)
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%w: API base URL must be an http(s) URL",
				ErrInvalidNeonConfig)
		}
	}
	return nil
}

// available is ErrCloneUnavailable without an API key: no branch can be
// made or deleted, so rehearsals are recommend-only.
func (p *NeonBranchProvider) available() error {
	if strings.TrimSpace(p.cfg.APIKey) == "" {
		return fmt.Errorf("%w: neon_branch has no API key: set clone.neon.api_key "+
			"(or SAGE_CLONE_NEON_API_KEY); rehearsals are recommend-only until then",
			ErrCloneUnavailable)
	}
	return nil
}

// Describe: a Neon rehearsal clone is a branch that expires server side
// after its TTL.
func (p *NeonBranchProvider) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindBranch, TypicalReady: neonTypicalReady, MaxTTL: p.cfg.TTL},
		nil
}

// SnapshotAge is zero: a branch copies its parent at the moment it is
// created.
func (p *NeonBranchProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}

// TargetScope is the receipt scope of this provider's clones.
func (p *NeonBranchProvider) TargetScope() string { return "projects/" + p.cfg.ProjectID }

// FindByName is the id of the branch named name, one pg_sage gives
// (sage-<purpose>-<8 hex>); found is false when no branch has it.
func (p *NeonBranchProvider) FindByName(ctx context.Context, name string) (string, bool,
	error) {
	if err := p.available(); err != nil {
		return "", false, err
	}
	if !neonOwnedName.MatchString(name) {
		return "", false, fmt.Errorf("%w: %q is not a pg_sage branch name "+
			"(sage-<purpose>-<8 hex>)", ErrInvalidNeonConfig, name)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	branches, err := p.api.ListBranches(ctx, name)
	if err != nil {
		return "", false, fmt.Errorf("neon_branch: find branch %s: %w", name, err)
	}
	var ids []string
	for _, b := range branches {
		if b.Name == name {
			ids = append(ids, b.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", false, nil
	case 1:
		return ids[0], true, nil
	default:
		return "", false, fmt.Errorf("neon_branch: %d branches are named %s (%s); delete "+
			"the extras by hand", len(ids), name, strings.Join(ids, ", "))
	}
}

// Destroy deletes a branch this provider made (its endpoint goes with
// it). The branch is read first: one not named sage-<purpose>-<8 hex>,
// the default branch or a protected one is refused and nothing is
// deleted. A branch already gone (its expiry) counts as deleted.
func (p *NeonBranchProvider) Destroy(ctx context.Context, target Clone) error {
	if err := p.available(); err != nil {
		return err
	}
	if !neonBranchID.MatchString(target.ID) || len(target.ID) > 64 {
		return fmt.Errorf("%w: %q is not a Neon branch id (br-...); nothing deleted",
			ErrInvalidNeonConfig, target.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	return p.deleteOwned(ctx, target.ID)
}

func (p *NeonBranchProvider) deleteOwned(ctx context.Context, id string) error {
	b, err := p.api.GetBranch(ctx, id)
	if errors.Is(err, ErrNeonNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("neon_branch: read branch %s before deleting it: %w", id, err)
	}
	if !neonOwnedName.MatchString(b.Name) || b.Default || b.Protected {
		return fmt.Errorf("%w: branch %s (%q, default %v, protected %v) is not a pg_sage "+
			"clone branch; nothing deleted", ErrInvalidNeonConfig, id, b.Name, b.Default,
			b.Protected)
	}
	if err := p.api.DeleteBranch(ctx, id); err != nil && !errors.Is(err, ErrNeonNotFound) {
		return fmt.Errorf("neon_branch: delete branch %s (%s): %w", id, b.Name, err)
	}
	return nil
}

var (
	_ Provider  = (*NeonBranchProvider)(nil)
	_ Describer = (*NeonBranchProvider)(nil)
	_ Finder    = (*NeonBranchProvider)(nil)
)

package clone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// NeonLister is the read-only slice of the Neon API v2 fleet discovery
// lists (fleetpol-01b): projects, their branches and their endpoints. It
// only sends GET requests, with the key only in the Authorization header;
// errors are the provider's (ProviderStatusError, so 429 and 5xx are
// Throttled) and never quote the key.
type NeonLister struct {
	base, key string
	client    *http.Client
}

// NeonListerConfig configures a NeonLister. BaseURL and HTTPClient default
// to NeonAPIBase and the egress floor's client.
type NeonListerConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

// NeonProject is a project the key can see.
type NeonProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NeonListedBranch is a branch with what Neon reports of its creation:
// created_at, creation_source ("console", "api", "neonctl", ...) and the
// created_by user's name when a person made it.
type NeonListedBranch struct {
	ID             string
	Name           string
	ParentID       string
	ProjectID      string
	Default        bool
	CreatedAt      time.Time
	CreationSource string
	CreatedBy      string
}

// NeonListedEndpoint is a compute endpoint of a branch.
type NeonListedEndpoint struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	BranchID string `json:"branch_id"`
	Type     string `json:"type"`
}

// neonProjectsLimit bounds the one projects page discovery reads.
const neonProjectsLimit = 100

// NewNeonLister refuses an empty key: it names the setting to fill.
func NewNeonLister(cfg NeonListerConfig) (*NeonLister, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("%w: neon discovery needs clone.neon.api_key "+
			"(SAGE_CLONE_NEON_API_KEY)", ErrCloneUnavailable)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = NeonAPIBase
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = egress.HTTPClient(0)
	}
	return &NeonLister{base: strings.TrimRight(cfg.BaseURL, "/"), key: key,
		client: cfg.HTTPClient}, nil
}

// IsNeonRehearsalBranch reports whether name is a branch pg_sage made
// for a rehearsal (sage-<purpose>-<8 hex>); discovery skips them.
func IsNeonRehearsalBranch(name string) bool { return neonOwnedName.MatchString(name) }

// ListProjects is the first page of the key's projects.
func (l *NeonLister) ListProjects(ctx context.Context) ([]NeonProject, error) {
	var out struct {
		Projects []NeonProject `json:"projects"`
	}
	q := url.Values{"limit": {fmt.Sprint(neonProjectsLimit)}}
	err := l.get(ctx, "list projects", "", "projects", q, &out)
	return out.Projects, err
}

// ListBranches is project's branches.
func (l *NeonLister) ListBranches(ctx context.Context, project string) (
	[]NeonListedBranch, error) {
	var out struct {
		Branches []struct {
			ID             string    `json:"id"`
			Name           string    `json:"name"`
			ParentID       string    `json:"parent_id"`
			ProjectID      string    `json:"project_id"`
			Default        bool      `json:"default"`
			CreatedAt      time.Time `json:"created_at"`
			CreationSource string    `json:"creation_source"`
			CreatedBy      struct {
				Name string `json:"name"`
			} `json:"created_by"`
		} `json:"branches"`
	}
	if err := l.get(ctx, "list branches", project, "branches", nil, &out); err != nil {
		return nil, err
	}
	branches := make([]NeonListedBranch, 0, len(out.Branches))
	for _, b := range out.Branches {
		branches = append(branches, NeonListedBranch{ID: b.ID, Name: b.Name,
			ParentID: b.ParentID, ProjectID: b.ProjectID, Default: b.Default,
			CreatedAt: b.CreatedAt.UTC(), CreationSource: b.CreationSource,
			CreatedBy: b.CreatedBy.Name})
	}
	return branches, nil
}

// ListEndpoints is project's endpoints.
func (l *NeonLister) ListEndpoints(ctx context.Context, project string) (
	[]NeonListedEndpoint, error) {
	var out struct {
		Endpoints []NeonListedEndpoint `json:"endpoints"`
	}
	err := l.get(ctx, "list endpoints", project, "endpoints", nil, &out)
	return out.Endpoints, err
}

// get reads /projects (project "") or /projects/<project>/<path>.
func (l *NeonLister) get(ctx context.Context, op, project, path string, q url.Values,
	out any) error {
	target := l.base + "/" + path
	if path != "projects" {
		if len(project) > 63 || !neonProjectID.MatchString(project) {
			return fmt.Errorf("%w: neon %s: project id %q", ErrInvalidNeonConfig, op, project)
		}
		target = l.base + "/projects/" + url.PathEscape(project) + "/" + path
	}
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("neon %s: %w", op, errors.New("invalid API base URL"))
	}
	req.Header.Set("Authorization", "Bearer "+l.key)
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("neon %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, neonMaxBody))
	if err != nil {
		return fmt.Errorf("neon %s: read the reply: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		api := &neonHTTP{key: l.key, project: project}
		return api.statusError(op, resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("neon %s: decode the reply: %w", op, err)
	}
	return nil
}

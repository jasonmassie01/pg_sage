package discovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/clone"
)

// NeonAPI is the read-only Neon listing a NeonSource uses
// (*clone.NeonLister, lab-05b's Neon API client).
type NeonAPI interface {
	ListProjects(ctx context.Context) ([]clone.NeonProject, error)
	ListBranches(ctx context.Context, project string) ([]clone.NeonListedBranch, error)
	ListEndpoints(ctx context.Context, project string) ([]clone.NeonListedEndpoint, error)
}

// neonDefaultDatabase is the database a Neon project starts with.
const neonDefaultDatabase = "neondb"

// NeonSource lists the branches of one Neon project, or of every project
// the key can see (fleetpol-01b): one candidate per branch with a compute
// endpoint, with its creator and creation time as Neon reports them.
// pg_sage's own rehearsal branches are skipped (market-06b).
type NeonSource struct {
	name, project, database string
	api                     NeonAPI
	cap                     int
}

// NewNeonSource lists project ("" for every project) through api, at
// most cap candidates; database "" is neondb.
func NewNeonSource(name, project, database string, api NeonAPI, cap int) *NeonSource {
	if database == "" {
		database = neonDefaultDatabase
	}
	return &NeonSource{name: name, project: project, database: database, api: api, cap: cap}
}

// Name is the source name.
func (n *NeonSource) Name() string { return n.name }

// List walks the projects until the cap: two listings per project.
func (n *NeonSource) List(ctx context.Context) ([]Candidate, error) {
	projects := []clone.NeonProject{{ID: n.project}}
	if n.project == "" {
		var err error
		if projects, err = n.api.ListProjects(ctx); err != nil {
			return nil, fmt.Errorf("neon source %s: %w", n.name, err)
		}
	}
	var out []Candidate
	for i := 0; i < len(projects) && len(out) < n.cap; i++ {
		cands, err := n.listProject(ctx, projects[i].ID, n.cap-len(out))
		if err != nil {
			return nil, fmt.Errorf("neon source %s: project %s: %w", n.name,
				projects[i].ID, err)
		}
		out = append(out, cands...)
	}
	return out, nil
}

func (n *NeonSource) listProject(ctx context.Context, project string, room int) (
	[]Candidate, error) {
	branches, err := n.api.ListBranches(ctx, project)
	if err != nil {
		return nil, err
	}
	endpoints, err := n.api.ListEndpoints(ctx, project)
	if err != nil {
		return nil, err
	}
	hosts := branchHosts(endpoints)
	var out []Candidate
	for _, b := range branches {
		if len(out) >= room {
			break
		}
		host := hosts[b.ID]
		if host == "" || clone.IsNeonRehearsalBranch(b.Name) {
			continue
		}
		out = append(out, Candidate{ExternalID: b.ID, Host: host, Port: 5432,
			DBName: n.database, Creator: neonCreator(b), CreatedAt: b.CreatedAt,
			Tags: map[string]string{"neon_project": project, "neon_branch": b.Name}})
	}
	return out, nil
}

// branchHosts is each branch's endpoint host, its read-write endpoint's
// when it has one.
func branchHosts(endpoints []clone.NeonListedEndpoint) map[string]string {
	hosts := map[string]string{}
	for _, ep := range endpoints {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(ep.Host)), ".")
		if host == "" {
			continue
		}
		if _, seen := hosts[ep.BranchID]; !seen || ep.Type == "read_write" {
			hosts[ep.BranchID] = host
		}
	}
	return hosts
}

// neonCreator maps Neon's creation source: the console is a person (by
// name when Neon says who); any other source (the API, the CLI, an
// integration) is treated as an agent, so the stricter agent-created set
// applies when in doubt. No creation source is no creator.
func neonCreator(b clone.NeonListedBranch) string {
	source := strings.ToLower(strings.TrimSpace(b.CreationSource))
	switch source {
	case "":
		return ""
	case "console":
		who := strings.Join(strings.Fields(b.CreatedBy), "_")
		if who == "" {
			who = "console"
		}
		return "person:" + who
	}
	return "agent:" + strings.Join(strings.Fields(source), "_")
}

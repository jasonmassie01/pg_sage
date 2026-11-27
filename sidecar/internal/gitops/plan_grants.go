package gitops

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/auth"
)

// planGrants plans every grant entry and removes the config-owned grants
// no file names. A grant changes what a person may see or do, not what an
// agent or pg_sage may do, so every change applies with the file as actor.
// A grant must name a known user and a monitored database.
func (p *planner) planGrants() {
	monitored := map[string]bool{}
	for _, d := range p.opts.Databases {
		monitored[d] = true
	}
	named := map[string]bool{}
	for _, spec := range p.dir.Grants {
		u, ok := p.st.users[spec.User]
		switch {
		case !ok:
			p.problem(Problem{File: spec.Path, Line: spec.Line,
				Message: "user " + spec.User + " is not a pg_sage user"})
			continue
		case !monitored[spec.Database]:
			p.problem(Problem{File: spec.Path, Line: spec.Line, Message: "database " +
				spec.Database + " is not one pg_sage monitors"})
			continue
		}
		key := grantKey(u.ID, spec.Database)
		named[key] = true
		p.planGrant(spec, u, key)
	}
	for _, key := range sortedKeys(p.st.grants) {
		g := p.st.grants[key]
		if g.Source != auth.GrantSourceConfig || named[key] {
			continue
		}
		c := auth.GrantChange{UserID: g.UserID, Database: g.Database}
		actor := "config:" + p.dir.Root + "@" + p.dir.Hash
		p.add(Change{Kind: KindGrant, Name: p.st.userByID[g.UserID] + " on " + g.Database,
			Op: OpRetire, Detail: "no file names it"},
			func(ctx context.Context, tx pgx.Tx) error {
				return auth.RevokeConfigGrantTx(ctx, tx, c, actor)
			})
	}
}

func (p *planner) planGrant(spec GrantSpec, u auth.GrantUser, key string) {
	change := Change{Kind: KindGrant, Name: spec.User + " on " + spec.Database,
		File: spec.Path, Detail: "role " + spec.Role}
	cur, exists := p.st.grants[key]
	switch {
	case !exists:
		change.Op = OpCreate
	case cur.Source != auth.GrantSourceConfig && !spec.Adopt:
		p.problem(Problem{File: spec.Path, Line: spec.Line, Message: "the grant of " +
			spec.User + " on " + spec.Database + " is set by " + cur.Source +
			"; add adopt: true to this entry to manage it here"})
		return
	case cur.Source != auth.GrantSourceConfig:
		change.Op = OpAdopt
	case cur.Role != spec.Role:
		change.Op = OpUpdate
	default:
		return
	}
	c := auth.GrantChange{UserID: u.ID, Database: spec.Database, Role: spec.Role}
	actor := spec.Actor()
	p.add(change, func(ctx context.Context, tx pgx.Tx) error {
		return auth.SetConfigGrantTx(ctx, tx, c, actor)
	})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

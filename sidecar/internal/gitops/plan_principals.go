package gitops

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
)

// planPrincipals plans every principal file and retires the config-owned
// principals no file names. A file never mints a token. A patch that
// widens a principal (a profile that adds a class or the direct lane, or a
// higher env_ceiling) is filed as the principal's pending change for a
// person to confirm; one that does not applies at once.
func (p *planner) planPrincipals(ctx context.Context, tx pgx.Tx) error {
	named := map[string]bool{}
	for _, spec := range p.dir.Principals {
		named[spec.Name] = true
		if err := p.planPrincipal(ctx, tx, spec); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(p.st.principals) {
		cur := p.st.principals[name]
		if cur.ManagedBy != agentguard.ManagedByConfig || named[cur.Name] ||
			cur.Status == agentguard.StatusRetired {
			continue
		}
		id := cur.ID
		audit := agentguard.Audit{Reason: "no file under policy.directory names it (" +
			p.dir.Hash + ")"}
		p.add(Change{Kind: KindPrincipal, Name: cur.Name, Op: OpRetire,
			Detail: "no file names it; its tokens stop working"},
			func(ctx context.Context, tx pgx.Tx) error {
				return agentguard.RetireTx(ctx, tx, id, audit)
			})
	}
	return nil
}

func (p *planner) planPrincipal(ctx context.Context, tx pgx.Tx, spec PrincipalSpec) error {
	sponsor, ok := p.sponsorOf(spec)
	if !ok {
		return nil
	}
	cur, exists := p.st.principals[spec.Name]
	if !exists {
		p.createPrincipal(spec, sponsor)
		return nil
	}
	switch {
	case cur.Status == agentguard.StatusRetired:
		p.problem(spec.problem("name", "principal %s is retired and never changes again; "+
			"choose another name", spec.Name))
		return nil
	case cur.ManagedBy != agentguard.ManagedByConfig && !spec.Adopt:
		p.problem(spec.problem("name", "principal %s is owned by the API; add adopt: true "+
			"to this file to manage it here", spec.Name))
		return nil
	case cur.Tenant != spec.Tenant:
		p.problem(spec.problem("tenant", "the tenant of %s is %q and cannot change; retire "+
			"it and create another principal", spec.Name, cur.Tenant))
		return nil
	}
	change := Change{Kind: KindPrincipal, Name: spec.Name, File: spec.Path}
	if cur.ManagedBy != agentguard.ManagedByConfig {
		id, audit := cur.ID, fileAudit(spec, "adopted from")
		c := change
		c.Op = OpAdopt
		p.add(c, func(ctx context.Context, tx pgx.Tx) error {
			return agentguard.AdoptTx(ctx, tx, id, audit)
		})
	}
	return p.patchPrincipal(ctx, tx, spec, cur, sponsor, change)
}

func fileAudit(spec PrincipalSpec, verb string) agentguard.Audit {
	return agentguard.Audit{Reason: verb + " " + spec.Actor()}
}

// sponsorOf resolves the file's sponsor email to an enabled operator or
// admin (nil: the file leaves the sponsor alone).
func (p *planner) sponsorOf(spec PrincipalSpec) (*int, bool) {
	if spec.Sponsor == "" {
		return nil, true
	}
	u, ok := p.st.users[spec.Sponsor]
	if !ok || u.Disabled || (u.Role != auth.RoleOperator && u.Role != auth.RoleAdmin) {
		p.problem(spec.problem("sponsor", "sponsor %s must be an enabled operator or admin "+
			"of pg_sage", spec.Sponsor))
		return nil, false
	}
	id := u.ID
	return &id, true
}

func (p *planner) createPrincipal(spec PrincipalSpec, sponsor *int) {
	req := agentguard.CreateRequest{Name: spec.Name, SponsorUserID: sponsor,
		Tenant: spec.Tenant, Profile: spec.Profile, EnvCeiling: spec.EnvCeiling,
		CreatedBy: spec.Actor(), Audit: fileAudit(spec, "created from")}
	p.add(Change{Kind: KindPrincipal, Name: spec.Name, Op: OpCreate, File: spec.Path,
		Detail: fmt.Sprintf("profile %s, env_ceiling %s, no token (a person mints one)",
			spec.Profile, spec.EnvCeiling)},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := agentguard.CreateOwnedTx(ctx, tx, req, agentguard.ManagedByConfig)
			return err
		})
}

// patchPrincipal plans the fields the file changes.
func (p *planner) patchPrincipal(ctx context.Context, tx pgx.Tx, spec PrincipalSpec,
	cur agentguard.OwnedPrincipal, sponsor *int, change Change) error {
	var patch agentguard.Patch
	if spec.Profile != cur.Profile {
		profile := spec.Profile
		patch.Profile = &profile
	}
	if spec.EnvCeiling != cur.EnvCeiling {
		ceiling := spec.EnvCeiling
		patch.EnvCeiling = &ceiling
	}
	if sponsor != nil && (cur.SponsorID == nil || *cur.SponsorID != *sponsor) {
		patch.SponsorUserID = sponsor
	}
	if patch == (agentguard.Patch{}) {
		return nil
	}
	id := cur.ID
	if !patch.Widens(agentguard.Principal{Profile: cur.Profile, EnvCeiling: cur.EnvCeiling}) {
		change.Op, change.Detail = OpUpdate, "narrows or keeps what it may do"
		audit := fileAudit(spec, "changed by")
		p.add(change, func(ctx context.Context, tx pgx.Tx) error {
			return agentguard.UpdateNarrowingTx(ctx, tx, id, patch, audit)
		})
		return nil
	}
	pending, err := agentguard.PendingChangeTx(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("gitops: %w", err)
	}
	if pending != nil && agentguard.SamePatch(*pending, patch) {
		change.Op, change.Detail = OpWaiting, "the widening waits for a person's confirm"
		p.note(change)
		return nil
	}
	change.Op = OpPropose
	change.Detail = "widens it: filed as a pending change for an admin to confirm"
	actor, audit := spec.Actor(), fileAudit(spec, "requested by")
	p.add(change, func(ctx context.Context, tx pgx.Tx) error {
		_, err := agentguard.RequestConfigChangeTx(ctx, tx, id, patch, actor, audit)
		return err
	})
	return nil
}

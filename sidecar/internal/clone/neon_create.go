package clone

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Create makes branch spec.ResourceName (sage-<purpose>-<8 hex>, the
// receipt's) of the parent branch with a read-write endpoint and an
// expiry after TTL, waits for its operations, and returns a DSN for
// production's role with sslmode=require. A failure after the branch
// exists deletes it before the error returns; a wait cut short leaves it
// to the reaper, which finds it by name (the receipt is create_uncertain).
// The clone id is the branch id.
func (p *NeonBranchProvider) Create(ctx context.Context, spec CloneSpec) (Clone, error) {
	if err := p.available(); err != nil {
		return Clone{}, err
	}
	if !neonOwnedName.MatchString(spec.ResourceName) {
		return Clone{}, fmt.Errorf("%w: clone name %q is not sage-<purpose>-<8 hex> (the "+
			"receipt's resource name); nothing created", ErrInvalidNeonConfig,
			spec.ResourceName)
	}
	prod, err := p.production(ctx)
	if err != nil {
		return Clone{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	parent, prodBranch, err := p.resolveParent(ctx, prod)
	if err != nil {
		return Clone{}, err
	}
	run := &neonCreate{p: p, prod: prod, name: spec.ResourceName, prodBranch: prodBranch,
		expire: p.cfg.Now().Add(p.cfg.TTL).UTC()}
	return run.create(ctx, parent)
}

// production reads and checks production's identity. Without it no clone
// can be told apart from production, so Create is unavailable (fail
// closed).
func (p *NeonBranchProvider) production(ctx context.Context) (NeonProduction, error) {
	if p.cfg.Production == nil {
		return NeonProduction{}, fmt.Errorf("%w: neon_branch has no production identity "+
			"to tell a clone from production", ErrCloneUnavailable)
	}
	prod, err := p.cfg.Production(ctx)
	if err != nil {
		return NeonProduction{}, fmt.Errorf("%w: read the monitored Neon database's "+
			"identity: %w", ErrCloneUnavailable, err)
	}
	if blank(prod.Host) || blank(prod.User) || blank(prod.Database) {
		return NeonProduction{}, fmt.Errorf("%w: the monitored Neon database's identity "+
			"is incomplete (host, role and database are required)", ErrCloneUnavailable)
	}
	return prod, nil
}

// resolveParent is the parent branch id and production's branch id ("" if
// production's host is not an endpoint of the project and a parent is
// configured).
func (p *NeonBranchProvider) resolveParent(ctx context.Context,
	prod NeonProduction) (string, string, error) {
	endpoints, err := p.api.ListEndpoints(ctx)
	if err != nil {
		return "", "", fmt.Errorf("neon_branch: list the endpoints of project %s: %w",
			p.cfg.ProjectID, err)
	}
	var prodBranch string
	for _, e := range endpoints {
		if sameNeonHost(e.Host, prod.Host) {
			prodBranch = e.BranchID
		}
	}
	if p.cfg.ParentBranch == "" {
		if prodBranch == "" {
			return "", "", fmt.Errorf("%w: the monitored host %s is not an endpoint of Neon "+
				"project %s: set clone.neon.project_id to its project or name "+
				"clone.neon.parent_branch", ErrInvalidNeonConfig, prod.Host, p.cfg.ProjectID)
		}
		return prodBranch, prodBranch, nil
	}
	branches, err := p.api.ListBranches(ctx, "")
	if err != nil {
		return "", "", fmt.Errorf("neon_branch: list the branches of project %s: %w",
			p.cfg.ProjectID, err)
	}
	for _, b := range branches {
		if b.ID == p.cfg.ParentBranch || b.Name == p.cfg.ParentBranch {
			return b.ID, prodBranch, nil
		}
	}
	return "", "", fmt.Errorf("%w: parent branch %q is not in Neon project %q",
		ErrInvalidNeonConfig, p.cfg.ParentBranch, p.cfg.ProjectID)
}

// neonCreate is one Create: what it asked for, so cleanup deletes exactly
// that.
type neonCreate struct {
	p                *NeonBranchProvider
	prod             NeonProduction
	name, prodBranch string
	branchID, host   string
	endpointID       string
	expire           time.Time
}

func (r *neonCreate) create(ctx context.Context, parent string) (Clone, error) {
	req := neonCreateRequest{Endpoints: []neonEndpointRequest{{Type: "read_write",
		MinCU: NeonMinCU, MaxCU: r.p.cfg.EndpointMaxCU}}}
	req.Branch.Name, req.Branch.ParentID = r.name, parent
	req.Branch.ExpiresAt = r.expire.Format(time.RFC3339)
	created, err := r.p.api.CreateBranch(ctx, req)
	if err != nil {
		err = fmt.Errorf("neon_branch: create branch %s from %s: %w", r.name, parent, err)
		var status *ProviderStatusError
		if errors.As(err, &status) {
			return Clone{}, err // Neon answered: nothing was made
		}
		return Clone{}, r.cleanupByName(err)
	}
	if err := r.check(created); err != nil {
		return Clone{}, r.cleanup(err)
	}
	if err := r.wait(ctx, created.Operations); err != nil {
		if ctx.Err() != nil {
			return Clone{}, fmt.Errorf("neon_branch: branch %s (%s) not ready after %s; "+
				"left to the reaper, which finds it by name: %w (%w)", r.name, r.branchID,
				r.p.cfg.WaitTimeout, err, ctx.Err())
		}
		return Clone{}, r.cleanup(err)
	}
	dsn, err := r.dsn(ctx)
	if err != nil {
		return Clone{}, r.cleanup(err)
	}
	return Clone{DSN: dsn, ID: r.branchID, CreatedFrom: r.p.cfg.Now()}, nil
}

// check is the identity guard on the create reply: the branch asked for,
// not production's, with a read-write endpoint on a host that is not
// production's.
func (r *neonCreate) check(c neonCreated) error {
	if blank(c.Branch.ID) {
		return fmt.Errorf("neon_branch: the create reply for %s has no branch.id", r.name)
	}
	r.branchID = c.Branch.ID
	if c.Branch.Name != r.name {
		return fmt.Errorf("neon_branch: branch %s cannot be shown to be %s: the reply names "+
			"it %q", c.Branch.ID, r.name, c.Branch.Name)
	}
	if c.Branch.ID == r.prodBranch {
		return fmt.Errorf("%w: branch %s has production branch %s's id",
			ErrCloneIsProduction, r.name, r.prodBranch)
	}
	for _, e := range c.Endpoints {
		if e.Type == "read_write" && e.BranchID == c.Branch.ID && !blank(e.Host) {
			r.host, r.endpointID = strings.TrimSpace(e.Host), e.ID
		}
	}
	if r.host == "" {
		return fmt.Errorf("neon_branch: the create reply for %s has no read_write endpoint "+
			"with a host (endpoints[].host)", r.name)
	}
	if sameNeonHost(r.host, r.prod.Host) {
		return fmt.Errorf("%w: branch %s's endpoint answers on production's host %s",
			ErrCloneIsProduction, r.name, r.prod.Host)
	}
	return nil
}

// wait polls ops until every one finished. A failed operation is an
// error; a throttled poll is retried at the next tick.
func (r *neonCreate) wait(ctx context.Context, ops []neonOperation) error {
	pending := map[string]bool{}
	for _, op := range ops {
		pending[op.ID] = !neonOpDone(op.Status)
	}
	for {
		for _, op := range ops {
			if !pending[op.ID] {
				continue
			}
			got, err := r.p.api.Operation(ctx, op.ID)
			if err != nil && !Throttled(err) {
				return fmt.Errorf("neon_branch: poll operation %s (%s) of %s: %w", op.ID,
					op.Action, r.name, err)
			}
			if err := neonOpFailed(got, r.name); err != nil {
				return err
			}
			pending[op.ID] = err != nil || !neonOpDone(got.Status)
		}
		if !anyPending(pending) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.p.cfg.PollEvery):
		}
	}
}

func neonOpDone(status string) bool { return status == "finished" || status == "skipped" }

func neonOpFailed(op neonOperation, name string) error {
	switch op.Status {
	case "failed", "error", "cancelling", "cancelled":
		return fmt.Errorf("neon_branch: operation %s (%s) of %s %s: %s", op.ID, op.Action,
			name, op.Status, op.Error)
	}
	return nil
}

func anyPending(pending map[string]bool) bool {
	for _, p := range pending {
		if p {
			return true
		}
	}
	return false
}

// dsn is the connection URI Neon gives for production's role and database
// on the clone endpoint, checked to name that endpoint, with
// sslmode=require. Errors never quote the URI (it holds the password).
func (r *neonCreate) dsn(ctx context.Context) (string, error) {
	uri, err := r.p.api.ConnectionURI(ctx, neonURIQuery{BranchID: r.branchID,
		EndpointID: r.endpointID, Database: r.prod.Database, Role: r.prod.User})
	if err != nil {
		return "", fmt.Errorf("neon_branch: connection URI of %s: %w", r.name, err)
	}
	if blank(uri) {
		return "", fmt.Errorf(`neon_branch: the connection URI reply for %s has no "uri"`,
			r.name)
	}
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil {
		return "", fmt.Errorf(`neon_branch: the "uri" for %s is not a postgres URL with a `+
			"role", r.name)
	}
	if !sameNeonHost(u.Hostname(), r.host) || sameNeonHost(u.Hostname(), r.prod.Host) {
		return "", fmt.Errorf(`%w: the "uri" for %s names host %s, not the clone `+
			"endpoint %s", ErrCloneIsProduction, r.name, u.Hostname(), r.host)
	}
	q := u.Query()
	q.Set("sslmode", "require")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// cleanup deletes the branch this Create made, on a fresh bounded context
// (the caller's may have ended). A delete that fails leaves the branch to
// its expiry and the reaper, and says so.
func (r *neonCreate) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := r.p.deleteOwned(ctx, r.branchID); err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

// cleanupByName deletes the branch a create whose reply was lost may have
// made, found by its name.
func (r *neonCreate) cleanupByName(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	id, found, err := r.p.FindByName(ctx, r.name)
	if err == nil && found {
		r.branchID = id
		err = r.p.deleteOwned(ctx, id)
	}
	if err != nil {
		return errors.Join(cause, r.leftBehind(err))
	}
	return cause
}

func (r *neonCreate) leftBehind(err error) error {
	return fmt.Errorf("%w: neon_branch cleanup failed; branch %s expires at %s and the "+
		"reaper finds it by name: %w", ErrCloneLeftBehind, r.name,
		r.expire.Format(time.RFC3339), err)
}

// sameNeonHost compares endpoint hosts, a pooler host (ep-x-pooler.<r>)
// counting as its endpoint's (ep-x.<r>).
func sameNeonHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if label, rest, ok := strings.Cut(h, "."); ok {
			return strings.TrimSuffix(label, "-pooler") + "." + rest
		}
		return strings.TrimSuffix(h, "-pooler")
	}
	return norm(a) != "" && norm(a) == norm(b)
}

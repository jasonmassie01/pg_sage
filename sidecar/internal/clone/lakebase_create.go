package clone

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

var lakebaseBranchName = regexp.MustCompile(`^projects/(` + lakebaseid.Pattern +
	`)/branches/(` + lakebaseid.Pattern + `)$`)

// Create makes branch sage-rh-<8 hex> of the source branch (expiring
// after TTL on the server), a read-write endpoint on it, and a credential
// for production's role. Any failure after the branch may exist deletes
// what was made before the error returns. The clone id is the branch's
// resource name.
func (p *LakebaseBranchProvider) Create(ctx context.Context, _ CloneSpec) (Clone, error) {
	prod, source, err := p.production(ctx)
	if err != nil {
		return Clone{}, err
	}
	api, err := p.cfg.API()
	if err != nil {
		return Clone{}, fmt.Errorf("%w: lakebase_branch needs Databricks unified auth: %w",
			ErrCloneUnavailable, err)
	}
	id, err := newLakebaseBranchID()
	if err != nil {
		return Clone{}, err
	}
	branch := "projects/" + p.cfg.Project + "/branches/" + id
	run := &lakebaseCreate{p: p, api: api, prod: prod, id: id, branch: branch,
		endpoint: branch + "/endpoints/" + id, expire: p.cfg.Now().Add(p.cfg.TTL)}
	created, err := run.create(ctx, source)
	if err != nil {
		return Clone{}, run.cleanup(err)
	}
	return created, nil
}

// production reads and checks production's identity and resolves the
// source branch. Without a complete identity no clone can be told apart
// from production, so Create is unavailable (fail closed).
func (p *LakebaseBranchProvider) production(ctx context.Context) (LakebaseProduction,
	string, error) {
	if p.cfg.Production == nil {
		return LakebaseProduction{}, "", fmt.Errorf("%w: lakebase_branch has no production "+
			"identity to tell a clone from production", ErrCloneUnavailable)
	}
	prod, err := p.cfg.Production(ctx)
	if err != nil {
		return LakebaseProduction{}, "", fmt.Errorf("%w: read the production Lakebase "+
			"identity: %w", ErrCloneUnavailable, err)
	}
	if !lakebaseBranchName.MatchString(prod.BranchName) || blank(prod.BranchUID) ||
		blank(prod.Host) || blank(prod.User) {
		return LakebaseProduction{}, "", fmt.Errorf("%w: the production Lakebase identity "+
			"is incomplete (branch, branch uid, host and role are required)",
			ErrCloneUnavailable)
	}
	if p.source != "" {
		return prod, p.source, nil
	}
	if !strings.HasPrefix(prod.BranchName, "projects/"+p.cfg.Project+"/branches/") {
		return LakebaseProduction{}, "", fmt.Errorf("%w: the monitored endpoint's branch %s "+
			"is not in project %s; set clone.lakebase.project to its project or name "+
			"clone.lakebase.source_branch", ErrInvalidLakebaseConfig, prod.BranchName,
			p.cfg.Project)
	}
	return prod, prod.BranchName, nil
}

// lakebaseCreate is one Create: what it asked for, so cleanup deletes
// exactly that.
type lakebaseCreate struct {
	p                    *LakebaseBranchProvider
	api                  LakebaseBranchAPI
	prod                 LakebaseProduction
	id, branch, endpoint string
	expire               time.Time
	endpointAsked        bool
}

func (r *lakebaseCreate) create(ctx context.Context, source string) (Clone, error) {
	wait := r.p.cfg.WaitTimeout
	branch, err := waitStep(ctx, wait, "create branch "+r.branch+" from "+source,
		func(ctx context.Context) (LakebaseBranch, error) {
			return r.api.CreateBranch(ctx, LakebaseBranchRequest{
				Parent: "projects/" + r.p.cfg.Project, BranchID: r.id,
				SourceBranch: source, ExpireTime: r.expire})
		})
	if err != nil {
		return Clone{}, err
	}
	if err := r.checkBranch(branch); err != nil {
		return Clone{}, err
	}
	r.endpointAsked = true
	endpoint, err := waitStep(ctx, wait, "create endpoint "+r.endpoint,
		func(ctx context.Context) (LakebaseEndpoint, error) {
			return r.api.CreateEndpoint(ctx, LakebaseEndpointRequest{Parent: r.branch,
				EndpointID: r.id, MaxCU: r.p.cfg.EndpointMaxCU})
		})
	if err != nil {
		return Clone{}, err
	}
	if err := r.checkEndpoint(endpoint); err != nil {
		return Clone{}, err
	}
	token, err := waitStep(ctx, wait, "mint a database credential for "+r.endpoint,
		func(ctx context.Context) (string, error) {
			return r.api.DatabaseCredential(ctx, r.endpoint)
		})
	if err != nil {
		return Clone{}, err
	}
	if token == "" {
		return Clone{}, fmt.Errorf("lakebase_branch: empty credential for %s", r.endpoint)
	}
	from := branch.SourceTime
	if from.IsZero() {
		from = r.p.cfg.Now()
	}
	return Clone{DSN: lakebaseDSN(r.prod, strings.TrimSpace(endpoint.Host), token),
		ID: r.branch, CreatedFrom: from}, nil
}

// checkBranch is the identity guard on the branch: it must be the one
// asked for, with a uid that is not production's.
func (r *lakebaseCreate) checkBranch(b LakebaseBranch) error {
	if b.Name != r.branch || blank(b.UID) {
		return fmt.Errorf("lakebase_branch: branch %s cannot be shown not to be "+
			"production: the control plane returned name %q (uid set: %v)", r.branch,
			b.Name, !blank(b.UID))
	}
	if b.UID == r.prod.BranchUID || b.Name == r.prod.BranchName {
		return fmt.Errorf("%w: branch %s has production branch %s's identity",
			ErrCloneIsProduction, r.branch, r.prod.BranchName)
	}
	return nil
}

// checkEndpoint is the identity guard on the endpoint: its host must be
// set and must not be production's.
func (r *lakebaseCreate) checkEndpoint(e LakebaseEndpoint) error {
	if blank(e.Host) {
		return fmt.Errorf("lakebase_branch: endpoint %s cannot be shown not to be "+
			"production: it has no host", r.endpoint)
	}
	if sameLakebaseHost(e.Host, r.prod.Host) {
		return fmt.Errorf("%w: endpoint %s answers on production's host %s",
			ErrCloneIsProduction, r.endpoint, r.prod.Host)
	}
	return nil
}

// cleanup deletes what this Create asked for, on a fresh bounded context
// (the caller's may be cancelled). A delete that fails leaves the branch
// to its server-side expiry, and the error says the create may have left
// a clone behind.
func (r *lakebaseCreate) cleanup(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	var errs []error
	if r.endpointAsked {
		if err := r.api.DeleteEndpoint(ctx, r.endpoint); err != nil &&
			!errors.Is(err, ErrLakebaseNotFound) {
			errs = append(errs, fmt.Errorf("delete endpoint %s: %w", r.endpoint, err))
		}
	}
	if err := r.api.DeleteBranch(ctx, r.branch); err != nil &&
		!errors.Is(err, ErrLakebaseNotFound) {
		errs = append(errs, fmt.Errorf("delete branch %s: %w", r.branch, err))
	}
	if len(errs) == 0 {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("%w: lakebase_branch cleanup failed; the server "+
		"deletes branch %s when it expires at %s: %w", ErrCloneLeftBehind, r.branch,
		r.expire.UTC().Format(time.RFC3339), errors.Join(errs...)))
}

// Destroy deletes the endpoint, then the branch, of a clone this provider
// names (projects/<project>/branches/sage-rh-<8 hex>); any other id is
// refused before the control plane is called. A resource already gone
// (the server expiry) counts as deleted.
func (p *LakebaseBranchProvider) Destroy(ctx context.Context, target Clone) error {
	if !p.owned.MatchString(target.ID) {
		return fmt.Errorf("%w: %q is not a rehearsal branch of project %s "+
			"(sage-rh-<8 hex>); nothing deleted", ErrInvalidLakebaseConfig, target.ID,
			p.cfg.Project)
	}
	api, err := p.cfg.API()
	if err != nil {
		return fmt.Errorf("%w: delete branch %s needs Databricks unified auth: %w",
			ErrCloneUnavailable, target.ID, err)
	}
	endpoint := target.ID + "/endpoints/" + path.Base(target.ID)
	var errs []error
	if err := p.boundedDelete(ctx, endpoint, api.DeleteEndpoint); err != nil {
		errs = append(errs, fmt.Errorf("lakebase_branch: delete endpoint %s: %w",
			endpoint, err))
	}
	if err := p.boundedDelete(ctx, target.ID, api.DeleteBranch); err != nil {
		errs = append(errs, fmt.Errorf("lakebase_branch: delete branch %s: %w",
			target.ID, err))
	}
	return errors.Join(errs...)
}

func (p *LakebaseBranchProvider) boundedDelete(ctx context.Context, name string,
	del func(context.Context, string) error) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WaitTimeout)
	defer cancel()
	if err := del(ctx, name); err != nil && !errors.Is(err, ErrLakebaseNotFound) {
		return err
	}
	return nil
}

// waitStep runs one control-plane step bounded by timeout and names it in
// the error. A step its context cut short wraps that context error, so
// the receipt records the create as uncertain.
func waitStep[T any](ctx context.Context, timeout time.Duration, step string,
	fn func(context.Context) (T, error)) (T, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := fn(stepCtx)
	if err == nil {
		return v, nil
	}
	if cerr := stepCtx.Err(); cerr != nil && !errors.Is(err, cerr) {
		err = fmt.Errorf("%w (%w)", err, cerr)
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return v, fmt.Errorf("lakebase_branch: %s: not ready after %s: %w", step,
			timeout, err)
	}
	return v, fmt.Errorf("lakebase_branch: %s: %w", step, err)
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func sameLakebaseHost(a, b string) bool {
	norm := func(h string) string { return strings.TrimSuffix(strings.TrimSpace(h), ".") }
	return norm(a) != "" && strings.EqualFold(norm(a), norm(b))
}

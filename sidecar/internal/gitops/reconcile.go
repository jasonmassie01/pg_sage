package gitops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Options are one reconcile's inputs beside the directory.
type Options struct {
	// Mode is ModeDryRun (show the diff, write nothing) or ModeApply.
	Mode string
	// Databases are the monitored databases' names; a grant must name one.
	Databases []string
	// Leader is false on a follower replica, which only reads.
	Leader bool
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// step is one planned write with the change it reports.
type step struct {
	change Change
	apply  func(ctx context.Context, tx pgx.Tx) error
}

// state is what the control database holds, read under the sets lock.
type state struct {
	sets       map[string]policy.SetDocument
	setList    []policy.SetDocument
	principals map[string]agentguard.OwnedPrincipal
	grants     map[string]auth.DatabaseGrant // user id|database
	users      map[string]auth.GrantUser     // lower-cased email
	userByID   map[int]string
}

// planner turns a directory and the stored state into steps.
type planner struct {
	dir      *Directory
	st       state
	opts     Options
	now      time.Time
	problems []Problem
	steps    []step
	notes    []Change // holds and waits: reported, never written
}

// Reconcile reconciles dir into the control database behind pool in one
// transaction. An invalid directory (a problem in a file, or a row a file
// names that the API owns) is an *InvalidError and writes nothing; a
// database error rolls the whole reconcile back. Dry runs and followers
// return the diff without writing.
func Reconcile(ctx context.Context, pool *pgxpool.Pool, dir *Directory,
	opts Options) (Result, error) {
	switch {
	case pool == nil:
		return Result{}, errors.New("gitops: no control database to reconcile into")
	case dir == nil:
		return Result{}, errors.New("gitops: no directory to reconcile")
	case opts.Mode != ModeDryRun && opts.Mode != ModeApply:
		return Result{}, fmt.Errorf("gitops: mode %q must be dry_run or apply", opts.Mode)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	res := Result{Mode: opts.Mode, Hash: dir.Hash, Follower: !opts.Leader}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("gitops: begin the reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p := &planner{dir: dir, opts: opts, now: now()}
	if err := p.load(ctx, tx); err != nil {
		return res, err
	}
	if err := p.plan(ctx, tx); err != nil {
		return res, err
	}
	res.Changes = p.changes()
	if opts.Mode == ModeDryRun || !opts.Leader || res.Writes() == 0 {
		return res, nil
	}
	if err := p.apply(ctx, tx); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("gitops: commit the reconcile of %s: %w", dir.Root, err)
	}
	res.Applied = true
	return res, nil
}

// load locks the sets and reads every row a file may name.
func (p *planner) load(ctx context.Context, tx pgx.Tx) error {
	if err := policy.LockSetsTx(ctx, tx); err != nil {
		return fmt.Errorf("gitops: %w", err)
	}
	sets, err := policy.SetsTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("gitops: %w", err)
	}
	principals, err := agentguard.OwnedPrincipalsTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("gitops: %w", err)
	}
	grants, err := auth.AllGrantsTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("gitops: %w", err)
	}
	users, err := auth.GrantUsersTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("gitops: %w", err)
	}
	p.st = state{sets: map[string]policy.SetDocument{}, setList: sets,
		principals: map[string]agentguard.OwnedPrincipal{},
		grants:     map[string]auth.DatabaseGrant{}, users: users, userByID: map[int]string{}}
	for _, s := range sets {
		p.st.sets[s.Set.Name] = s
	}
	for _, pr := range principals {
		p.st.principals[pr.Name] = pr
	}
	for _, g := range grants {
		p.st.grants[grantKey(g.UserID, g.Database)] = g
	}
	for email, u := range users {
		p.st.userByID[u.ID] = email
	}
	return nil
}

func grantKey(userID int, database string) string {
	return fmt.Sprintf("%d|%s", userID, database)
}

// plan computes every step; any problem fails the whole reconcile.
func (p *planner) plan(ctx context.Context, tx pgx.Tx) error {
	if err := p.planSets(ctx, tx); err != nil {
		return err
	}
	if err := p.planPrincipals(ctx, tx); err != nil {
		return err
	}
	p.planGrants()
	if len(p.problems) > 0 {
		return &InvalidError{Problems: p.problems}
	}
	return nil
}

func (p *planner) changes() []Change {
	out := make([]Change, 0, len(p.steps)+len(p.notes))
	for _, s := range p.steps {
		out = append(out, s.change)
	}
	return append(out, p.notes...)
}

// apply runs the steps, then writes the reconcile's one audit entry.
func (p *planner) apply(ctx context.Context, tx pgx.Tx) error {
	for _, s := range p.steps {
		if err := s.apply(ctx, tx); err != nil {
			return fmt.Errorf("gitops: %s: %s %s %s: %w", s.change.File, s.change.Op,
				s.change.Kind, s.change.Name, err)
		}
	}
	changes := p.changes()
	if err := auth.RecordAuthAudit(ctx, tx, auth.AuthAuditEvent{
		Event: auth.AuditConfigReconciled,
		Detail: map[string]any{"directory": p.dir.Root, "hash": p.dir.Hash,
			"writes": len(p.steps), "changes": changes}}); err != nil {
		return fmt.Errorf("gitops: audit the reconcile: %w", err)
	}
	return nil
}

func (p *planner) add(c Change, apply func(context.Context, pgx.Tx) error) {
	p.steps = append(p.steps, step{change: c, apply: apply})
}

func (p *planner) note(c Change) { p.notes = append(p.notes, c) }

func (p *planner) problem(pr Problem) { p.problems = append(p.problems, pr) }

package agentposture

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ArmSkip records a detector arm that does not apply to the server.
type ArmSkip struct {
	Arm    string
	Reason string
}

// Outcome is one detector's completed run.
type Outcome struct {
	Detector string
	Findings []Finding
	Skipped  []ArmSkip
}

// Note states the skipped arms with their reasons, for the check note.
func (o Outcome) Note() string {
	parts := make([]string, len(o.Skipped))
	for i, s := range o.Skipped {
		parts[i] = "arm " + s.Arm + " skipped: " + s.Reason
	}
	return strings.Join(parts, "; ")
}

// RunDetector runs d against q and validates what it reports: every
// finding names d, an object, an object type and a title, and stays at
// or below d's declared severity. A detector error or an invalid finding
// fails the run with d's id in the error; no finding is returned then.
func RunDetector(ctx context.Context, d Detector, q Querier, env Env) (Outcome, error) {
	if d == nil {
		return Outcome{}, fmt.Errorf("%w: nil detector", ErrInvalidDetector)
	}
	spec := d.Spec()
	if err := ctx.Err(); err != nil {
		return Outcome{}, fmt.Errorf("agent posture %s: %w", spec.ID, err)
	}
	out := Outcome{Detector: spec.ID}
	in := Input{Q: q, Env: env, active: map[string]bool{}}
	for _, a := range spec.Arms {
		if a.Applies(env.VersionNum) {
			in.active[a.Name] = true
		} else {
			out.Skipped = append(out.Skipped, ArmSkip{Arm: a.Name, Reason: a.SkipReason})
		}
	}
	found, err := d.Detect(ctx, in)
	if err != nil {
		return Outcome{Detector: spec.ID}, fmt.Errorf("agent posture %s: %w", spec.ID, err)
	}
	for i := range found {
		if found[i].Detector == "" {
			found[i].Detector = spec.ID
		}
		if err := validFinding(spec, found[i]); err != nil {
			return Outcome{Detector: spec.ID}, err
		}
	}
	out.Findings = found
	return out, nil
}

func validFinding(spec Spec, f Finding) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: %s on %q: %s", ErrInvalidFinding, spec.ID, f.Object, why)
	}
	switch {
	case f.Detector != spec.ID:
		return bad("reported as " + f.Detector)
	case !f.Severity.Valid():
		return bad(fmt.Sprintf("severity %q is not info, warning or critical", f.Severity))
	case !f.Severity.AtMost(spec.Severity):
		return bad(fmt.Sprintf("severity %s is above the declared %s", f.Severity,
			spec.Severity))
	case f.Object == "" || f.ObjectType == "":
		return bad("no object identity")
	case f.Title == "":
		return bad("no title")
	}
	return nil
}

// DefaultStatementTimeout is posture's per-statement budget outside the
// first look: the first look's own budget (spec §6.15).
const DefaultStatementTimeout = 5 * time.Second

// RunOptions configure RunAll.
type RunOptions struct {
	Registry         *Registry // nil: Default()
	Config           Config
	StatementTimeout time.Duration     // 0: DefaultStatementTimeout
	Observations     *ObservationStore // kept between runs; nil: none
}

// Result is one run of every detector.
type Result struct {
	Env      Env
	Outcomes []Outcome        // the detectors that completed, by id
	Failed   map[string]error // detector id -> why it did not complete
}

// EvaluatedCategories are the categories of the detectors that completed:
// their open findings that were not reported again may resolve.
func (r Result) EvaluatedCategories() []string {
	out := make([]string, 0, len(r.Outcomes))
	for _, o := range r.Outcomes {
		out = append(out, CategoryPrefix+o.Detector)
	}
	sort.Strings(out)
	return out
}

// Findings lists every completed detector's findings.
func (r Result) Findings() []Finding {
	var out []Finding
	for _, o := range r.Outcomes {
		out = append(out, o.Findings...)
	}
	return out
}

// RunAll runs every detector of the registry in one read-only transaction
// with JIT off and a statement timeout; each detector runs under its own
// savepoint, so one failure does not stop the others. It fails only when
// the transaction or the environment cannot be had, or ctx ends.
func RunAll(ctx context.Context, pool *pgxpool.Pool, opts RunOptions) (Result, error) {
	if pool == nil {
		return Result{}, ErrNoPool
	}
	if err := opts.Config.Validate(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("agent posture: %w", err)
	}
	tx, err := beginRead(ctx, pool, opts.StatementTimeout)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // read-only
	env, err := ResolveEnv(ctx, tx, opts.Config)
	if err != nil {
		return Result{}, err
	}
	env.Observations = opts.Observations
	reg := opts.Registry
	if reg == nil {
		reg = Default()
	}
	res := Result{Env: env, Failed: map[string]error{}}
	for _, d := range reg.Detectors() {
		out, err := runSaved(ctx, tx, d, env)
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("agent posture: %w", ctx.Err())
		}
		if err != nil {
			res.Failed[d.Spec().ID] = err
			continue
		}
		res.Outcomes = append(res.Outcomes, out)
	}
	return res, nil
}

// runSaved runs one detector under a savepoint of tx.
func runSaved(ctx context.Context, tx pgx.Tx, d Detector, env Env) (Outcome, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("agent posture %s: savepoint: %w", d.Spec().ID, err)
	}
	out, err := RunDetector(ctx, d, sp, env)
	if err != nil {
		_ = sp.Rollback(ctx) // the detector's error is the one to report
		return Outcome{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return Outcome{}, fmt.Errorf("agent posture %s: release savepoint: %w",
			d.Spec().ID, err)
	}
	return out, nil
}

// beginRead opens a read-only repeatable-read transaction with JIT off
// and the statement timeout set locally.
func beginRead(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) (pgx.Tx,
	error) {
	if timeout <= 0 {
		timeout = DefaultStatementTimeout
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("agent posture: begin read-only transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, Tag+`SELECT pg_catalog.set_config('jit', 'off', true),
		pg_catalog.set_config('statement_timeout', $1, true)`,
		strconv.FormatInt(timeout.Milliseconds(), 10)); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("agent posture: set JIT and statement timeout: %w", err)
	}
	return tx, nil
}

// transient reports whether err may pass when the run is repeated: a
// timeout, a lock or serialization conflict, a lost connection.
func transient(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57014", "55P03", "40001", "40P01":
			return true
		}
		return false
	}
	return !errors.Is(err, ErrInvalidFinding) && !errors.Is(err, ErrInvalidDetector)
}

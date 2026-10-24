// Package receipt is Change Review's post-deploy half: it watches each
// completed review's change land, decides its receipt once the window
// after landing closes, and scores the shadow auto-clear flag.
//
// Landing comes from the review's catalog post-conditions (bracketed
// between the last check that saw them unmet and the check that saw them
// hold), a signed change-feed deploy, migration or DDL event whose link is
// the review's PR, or report_change_deployed. A review whose
// post-conditions already hold when first checked is not applicable; one
// that never lands expires after landing_expiry_days.
//
// The receipt compares the watched statements before and after landing
// with verify.DecideTargets over sage.query_store (read through
// histstore), looks for lock incidents and lock_chain findings on the
// touched relations within 15 minutes of landing, lists born queryids and
// treats statistics resets, restarts and failovers as confounders. It is
// decided once (a conditional UPDATE) and sealed with receipt_hash; credit
// moves only through changereview's state machine.
//
// Nothing here runs the reviewed change; the only writes are the
// foundation's sage.change_review transitions and landing_checked_at.
package receipt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
)

// Cycle bounds.
const (
	// CycleInterval is how often the leader runs a cycle.
	CycleInterval = 10 * time.Minute
	// MaxAwaitingPerCycle bounds the awaiting reviews one cycle evaluates
	// per database (oldest completion first).
	MaxAwaitingPerCycle = 200
	// MaxReceiptsPerCycle bounds the receipts one cycle decides per
	// database (oldest landing first).
	MaxReceiptsPerCycle = 100
	// MaxShadowPerCycle bounds the shadow scores one cycle records.
	MaxShadowPerCycle = 200
	// maxReportErrors bounds the errors one report keeps.
	maxReportErrors = 20
)

// Database is one monitored database the job covers: its Change Review
// service (store and configuration), its pool (catalog, sage tables and,
// through histstore, its history) and its change feed (nil: none).
type Database struct {
	Name    string
	Service *changereview.Service
	Pool    *pgxpool.Pool
	Changes ChangeFeed
}

// Deps configure the job. Timeouts bound each catalog read (zero:
// catalogread.Default(); production passes catalogread.FromSafety).
type Deps struct {
	Timeouts catalogread.Timeouts
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// Report is what one cycle did on one database. Lost counts transitions a
// concurrent writer made first (changereview.ErrAlreadyDecided).
type Report struct {
	Database      string   `json:"database"`
	Evaluated     int      `json:"evaluated"`
	Landed        int      `json:"landed"`
	NotApplicable int      `json:"not_applicable"`
	Expired       int      `json:"expired"`
	Decided       int      `json:"decided"`
	Scored        int      `json:"scored"`
	Lost          int      `json:"lost"`
	Errors        []string `json:"errors,omitempty"`
}

func (r *Report) addError(format string, args ...any) {
	if len(r.Errors) < maxReportErrors {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// Job is the receipts job. It holds no state between cycles: everything
// it needs is in sage.change_review, so any sidecar that leads can run
// it. Safe for concurrent use; transitions are decided once.
type Job struct{ d Deps }

// NewJob returns a job with d's defaults filled in.
func NewJob(d Deps) *Job {
	if d.Timeouts == (catalogread.Timeouts{}) {
		d.Timeouts = catalogread.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return &Job{d: d}
}

// Cycle runs one cycle over dbs in name order. A database that fails is
// reported with its error; the others still run.
func (j *Job) Cycle(ctx context.Context, dbs []Database) []Report {
	sorted := append([]Database(nil), dbs...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Name < sorted[b].Name })
	out := make([]Report, 0, len(sorted))
	for _, db := range sorted {
		rep, err := j.RunDatabase(ctx, db)
		if err != nil {
			rep.Database = db.Name
			rep.addError("%v", err)
			j.d.Logf("change review receipts: %s: %v", db.Name, err)
		}
		out = append(out, rep)
	}
	return out
}

// RunDatabase runs one cycle on db: landing, due receipts, shadow scores.
// A database whose Change Review is disabled is skipped. Per-review
// failures are in the report; the error is for the database as a whole.
func (j *Job) RunDatabase(ctx context.Context, db Database) (Report, error) {
	rep := Report{Database: db.Name}
	if db.Name == "" || db.Service == nil || db.Pool == nil {
		return rep, fmt.Errorf("%w: a receipts database needs a name, a service and a pool",
			changereview.ErrInvalid)
	}
	cfg := db.Service.Config()
	if !cfg.Enabled {
		return rep, nil
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	now := j.d.Now()
	steps := []func(context.Context, Database, time.Time, *Report) error{j.land,
		j.decideDue, j.score}
	for _, step := range steps {
		if err := step(ctx, db, now, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// decideDue decides the receipts whose window after landing has closed.
func (j *Job) decideDue(ctx context.Context, db Database, now time.Time, rep *Report) error {
	hours := db.Service.Config().ReceiptWindowHours
	st := db.Service.Store()
	due, err := st.ListReceiptsDue(ctx, now.Add(-time.Duration(hours)*time.Hour),
		MaxReceiptsPerCycle)
	if err != nil {
		return fmt.Errorf("list receipts due: %w", err)
	}
	obs := Observer{Pool: db.Pool, Changes: db.Changes, Logf: j.d.Logf}
	for _, r := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		w, err := WindowsOf(r, hours)
		if err != nil {
			rep.addError("review %s: %v", r.ID, err)
			continue
		}
		o, err := obs.Observe(ctx, r, w)
		if err != nil {
			rep.addError("review %s: receipt postponed: %v", r.ID, err)
			continue
		}
		j.record(rep, r.ID, "receipt", func() error {
			_, err := st.DecideReceipt(ctx, r.ID, Decide(r, w, o))
			return err
		}, &rep.Decided)
	}
	return nil
}

// record runs one transition: counted on success, lost when another
// writer made it first, reported otherwise.
func (j *Job) record(rep *Report, id, what string, transition func() error, n *int) {
	err := transition()
	switch {
	case err == nil:
		*n++
	case errors.Is(err, changereview.ErrAlreadyDecided):
		rep.Lost++
	default:
		rep.addError("review %s: record the %s: %v", id, what, err)
	}
}

package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/store"
)

// ObserveApplicationName is the application_name of the sweeper's
// sessions. It contains pg_sage's own name, so every collector leaves
// these sessions out, and pg_stat_activity shows how many are open.
const ObserveApplicationName = selfmonitor.ApplicationName + " observe"

// observeConnectTimeout bounds the connection a visit opens.
const observeConnectTimeout = 10 * time.Second

// visitOne visits rec and records its sweep. A round that ends mid-visit
// records nothing for the member: its previous sweep stays.
func (s *ObserveSweeper) visitOne(ctx context.Context, roundID int64,
	rec store.DatabaseRecord, tally *roundTally) {
	n := tally.active.Add(1)
	defer tally.active.Add(-1)
	for p := tally.peak.Load(); n > p && !tally.peak.CompareAndSwap(p, n); {
		p = tally.peak.Load()
	}
	mctx, cancel := context.WithTimeout(ctx, s.opts.MemberTimeout)
	defer cancel()
	start := s.opts.Now()
	dsn, err := s.store.GetConnectionString(mctx, rec.ID)
	var grade ObserveGrade
	if err == nil {
		grade, err = s.opts.Visit(mctx, rec, dsn)
	} else {
		err = fmt.Errorf("reading the credentials of %s: %w", rec.Name, err)
	}
	if ctx.Err() != nil {
		return
	}
	sw := store.ObserveSweep{DatabaseID: rec.ID, RoundID: roundID, SweptAt: start,
		DurationMS: s.opts.Now().Sub(start).Milliseconds()}
	tally.mu.Lock()
	switch {
	case err == nil:
		sw.Status, sw.Letter, sw.Grade = store.ObserveOK, grade.Letter, grade.Detail
		tally.ok++
	case errors.Is(err, ErrObserveUnreachable):
		sw.Status, sw.Error = store.ObserveUnreachable, clip(err.Error())
		tally.unreach++
	default:
		sw.Status, sw.Error = store.ObserveFailed, clip(err.Error())
		tally.failed++
	}
	tally.mu.Unlock()
	if recErr := s.store.RecordObserveSweep(context.WithoutCancel(ctx), sw); recErr != nil {
		tally.mu.Lock()
		tally.errs = append(tally.errs, recErr)
		tally.mu.Unlock()
	}
}

func clip(s string) string {
	if len(s) > maxObserveErrorLen {
		return s[:maxObserveErrorLen]
	}
	return s
}

// ObserveCheck grades a member through its one connection.
type ObserveCheck func(ctx context.Context, pool *pgxpool.Pool,
	rec store.DatabaseRecord) (ObserveGrade, error)

// DefaultObserveVisit opens one read-only connection to the member, runs
// check on it and closes it.
func DefaultObserveVisit(check ObserveCheck) ObserveVisit {
	return func(ctx context.Context, rec store.DatabaseRecord, dsn string) (ObserveGrade,
		error) {
		pool, err := observeConnect(ctx, dsn)
		if err != nil {
			return ObserveGrade{}, fmt.Errorf("%w: %s: %w", ErrObserveUnreachable, rec.Name,
				err)
		}
		defer pool.Close()
		return check(ctx, pool, rec)
	}
}

// observeConnect is a pool of at most one connection, opened now, whose
// transactions are read-only and whose lock waits are bounded.
func observeConnect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid DSN: %w", err)
	}
	cfg.MaxConns, cfg.MinConns = 1, 0
	cfg.ConnConfig.ConnectTimeout = observeConnectTimeout
	selfmonitor.ConfigurePool(cfg)
	cfg.ConnConfig.RuntimeParams["application_name"] = ObserveApplicationName
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "1000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// ObserveCheckOptions configure DefaultObserveCheck.
type ObserveCheckOptions struct {
	Provider         string
	StatementTimeout time.Duration // each statement; 0 is the first look's 5s
	Now              func() time.Time
}

// ObserveGradeDetail is the stored grade of an observe-only member.
type ObserveGradeDetail struct {
	RubricVersion    string               `json:"rubric_version"`
	Overall          string               `json:"overall"`
	OverallScore     float64              `json:"overall_score"`
	CoveragePct      float64              `json:"coverage_pct"`
	Dimensions       map[string]string    `json:"dimensions"`
	Checks           []rubric.CheckResult `json:"checks"`
	FirstLookItems   int                  `json:"first_look_items"`
	ExposureFindings int                  `json:"exposure_findings"`
}

// DefaultObserveCheck is the observe-only subset: the first look's
// read-only catalog checks with only the exposure detectors
// (fleetpol-02a) as its agent posture section, the settings probe, and
// review-01's deterministic rubric over them. Producers that need a
// runtime (findings, drills, lint) grade unknown, never pass.
func DefaultObserveCheck(o ObserveCheckOptions) ObserveCheck {
	return func(ctx context.Context, pool *pgxpool.Pool,
		rec store.DatabaseRecord) (ObserveGrade, error) {
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		report, err := firstlook.Run(ctx, pool, firstlook.Options{Database: rec.Name,
			Provider: o.Provider, StatementTimeout: o.StatementTimeout, Now: now,
			PostureRegistry: agentposture.ExposureRegistry()})
		if err != nil {
			return ObserveGrade{}, fmt.Errorf("first look of %s: %w", rec.Name, err)
		}
		timeout := o.StatementTimeout
		if timeout <= 0 {
			timeout = firstlook.DefaultStatementTimeout
		}
		in := rubric.Inputs{Provider: o.Provider, Now: now().UTC(),
			FirstLook: rubric.FirstLookInput{Found: true, Report: report},
			Settings:  rubric.ProbeSettings(ctx, pool, timeout)}
		return observeGrade(in, report)
	}
}

func observeGrade(in rubric.Inputs, report firstlook.Report) (ObserveGrade, error) {
	r := rubric.V1()
	results := rubric.Evaluate(r, in)
	g := rubric.Compute(r, results)
	d := ObserveGradeDetail{RubricVersion: g.RubricVersion, Overall: string(g.Overall),
		OverallScore: g.OverallScore, CoveragePct: g.CoveragePct,
		Dimensions: map[string]string{}, Checks: results, FirstLookItems: len(report.Items)}
	for _, dim := range g.Dimensions {
		d.Dimensions[dim.Dimension] = string(dim.Letter)
	}
	for _, it := range report.Items {
		if it.Section == firstlook.SectionAgentPosture && it.IsFinding() {
			d.ExposureFindings++
		}
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ObserveGrade{}, fmt.Errorf("encoding the grade: %w", err)
	}
	return ObserveGrade{Letter: d.Overall, Detail: b}, nil
}

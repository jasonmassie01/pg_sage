package rubric

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/schema/lint"
)

// DefaultProbeTimeout bounds each read the loader runs on the database.
const DefaultProbeTimeout = 5 * time.Second

// FirstLookReader reads the newest first look (firstlook.Store).
type FirstLookReader interface {
	Latest(ctx context.Context, database string) (firstlook.Report, bool, error)
}

// DrillReader reads the restore drill history (drill.Store).
type DrillReader interface {
	LastPassedResult(ctx context.Context, database string) (*drill.Result, error)
	LastAttempt(ctx context.Context, database string) (drill.Attempt, bool, error)
}

// LintScanner scans the schema lint rules (lint.Linter), read-only.
type LintScanner interface {
	ScanReport(ctx context.Context) ([]lint.Finding, map[string]bool, error)
}

// DBLoader reads a database's producer results from its own sage schema
// and a bounded, read-only probe of pg_settings. Nil readers default to
// the database's own stores; a nil Lint leaves the lint checks unknown.
type DBLoader struct {
	Pool         *pgxpool.Pool
	Database     string
	Provider     string
	FirstLook    FirstLookReader
	Drills       DrillReader
	Lint         LintScanner
	LintDisabled []string
	CostRates    bool
	ProbeTimeout time.Duration
	Now          func() time.Time
}

// Load reads every part; each part that fails keeps its error, and the
// joined errors are returned with the inputs.
func (l *DBLoader) Load(ctx context.Context) (Inputs, error) {
	if l == nil || l.Pool == nil {
		return Inputs{}, ErrNoStore
	}
	in := Inputs{Provider: l.Provider, Now: l.now(), CostRates: l.CostRates}
	in.FirstLook = l.firstLook(ctx)
	in.Findings = l.findings(ctx)
	in.Lint = l.lint(ctx)
	in.Drill = l.drill(ctx)
	in.Settings = l.settings(ctx)
	err := errors.Join(wrapPart("firstlook", in.FirstLook.Err),
		wrapPart("findings", in.Findings.Err), wrapPart("schema_lint", in.Lint.Err),
		wrapPart("drill", in.Drill.Err), wrapPart("settings", in.Settings.Err))
	return in, err
}

func wrapPart(part string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", part, err)
}

func (l *DBLoader) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *DBLoader) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	d := l.ProbeTimeout
	if d <= 0 {
		d = DefaultProbeTimeout
	}
	return context.WithTimeout(ctx, d)
}

func (l *DBLoader) firstLook(ctx context.Context) FirstLookInput {
	reader := l.FirstLook
	if reader == nil {
		reader = firstlook.NewStore(l.Pool)
	}
	ctx, cancel := l.bounded(ctx)
	defer cancel()
	report, found, err := reader.Latest(ctx, l.Database)
	return FirstLookInput{Err: err, Found: found && err == nil, Report: report}
}

func (l *DBLoader) drill(ctx context.Context) DrillInput {
	reader := l.Drills
	if reader == nil {
		reader = drill.NewStore(l.Pool)
	}
	ctx, cancel := l.bounded(ctx)
	defer cancel()
	attempt, ok, err := reader.LastAttempt(ctx, l.Database)
	if err != nil {
		return DrillInput{Err: err}
	}
	out := DrillInput{Attempted: ok, LastAttemptPassed: attempt.Passed}
	pass, err := reader.LastPassedResult(ctx, l.Database)
	if err != nil {
		return DrillInput{Err: err}
	}
	if pass != nil {
		out.LastPassedID, out.LastPassedAt = pass.ID, pass.FinishedAt
	}
	return out
}

func (l *DBLoader) lint(ctx context.Context) LintInput {
	out := LintInput{Disabled: map[string]bool{}, Findings: map[string]string{}}
	for _, id := range l.LintDisabled {
		out.Disabled[id] = true
	}
	if l.Lint == nil {
		return out
	}
	findings, failed, err := l.Lint.ScanReport(ctx)
	if err != nil {
		out.Err = err
		return out
	}
	out.Ran, out.Failed = true, failed
	for _, f := range findings {
		out.Findings[f.RuleID] = worstOf(out.Findings[f.RuleID], f.Severity)
	}
	return out
}

// settings reads the server version and the settings waiting for a
// restart, in a read-only transaction under statement_timeout.
func (l *DBLoader) settings(ctx context.Context) SettingsInput {
	ctx, cancel := l.bounded(ctx)
	defer cancel()
	var out SettingsInput
	err := pgx.BeginTxFunc(ctx, l.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)",
				fmt.Sprint(l.timeoutMS())); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `/* pg_sage review v1 */
				SELECT current_setting('server_version_num')::int,
				       coalesce(array_agg(name ORDER BY name) FILTER (WHERE pending_restart),
				                '{}')
				FROM pg_catalog.pg_settings`).Scan(&out.ServerVersionNum, &out.PendingRestart)
		})
	if err != nil {
		return SettingsInput{Err: fmt.Errorf("read pg_settings: %w", err)}
	}
	return out
}

func (l *DBLoader) timeoutMS() int64 {
	if l.ProbeTimeout > 0 {
		return l.ProbeTimeout.Milliseconds()
	}
	return DefaultProbeTimeout.Milliseconds()
}

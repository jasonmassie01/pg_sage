package managedparam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Resolver resolves the provider resource behind the database (its
// parameter group and parameters, or its database flags).
type Resolver interface {
	Target(ctx context.Context) (Target, error)
}

// WorkerOptions configure one database's managed-change worker.
type WorkerOptions struct {
	Database string
	Provider string
	Pool     *pgxpool.Pool
	Store    *Store
	Resolver Resolver // nil: proposals use placeholders, no drift
	// TargetTTL caches the resolved target (default 15 minutes).
	TargetTTL time.Duration
	Now       func() time.Time
}

// CycleResult is what one cycle did.
type CycleResult struct {
	Proposed    int      `json:"proposed"`
	Refreshed   int      `json:"refreshed"`
	Skipped     int      `json:"skipped"`
	Superseded  int      `json:"superseded"`
	Applied     int      `json:"applied"`
	TargetError string   `json:"target_error,omitempty"`
	BuildErrors []string `json:"build_errors,omitempty"`
	Drift       []Drift  `json:"drift"`
}

// Worker turns open findings that carry a managed intent into stored
// proposals, observes when their value runs, and detects drift.
type Worker struct {
	opts     WorkerOptions
	store    *Store
	provider string

	mu        sync.Mutex
	target    Target
	targetAt  time.Time
	targetErr string
	drift     []Drift
}

// NewWorker validates the options.
func NewWorker(opts WorkerOptions) (*Worker, error) {
	provider := NormalizeProvider(opts.Provider)
	switch {
	case opts.Database == "":
		return nil, errors.New("managed change worker needs a database name")
	case provider == "":
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, opts.Provider)
	case opts.Pool == nil || opts.Store == nil:
		return nil, errors.New("managed change worker needs a pool and a store")
	}
	if opts.TargetTTL <= 0 {
		opts.TargetTTL = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Worker{opts: opts, store: opts.Store, provider: provider}, nil
}

// Drift is the drift found by the last cycle.
func (w *Worker) Drift() []Drift {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Drift(nil), w.drift...)
}

type findingIntent struct {
	findingID int64
	intent    Intent
}

// Cycle runs one pass. Target resolution failures are reported in the
// result, never as a failed cycle: proposals then carry placeholders.
func (w *Worker) Cycle(ctx context.Context) (CycleResult, error) {
	var res CycleResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	intents, err := w.readIntents(ctx)
	if err != nil {
		return res, err
	}
	target := w.resolve(ctx, &res)
	settings, err := w.readSettings(ctx, settingNames(intents, target))
	if err != nil {
		return res, err
	}
	keep := map[string]bool{}
	for _, fi := range intents {
		if err := w.propose(ctx, fi, target, settings, keep, &res); err != nil {
			return res, err
		}
	}
	if res.Superseded, err = w.store.SupersedeExcept(ctx, keep); err != nil {
		return res, err
	}
	res.Drift = DetectDrift(target, settingList(settings))
	w.mu.Lock()
	w.drift = res.Drift
	w.mu.Unlock()
	return res, nil
}

func (w *Worker) propose(ctx context.Context, fi findingIntent, target Target,
	settings map[string]collector.PGSetting, keep map[string]bool, res *CycleResult) error {
	if NormalizeProvider(fi.intent.Provider) != w.provider {
		res.Skipped++
		return nil
	}
	var running *collector.PGSetting
	if s, ok := settings[fi.intent.Parameter]; ok {
		running = &s
	}
	p, err := Build(fi.intent, target, running)
	if err != nil {
		res.Skipped++
		res.BuildErrors = append(res.BuildErrors, fmt.Sprintf("finding %d: %v",
			fi.findingID, err))
		return nil
	}
	keep[p.Fingerprint] = true
	live := running != nil && !running.PendingRestart && sameValue(p.Value, running.Setting)
	if live {
		done, err := w.store.appliedExists(ctx, p.Fingerprint)
		if err != nil || done {
			return err
		}
	}
	rec, created, err := w.store.Upsert(ctx, p, fi.findingID)
	if err != nil {
		return err
	}
	if created {
		res.Proposed++
	} else {
		res.Refreshed++
	}
	if live && (rec.Status == StatusPending || rec.Status == StatusApproved) {
		if err := w.store.MarkApplied(ctx, rec.ID, w.opts.Now().UTC()); err != nil {
			return err
		}
		res.Applied++
	}
	return nil
}

// resolve returns the cached or freshly resolved target; on failure an
// unresolved target that says why.
func (w *Worker) resolve(ctx context.Context, res *CycleResult) Target {
	unresolved := Target{Provider: w.provider}
	if w.opts.Resolver == nil {
		unresolved.Unresolved = "cloud telemetry is not configured"
		res.TargetError = unresolved.Unresolved
		return unresolved
	}
	w.mu.Lock()
	cached, at, cachedErr := w.target, w.targetAt, w.targetErr
	w.mu.Unlock()
	if !at.IsZero() && w.opts.Now().Sub(at) < w.opts.TargetTTL && cachedErr == "" {
		return cached
	}
	t, err := w.opts.Resolver.Target(ctx)
	if err != nil {
		unresolved.Unresolved = err.Error()
		res.TargetError = unresolved.Unresolved
		w.mu.Lock()
		w.target, w.targetAt, w.targetErr = unresolved, w.opts.Now(), res.TargetError
		w.mu.Unlock()
		return unresolved
	}
	w.mu.Lock()
	w.target, w.targetAt, w.targetErr = t, w.opts.Now(), ""
	w.mu.Unlock()
	return t
}

func (w *Worker) readIntents(ctx context.Context) ([]findingIntent, error) {
	rows, err := w.opts.Pool.Query(ctx, `/* pg_sage */ SELECT id, detail->'managed_change'
		FROM sage.findings
		WHERE status = 'open' AND detail ? 'managed_change'
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read managed change findings: %w", err)
	}
	defer rows.Close()
	var out []findingIntent
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan managed change finding: %w", err)
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if in, ok := IntentFromDetail(map[string]any{DetailKey: m}); ok {
			out = append(out, findingIntent{findingID: id, intent: in})
		}
	}
	return out, rows.Err()
}

package managedparam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// Resolver resolves the provider resource behind the database (its
// parameter group and parameters, or its database flags).
type Resolver interface {
	Target(ctx context.Context) (Target, error)
}

// ParameterResolver is a Resolver that reads parameters one at a time (an
// Azure server's configurations): the worker asks it for exactly the
// parameters its intents name, and caches the answer per parameter set.
type ParameterResolver interface {
	Resolver
	TargetFor(ctx context.Context, parameters []string) (Target, error)
}

// WorkerOptions configure one database's managed-change worker.
type WorkerOptions struct {
	Database string
	Provider string
	Pool     *pgxpool.Pool
	Store    *Store
	Resolver Resolver // nil: proposals use placeholders, no drift
	// Resource names the provider instance (clouds-08): the worker skips a
	// change a peer database of it holds (see JoinPeers). "": no peers.
	Resource string
	// TargetTTL caches the resolved target (default 15 minutes).
	TargetTTL time.Duration
	Now       func() time.Time
}

// CycleResult is what one cycle did.
type CycleResult struct {
	Proposed   int `json:"proposed"`
	Refreshed  int `json:"refreshed"`
	Skipped    int `json:"skipped"`
	Superseded int `json:"superseded"`
	Applied    int `json:"applied"`
	// HeldByPeer counts changes not proposed because a peer database of
	// the same instance holds them; PeerError reports, once, that a peer
	// could not be read (those changes were proposed here instead).
	HeldByPeer  int      `json:"held_by_peer"`
	PeerError   string   `json:"peer_error,omitempty"`
	TargetError string   `json:"target_error,omitempty"`
	BuildErrors []string `json:"build_errors,omitempty"`
	Drift       []Drift  `json:"drift"`

	peerReads int    // peer stores read this cycle
	peerErr   string // the cycle's first peer read failure
}

// Worker turns open findings that carry a managed intent into stored
// proposals, observes when their value runs, and detects drift.
type Worker struct {
	opts     WorkerOptions
	store    *Store
	provider string

	mu sync.Mutex
	// target is the last successfully resolved target and targetAt when
	// (zero: never); targetErr is the latest attempt's error, if it failed.
	// targetKey names the parameters a ParameterResolver described.
	target    Target
	targetAt  time.Time
	targetErr string
	targetKey string
	drift     []Drift
	peerErr   string // the peer read failure last reported
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
// result, never as a failed cycle: proposals are then built against the
// last resolved target (with a note), or placeholders when there is none,
// and no proposal is superseded, because an outage says nothing about
// which changes are still wanted.
func (w *Worker) Cycle(ctx context.Context) (CycleResult, error) {
	var res CycleResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	intents, refused, err := w.readIntents(ctx)
	if err != nil {
		return res, err
	}
	res.Skipped += len(refused)
	res.BuildErrors = append(res.BuildErrors, refused...)
	target, note := w.resolve(ctx, &res, intentParameters(intents))
	settings, err := w.readSettings(ctx, settingNames(intents, target))
	if err != nil {
		return res, err
	}
	keep := map[string]bool{}
	for _, fi := range intents {
		if err := w.propose(ctx, fi, target, note, settings, keep, &res); err != nil {
			return res, err
		}
	}
	resolverFailed := w.opts.Resolver != nil && res.TargetError != ""
	if !resolverFailed {
		if res.Superseded, err = w.store.SupersedeExcept(ctx, keep); err != nil {
			return res, err
		}
	}
	if res.TargetError == "" { // drift only against a target resolved now
		res.Drift = DetectDrift(target, settingList(settings))
	}
	w.mu.Lock()
	w.drift = res.Drift
	w.mu.Unlock()
	w.notePeerError(&res)
	return res, nil
}

func (w *Worker) propose(ctx context.Context, fi findingIntent, target Target, note string,
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
	if note != "" {
		p.Notes = append(p.Notes, note)
	}
	keep[p.Fingerprint] = true
	live := running != nil && !running.PendingRestart &&
		sameValue(runningForm(p), running.Setting)
	if live {
		done, err := w.store.appliedExists(ctx, p.Fingerprint)
		if err != nil || done {
			return err
		}
	}
	rec, created, held, err := w.upsert(ctx, p, fi.findingID, res)
	if err != nil || held {
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

// resolve returns the target to build proposals against: the cached or
// freshly resolved one. When the resolver fails it returns the last
// resolved target with a note naming the error, or, when nothing was ever
// resolved, an unresolved target that says why.
func (w *Worker) resolve(ctx context.Context, res *CycleResult, names []string) (Target,
	string) {
	unresolved := Target{Provider: w.provider}
	if w.opts.Resolver == nil {
		unresolved.Unresolved = "cloud telemetry is not configured"
		res.TargetError = unresolved.Unresolved
		return unresolved, ""
	}
	key := w.parameterKey(names)
	w.mu.Lock()
	cached, at, cachedErr, cachedKey := w.target, w.targetAt, w.targetErr, w.targetKey
	w.mu.Unlock()
	if !at.IsZero() && w.opts.Now().Sub(at) < w.opts.TargetTTL && cachedErr == "" &&
		cachedKey == key {
		return cached, ""
	}
	t, err := w.lookup(ctx, names)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.target, w.targetAt, w.targetErr, w.targetKey = t, w.opts.Now(), "", key
		return t, ""
	}
	res.TargetError = err.Error()
	w.targetErr = res.TargetError
	if w.targetAt.IsZero() {
		unresolved.Unresolved = res.TargetError
		return unresolved, ""
	}
	return w.target, fmt.Sprintf("Resolving the provider resource failed (%s): this "+
		"proposal uses the target last resolved at %s. Check that it is still the "+
		"instance's before running the command.", res.TargetError,
		w.targetAt.UTC().Format(time.RFC3339))
}

// parameterKey names the parameters a ParameterResolver describes ("" for
// a resolver that resolves the whole target at once).
func (w *Worker) parameterKey(names []string) string {
	if _, ok := w.opts.Resolver.(ParameterResolver); ok {
		return strings.Join(names, ",")
	}
	return ""
}

// lookup resolves the target, through TargetFor when the resolver reads
// parameters one at a time.
func (w *Worker) lookup(ctx context.Context, names []string) (Target, error) {
	if pr, ok := w.opts.Resolver.(ParameterResolver); ok {
		return pr.TargetFor(ctx, names)
	}
	return w.opts.Resolver.Target(ctx)
}

// intentParameters are the distinct parameters the intents name, sorted.
func intentParameters(intents []findingIntent) []string {
	names := make([]string, 0, len(intents))
	for _, fi := range intents {
		names = append(names, fi.intent.Parameter)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// producerCategories are the finding categories whose managed intents
// pg_sage itself writes, after its configuration gates: the advisor's
// config sub-advisors and the tuning agent (memory_tuning,
// vacuum_tuning). Any other category's managed_change is not trusted.
var producerCategories = []string{"connection_tuning", "memory_tuning", "vacuum_tuning",
	"wal_tuning"}

// ProducerCategories returns a copy of the categories whose findings may
// carry a managed intent the worker proposes.
func ProducerCategories() []string { return append([]string(nil), producerCategories...) }

// intentRefusal says why the worker refuses a finding's intent ("" when
// it may propose it): the finding must come from a producer category and
// the parameter must be one the executor allowlists at instance scope.
func intentRefusal(category string, in Intent) string {
	if !slices.Contains(producerCategories, category) {
		return fmt.Sprintf("category %q does not produce managed changes", category)
	}
	if !pgconf.ExecutableGUC(in.Parameter) {
		return fmt.Sprintf("parameter %q is not on the executor's allowlist", in.Parameter)
	}
	if pgconf.DatabaseScopeApprovalOnly(in.Parameter) {
		// advisors-07: a provider change is instance-wide; this setting is
		// changed only per database (ALTER DATABASE), so it is never proposed.
		return fmt.Sprintf("parameter %q is changed only per database, never through "+
			"the provider", in.Parameter)
	}
	return ""
}

// readIntents returns the open findings' valid intents and a message for
// each one refused by intentRefusal.
func (w *Worker) readIntents(ctx context.Context) ([]findingIntent, []string, error) {
	rows, err := w.opts.Pool.Query(ctx, `/* pg_sage */ SELECT id, category,
		detail->'managed_change'
		FROM sage.findings
		WHERE status = 'open' AND detail ? 'managed_change'
		ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("read managed change findings: %w", err)
	}
	defer rows.Close()
	var out []findingIntent
	var refused []string
	for rows.Next() {
		var id int64
		var category string
		var raw []byte
		if err := rows.Scan(&id, &category, &raw); err != nil {
			return nil, nil, fmt.Errorf("scan managed change finding: %w", err)
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		in, ok := IntentFromDetail(map[string]any{DetailKey: m})
		if !ok {
			continue
		}
		if why := intentRefusal(category, in); why != "" {
			refused = append(refused, fmt.Sprintf("finding %d: refused: %s", id, why))
			continue
		}
		out = append(out, findingIntent{findingID: id, intent: in})
	}
	return out, refused, rows.Err()
}

package slo

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// staleEvaluations is how many intervals an evaluation stays current.
const staleEvaluations = 3

// Statuses reads the durable error-budget state of every configured SLO.
// An evaluation older than three intervals is unknown (evaluation_stale):
// an old "ok" is not a current "ok".
func (e *Engine) Statuses(ctx context.Context) ([]Status, error) {
	scope, err := e.BoundScope(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := e.store.Statuses(ctx, scope)
	if err != nil {
		return nil, err
	}
	now := e.now()
	out := make([]Status, 0, len(stored))
	for _, st := range stored {
		o, ok := e.objective(st.Name)
		if !ok {
			continue
		}
		if e.databaseScopedPush && o.Source == SourcePush && st.DefinitionHash != e.definitionHash(scope, o) {
			st = unevaluatedStatus(o, e.definitionHash(scope, o))
		}
		if now.Sub(st.EvaluatedAt) > staleEvaluations*e.interval {
			st = staleStatus(st)
		}
		st.Database = e.database
		out = append(out, st)
	}
	return out, nil
}

func staleStatus(st Status) Status {
	st.State, st.FastBurning, st.CustomerImpact = StateUnknown, false, false
	st.Unknown = append(append([]string{}, st.Unknown...), ReasonEvaluationStale)
	sort.Strings(st.Unknown)
	return st
}

// Status reads one SLO's state; a configured SLO not yet evaluated is
// unknown (not_evaluated).
func (e *Engine) Status(ctx context.Context, name string) (Status, error) {
	o, ok := e.objective(name)
	if !ok {
		return Status{}, fmt.Errorf("%w: %q", ErrUnknownSLO, name)
	}
	sts, err := e.Statuses(ctx)
	if err != nil {
		return Status{}, err
	}
	for _, st := range sts {
		if st.Name == name {
			return st, nil
		}
	}
	scope, err := e.BoundScope(ctx)
	if err != nil {
		return Status{}, err
	}
	return unevaluatedStatus(o, e.definitionHash(scope, o)), nil
}

func unevaluatedStatus(o Objective, definition string) Status {
	return Status{Name: o.Name, Database: o.Database, Kind: o.Kind, Source: o.Source,
		Target: o.Target, Window: FormatWindow(o.Window), State: StateUnknown,
		Unknown: []string{ReasonNotEvaluated}, Rules: []RuleResult{}, DefinitionHash: definition}
}

func (e *Engine) definitionHash(scope sre.Scope, o Objective) string {
	if !e.databaseScopedPush || o.Source != SourcePush {
		return o.Hash()
	}
	h := sha256.Sum256([]byte(o.Hash() + "\x00" + e.sampleName(scope, o)))
	return hex.EncodeToString(h[:])
}

// Transitions reads an SLO's state history, newest first.
func (e *Engine) Transitions(ctx context.Context, name string, limit int) ([]Transition,
	error) {
	if _, ok := e.objective(name); !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSLO, name)
	}
	scope, err := e.BoundScope(ctx)
	if err != nil {
		return nil, err
	}
	return e.store.Transitions(ctx, scope, name, limit)
}

// ErrorBudgetSource publishes error-budget state to the autonomy layer
// (AI-SRE-SPEC §7.3: automatic downgrade to <= L1 while an error budget
// is burning in a fast window). The engine implements it per database.
type ErrorBudgetSource interface {
	BudgetSummary(ctx context.Context) (BudgetSummary, error)
}

var _ ErrorBudgetSource = (*Engine)(nil)

// BudgetSummary is one database's error-budget state. FastBurning is
// true while any SLO (app or proxy) fires a page-level rule;
// AppFastBurning only counts registered app SLIs. Unknown names the
// SLOs whose state is unknown (unknown is never "not burning");
// UnknownApp only the registered app SLIs among them, since a proxy can
// be unknown for a structural reason (no standbys, no log access).
// AppSLOs counts the registered app SLIs.
type BudgetSummary struct {
	Database       string    `json:"database"`
	FastBurning    bool      `json:"fast_burning"`
	AppFastBurning bool      `json:"app_fast_burning"`
	AppSLOs        int       `json:"app_slos"`
	Unknown        []string  `json:"unknown"`
	UnknownApp     []string  `json:"unknown_app"`
	SLOs           []Status  `json:"slos"`
	ReadAt         time.Time `json:"read_at"`
}

// BudgetSummary reads the durable state of every SLO.
func (e *Engine) BudgetSummary(ctx context.Context) (BudgetSummary, error) {
	sts, err := e.Statuses(ctx)
	if err != nil {
		return BudgetSummary{}, err
	}
	sum := BudgetSummary{Database: e.database, Unknown: []string{}, UnknownApp: []string{},
		SLOs: sts, ReadAt: e.now()}
	for _, o := range e.objectives {
		if o.Kind == KindApp {
			sum.AppSLOs++
		}
	}
	for _, st := range sts {
		app := st.Kind == KindApp
		switch {
		case st.FastBurning:
			sum.FastBurning = true
			sum.AppFastBurning = sum.AppFastBurning || app
		case st.State == StateUnknown:
			sum.Unknown = append(sum.Unknown, st.Name)
			if app {
				sum.UnknownApp = append(sum.UnknownApp, st.Name)
			}
		}
	}
	return sum, nil
}

// HasPush reports whether name is a pushed app SLI of this engine.
func (e *Engine) HasPush(name string) bool {
	o, ok := e.objective(name)
	return ok && o.Source == SourcePush
}

// Push freshness bounds.
const (
	maxPushAge    = 10 * time.Minute
	maxPushFuture = time.Minute
)

// Push records one pushed counter sample of a push SLO. A replay is a
// no-op (created false). Samples must be fresh: observed within the last
// 10 minutes and at most a minute ahead.
func (e *Engine) Push(ctx context.Context, name string, p PushSample) (bool, error) {
	return e.push(ctx, nil, name, p)
}

// BoundScope resolves the durable registered scope, never the display name.
func (e *Engine) BoundScope(ctx context.Context) (sre.Scope, error) {
	scope, err := e.scope(ctx)
	if err == nil && e.databaseScopedPush && scope.Validate() != nil {
		err = ErrScopeMismatch
	}
	return scope, err
}

func (e *Engine) sampleName(scope sre.Scope, o Objective) string {
	if e.databaseScopedPush && o.Source == SourcePush {
		// ':' cannot occur in a public SLO name, so a scoped key cannot collide
		// with a legacy deployment-wide objective, including a future name.
		h := sha256.Sum256([]byte(string(scope.DatabaseID) + "\x00" + o.Name))
		// Full SHA-256 as base64url plus reserved prefix fits the existing
		// 64-character storage bound even for a maximal public SLO name.
		return "db:" + base64.RawURLEncoding.EncodeToString(h[:])
	}
	return o.Name
}

// ReadLastPushSample reads this engine's correctly scoped cumulative series.
// It never accepts a caller-supplied deployment/database identity or store key.
func (e *Engine) ReadLastPushSample(ctx context.Context, name, series string) (PushSample, bool, error) {
	o, ok := e.objective(name)
	if !ok {
		return PushSample{}, false, ErrUnknownSLO
	}
	if o.Source != SourcePush {
		return PushSample{}, false, ErrNotPush
	}
	if series == "" {
		series = DefaultSeries
	}
	if !seriesPattern.MatchString(series) {
		return PushSample{}, false, ErrInvalidSample
	}
	scope, err := e.BoundScope(ctx)
	if err != nil {
		return PushSample{}, false, err
	}
	return e.store.LastSample(ctx, scope.DeploymentID, e.sampleName(scope, o), series)
}

// PushScoped is the human-authenticated ingress boundary. Resolve and compare
// the expected registered identity once, then write under that exact scope.
func (e *Engine) PushScoped(ctx context.Context, expected sre.Scope, name string, p PushSample) (bool, error) {
	return e.push(ctx, &expected, name, p)
}

func (e *Engine) push(ctx context.Context, expected *sre.Scope, name string, p PushSample) (bool, error) {
	o, ok := e.objective(name)
	switch {
	case !ok:
		return false, fmt.Errorf("%w: %q", ErrUnknownSLO, name)
	case o.Source != SourcePush:
		return false, fmt.Errorf("%w: %q", ErrNotPush, name)
	}
	if p.Series == "" {
		p.Series = DefaultSeries
	}
	now := e.now()
	if p.ObservedAt.Before(now.Add(-maxPushAge)) || p.ObservedAt.After(now.Add(maxPushFuture)) {
		return false, fmt.Errorf("%w: observed_at must be within the last 10 minutes",
			ErrInvalidSample)
	}
	scope, err := e.BoundScope(ctx)
	if err != nil {
		return false, err
	}
	if expected != nil && (expected.DeploymentID == "" || expected.DatabaseID == "" || *expected != scope) {
		return false, ErrScopeMismatch
	}
	return e.store.RecordSample(ctx, scope.DeploymentID, e.sampleName(scope, o), p, nil)
}

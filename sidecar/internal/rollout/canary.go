package rollout

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Fleet canary errors.
var (
	ErrInvalidCanary     = errors.New("invalid canary request")
	ErrSourceNotVerified = errors.New("the source action is not a verified success with a rollback")
	ErrCanaryRunning     = errors.New("a canary rollout is already running")
)

// SourceAction is the action a canary rolls out, as executed and
// verified on its source database.
type SourceAction struct {
	SQL, RollbackSQL, ActionType string
	Outcome, Verification        string
}

// CanaryTarget is one fleet database as the canary uses it: its own
// recommendations (local re-verification), the operator-approved
// execution path through its gate, its rollback and its verification.
type CanaryTarget interface {
	Name() string
	SourceAction(ctx context.Context, actionLogID int64) (SourceAction, error)
	MatchingFinding(ctx context.Context, sql string) (int, bool, error)
	Execute(ctx context.Context, findingID int, sql, rollbackSQL string,
		approvedBy *int) (int64, error)
	Rollback(ctx context.Context, actionLogID int64, reason string) error
	ActionResult(ctx context.Context, actionLogID int64) (string, string, error)
}

// TargetResolver resolves a fleet database name.
type TargetResolver func(database string) (CanaryTarget, error)

// CanaryOptions are the operator's canary settings.
type CanaryOptions struct {
	CanaryInstances    int
	RegressionLimitPct float64
	// Settle is the wait after a change before measuring it.
	Settle time.Duration
	Now    func() time.Time
	Log    func(format string, args ...any)
}

// StartRequest asks for a canary of one verified action.
type StartRequest struct {
	SourceDatabase string   `json:"source_database"`
	ActionLogID    int64    `json:"action_log_id"`
	Targets        []string `json:"targets"`
	Family         string   `json:"family,omitempty"`
	Class          string   `json:"class,omitempty"`
	StartedBy      string   `json:"-"`
	ApprovedBy     *int     `json:"-"`
}

// CanaryService runs fleet canaries of remediations, one at a time.
type CanaryService struct {
	resolve TargetResolver
	runs    *PostgresRunStore
	opts    CanaryOptions
	// settle waits before a measurement (a test seam over opts.Settle).
	settle  func(context.Context) error
	running atomic.Bool
}

// NewCanaryService builds the service.
func NewCanaryService(resolve TargetResolver, runs *PostgresRunStore,
	opts CanaryOptions) (*CanaryService, error) {
	if resolve == nil || runs == nil {
		return nil, errors.New("canary service needs a target resolver and a run store")
	}
	if opts.CanaryInstances < 1 {
		opts.CanaryInstances = 1
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = log.Printf
	}
	s := &CanaryService{resolve: resolve, runs: runs, opts: opts}
	s.settle = func(ctx context.Context) error {
		if opts.Settle <= 0 {
			return nil
		}
		select {
		case <-time.After(opts.Settle):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s, nil
}

// Run validates, records and runs a canary, waiting for it.
func (s *CanaryService) Run(ctx context.Context, req StartRequest) (RunRecord, error) {
	if !s.running.CompareAndSwap(false, true) {
		return RunRecord{}, ErrCanaryRunning
	}
	defer s.running.Store(false)
	prepared, err := s.prepare(ctx, req)
	if err != nil {
		return RunRecord{}, err
	}
	return s.execute(ctx, prepared)
}

// Start validates and records a canary, then runs it in the background.
func (s *CanaryService) Start(ctx context.Context, req StartRequest) (RunRecord, error) {
	if !s.running.CompareAndSwap(false, true) {
		return RunRecord{}, ErrCanaryRunning
	}
	prepared, err := s.prepare(ctx, req)
	if err != nil {
		s.running.Store(false)
		return RunRecord{}, err
	}
	go func() {
		defer s.running.Store(false)
		if run, err := s.execute(context.WithoutCancel(ctx), prepared); err != nil {
			s.opts.Log("rollout: canary %s %s: %v", run.EvidenceID, run.State, err)
		}
	}()
	return prepared.record, nil
}

// Get reads a run and its instances.
func (s *CanaryService) Get(ctx context.Context, id string) (RunRecord, []InstanceRecord,
	error) {
	run, err := s.runs.Get(ctx, id)
	if err != nil {
		return RunRecord{}, nil, fmt.Errorf("read canary %s: %w", id, err)
	}
	instances, err := s.runs.Instances(ctx, id)
	return run, instances, err
}

// List lists the newest runs.
func (s *CanaryService) List(ctx context.Context, limit int) ([]RunRecord, error) {
	return s.runs.List(ctx, limit)
}

// preparedCanary is a validated, recorded canary ready to run.
type preparedCanary struct {
	record    RunRecord
	request   Request
	source    SourceAction
	approvedB *int
}

func (s *CanaryService) prepare(ctx context.Context, req StartRequest) (preparedCanary,
	error) {
	if err := validateStart(req); err != nil {
		return preparedCanary{}, err
	}
	source, err := s.verifiedSource(ctx, req)
	if err != nil {
		return preparedCanary{}, err
	}
	intent, err := json.Marshal(map[string]string{"sql": source.SQL,
		"rollback_sql": source.RollbackSQL, "action_type": source.ActionType})
	if err != nil {
		return preparedCanary{}, fmt.Errorf("encode canary intent: %w", err)
	}
	prior := Prior{EvidenceID: fmt.Sprintf("action_log:%s:%d", req.SourceDatabase,
		req.ActionLogID), ValidatedInstanceID: req.SourceDatabase, Intent: intent}
	request := Request{Prior: prior, Policy: s.policyFor(len(req.Targets))}
	for _, t := range req.Targets {
		request.Instances = append(request.Instances, Instance{ID: t, Class: "fleet"})
	}
	now := s.opts.Now().UTC()
	record := RunRecord{EvidenceID: "canary-" + newRunID(), SourceInstance: req.SourceDatabase,
		PriorEvidenceID: prior.EvidenceID, Policy: request.Policy, State: "canary",
		CreatedAt: now, UpdatedAt: now, Family: req.Family, Class: req.Class,
		StartedBy: req.StartedBy}
	if err := s.runs.Create(ctx, record); err != nil {
		return preparedCanary{}, err
	}
	return preparedCanary{record: record, request: request, source: source,
		approvedB: req.ApprovedBy}, nil
}

func (s *CanaryService) policyFor(targets int) Policy {
	return Policy{CanaryInstances: min(s.opts.CanaryInstances, targets),
		MaxAffectedInstances: targets, AggregateRegressionLimitPct: s.opts.RegressionLimitPct,
		RequireLocalReverification: true}
}

// verifiedSource requires a successful, verified action with a rollback.
func (s *CanaryService) verifiedSource(ctx context.Context, req StartRequest) (SourceAction,
	error) {
	target, err := s.resolve(req.SourceDatabase)
	if err != nil {
		return SourceAction{}, fmt.Errorf("%w: %v", ErrInvalidCanary, err)
	}
	src, err := target.SourceAction(ctx, req.ActionLogID)
	if err != nil {
		return SourceAction{}, fmt.Errorf("%w: %v", ErrSourceNotVerified, err)
	}
	verified := src.Verification == "" || src.Verification == "success"
	if src.Outcome != "success" || !verified || strings.TrimSpace(src.SQL) == "" ||
		strings.TrimSpace(src.RollbackSQL) == "" {
		return SourceAction{}, fmt.Errorf("%w: outcome %q, verification %q", ErrSourceNotVerified,
			src.Outcome, src.Verification)
	}
	return src, nil
}

// execute runs the engine over the targets and persists the result.
func (s *CanaryService) execute(ctx context.Context, p preparedCanary) (RunRecord, error) {
	backend := &canaryBackend{svc: s, source: p.source, approvedBy: p.approvedB,
		findings: map[string]int{}, actions: map[string]int64{}}
	result, runErr := NewEngine(backend, backend).Rollout(ctx, p.request)
	record := p.record
	record.AppliedInstances = result.AppliedInstances
	record.CanaryInstanceIDs = append([]string(nil), result.CanaryInstanceIDs...)
	record.AggregateRegressionPct = result.AggregateRegressionPct
	record.UpdatedAt = s.opts.Now().UTC()
	switch {
	case runErr != nil:
		record.State, record.HaltReason = "failed", "rollout_error"
	case result.Halted:
		record.State, record.HaltReason = "halted", result.HaltReason
	default:
		record.State = "complete"
	}
	persistCtx := context.WithoutCancel(ctx)
	err := errors.Join(runErr, s.runs.Update(persistCtx, record),
		s.runs.SaveInstances(persistCtx, record.EvidenceID, backend.records(result,
			record.UpdatedAt)))
	return record, err
}

func validateStart(req StartRequest) error {
	seen := map[string]bool{}
	for _, t := range req.Targets {
		if strings.TrimSpace(t) == "" || seen[t] || t == req.SourceDatabase {
			return fmt.Errorf("%w: target %q is empty, repeated or the source", ErrInvalidCanary, t)
		}
		seen[t] = true
	}
	switch {
	case strings.TrimSpace(req.SourceDatabase) == "" || req.ActionLogID <= 0:
		return fmt.Errorf("%w: a source database and action are required", ErrInvalidCanary)
	case len(req.Targets) == 0 || len(req.Targets) > 100:
		return fmt.Errorf("%w: 1-100 target databases", ErrInvalidCanary)
	case !humanStarter(req.StartedBy):
		return fmt.Errorf("%w: a canary is started by a person", ErrInvalidCanary)
	case req.Family != "" && !earned.KnownFamily(earned.Family(req.Family)):
		return fmt.Errorf("%w: unknown family %q", ErrInvalidCanary, req.Family)
	case req.Class != "" && !knownClass(req.Class):
		return fmt.Errorf("%w: unknown action class %q", ErrInvalidCanary, req.Class)
	}
	return nil
}

func knownClass(class string) bool {
	_, ok := earned.Spec(earned.ActionClass(class))
	return ok
}

func humanStarter(actor string) bool {
	actor = strings.TrimSpace(actor)
	for _, prefix := range []string{earned.ActorPgSage, "system", "mcp"} {
		if strings.HasPrefix(actor, prefix) {
			return false
		}
	}
	return actor != ""
}

func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("%x", b)
}

// canaryBackend applies the source action on each target through the
// target's own approval path.
type canaryBackend struct {
	svc        *CanaryService
	source     SourceAction
	approvedBy *int
	mu         sync.Mutex
	findings   map[string]int
	actions    map[string]int64
	order      []string
}

// Reverify requires the target's own analyzer to recommend the same SQL.
func (b *canaryBackend) Reverify(ctx context.Context, instance Instance,
	_ Prior) (Reverification, error) {
	b.note(instance.ID)
	target, err := b.svc.resolve(instance.ID)
	if err != nil {
		return Reverification{}, err
	}
	id, found, err := target.MatchingFinding(ctx, b.source.SQL)
	if err != nil || !found {
		return Reverification{Reason: "no matching local recommendation"}, err
	}
	b.mu.Lock()
	b.findings[instance.ID] = id
	b.mu.Unlock()
	return Reverification{Eligible: true, LocalEvidenceID: "finding:" + strconv.Itoa(id)}, nil
}

// Apply executes the action as the operator's approval on the target.
func (b *canaryBackend) Apply(ctx context.Context, instance Instance,
	_ Reverification) (AppliedChange, error) {
	target, err := b.svc.resolve(instance.ID)
	if err != nil {
		return AppliedChange{}, err
	}
	b.mu.Lock()
	findingID := b.findings[instance.ID]
	b.mu.Unlock()
	actionID, err := target.Execute(ctx, findingID, b.source.SQL, b.source.RollbackSQL,
		b.approvedBy)
	if err != nil {
		return AppliedChange{}, err
	}
	b.mu.Lock()
	b.actions[instance.ID] = actionID
	b.mu.Unlock()
	return AppliedChange{Handle: strconv.FormatInt(actionID, 10)}, nil
}

// Measure waits for the change to settle and reads its verification.
func (b *canaryBackend) Measure(ctx context.Context, instance Instance,
	change AppliedChange) (Outcome, error) {
	if err := b.svc.settle(ctx); err != nil {
		return Outcome{}, err
	}
	target, err := b.svc.resolve(instance.ID)
	if err != nil {
		return Outcome{}, err
	}
	actionID, _ := strconv.ParseInt(change.Handle, 10, 64)
	outcome, verification, err := target.ActionResult(ctx, actionID)
	if err != nil {
		return Outcome{}, err
	}
	o := Outcome{EvidenceID: "action_log:" + change.Handle,
		Detail: fmt.Sprintf("outcome %s, verification %q", outcome, verification)}
	switch verification {
	case "failed", "revert", "unverifiable":
		o.Failed = true
	}
	if outcome != "success" {
		o.Failed = true
	}
	if o.Failed {
		o.RegressionPct = 100
	}
	return o, nil
}

// Rollback undoes the change on the target.
func (b *canaryBackend) Rollback(ctx context.Context, instance Instance,
	change AppliedChange) error {
	target, err := b.svc.resolve(instance.ID)
	if err != nil {
		return err
	}
	actionID, _ := strconv.ParseInt(change.Handle, 10, 64)
	return target.Rollback(ctx, actionID, "fleet canary halted: rolled back")
}

func (b *canaryBackend) note(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = append(b.order, id)
}

// records are the instance results in rollout order.
func (b *canaryBackend) records(result Result, at time.Time) []InstanceRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]InstanceRecord, 0, len(b.order))
	for i, id := range b.order {
		res, ok := result.Instances[id]
		if !ok {
			continue
		}
		out = append(out, InstanceRecord{InstanceID: id, Ordinal: i + 1, Status: res.Status,
			EvidenceID: res.EvidenceID, ActionLogID: b.actions[id],
			RegressionPct: res.RegressionPct, Detail: res.Detail, UpdatedAt: at})
	}
	return out
}

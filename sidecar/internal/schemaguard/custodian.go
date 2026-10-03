package schemaguard

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Detector interface {
	Detect(context.Context) ([]Invariant, error)
}

// ContractSource reads the owners' table contracts for a scan's invariants
// in one lookup, keyed by Invariant.Target. A table without a contract is
// absent.
type ContractSource interface {
	Contracts(context.Context, []Invariant) (map[string]TableContract, error)
}

// HistorySource reads a scan's recorded history in one lookup.
type HistorySource interface {
	History(context.Context, []Invariant) (HistoryIndex, error)
}

type Remediation struct {
	Invariant Invariant
	Contract  TableContract
	Decision  Decision
}

type RemediationRouter interface {
	Route(context.Context, Remediation) error
}

type DecisionRecorder interface {
	Record(context.Context, DecisionRecord) error
}

// CycleResult counts one scan: Recorded is ledger rows written, Unchanged
// is identities whose decision was already recorded, Skipped is invariants
// of idle leftover families parked without planning or routing.
type CycleResult struct {
	Detected  int
	Routed    int
	Recorded  int
	Unchanged int
	Skipped   int
}

// Custodian scans schema invariants, plans and routes their remediation and
// records each decision when it changes. seen holds the identities of the
// last completed scan (nil before the first), so an invariant that
// disappears and comes back is recorded again.
type Custodian struct {
	detector  Detector
	contracts ContractSource
	history   HistorySource
	router    RemediationRouter
	recorder  DecisionRecorder
	policy    Policy
	mu        sync.Mutex
	seen      map[string]bool
}

func NewCustodian(
	detector Detector, contracts ContractSource, history HistorySource,
	router RemediationRouter, recorder DecisionRecorder, policy Policy,
) *Custodian {
	return &Custodian{detector: detector, contracts: contracts, history: history,
		router: router, recorder: recorder, policy: policy}
}

// scanCycle is what one scan read in its batched lookups.
type scanCycle struct {
	contracts map[string]TableContract
	history   HistoryIndex
}

func (c *Custodian) Scan(ctx context.Context) (CycleResult, error) {
	if err := ctx.Err(); err != nil {
		return CycleResult{}, err
	}
	if err := c.validate(); err != nil {
		return CycleResult{}, err
	}
	invariants, err := c.detector.Detect(ctx)
	if err != nil {
		return CycleResult{}, fmt.Errorf("detect schema invariants: %w", err)
	}
	result := CycleResult{Detected: len(invariants)}
	cycle, err := c.load(ctx, invariants)
	if err != nil {
		return result, err
	}
	outcomes := make([]Remediation, 0, len(invariants))
	for _, invariant := range invariants {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if skipsIdle(invariant) {
			outcomes = append(outcomes, idleOutcome(invariant))
			result.Skipped++
			continue
		}
		item, err := c.process(ctx, invariant, cycle, &result)
		var failure *routeFailure
		if errors.As(err, &failure) {
			return result, c.recordRouteFailure(ctx, outcomes, cycle, failure, &result)
		}
		if err != nil {
			return result, err
		}
		outcomes = append(outcomes, item)
	}
	if err := c.recordChanged(ctx, outcomes, cycle, &result); err != nil {
		return result, err
	}
	c.markSeen(outcomes)
	return result, nil
}

func (c *Custodian) validate() error {
	if c == nil || c.detector == nil || c.contracts == nil || c.history == nil ||
		c.router == nil || c.recorder == nil {
		return fmt.Errorf("schema custodian dependencies are incomplete")
	}
	return nil
}

// load reads the contracts of the invariants that will be planned and the
// history of all of them, one lookup each.
func (c *Custodian) load(ctx context.Context, invariants []Invariant) (scanCycle, error) {
	var cycle scanCycle
	if len(invariants) == 0 {
		return cycle, nil
	}
	planned := make([]Invariant, 0, len(invariants))
	for _, invariant := range invariants {
		if !skipsIdle(invariant) {
			planned = append(planned, invariant)
		}
	}
	if len(planned) > 0 {
		contracts, err := c.contracts.Contracts(ctx, planned)
		if err != nil {
			return cycle, fmt.Errorf("read table contracts: %w", err)
		}
		cycle.contracts = contracts
	}
	history, err := c.history.History(ctx, invariants)
	if err != nil {
		return cycle, fmt.Errorf("read schema remediation history: %w", err)
	}
	cycle.history = history
	return cycle, nil
}

// routeFailure is a route that failed (not parked): the scan stops.
type routeFailure struct {
	item Remediation
	err  error
}

func (f *routeFailure) Error() string { return f.err.Error() }

func (f *routeFailure) Unwrap() error { return f.err }

func (c *Custodian) process(
	ctx context.Context, invariant Invariant, cycle scanCycle, result *CycleResult,
) (Remediation, error) {
	contract := cycle.contracts[invariant.Target()]
	decision, err := Plan(ctx, Request{
		Invariant: invariant, Contract: contract, Policy: c.policy,
		History: cycle.history.For(invariant),
	})
	if err != nil {
		return Remediation{}, err
	}
	item := Remediation{Invariant: invariant, Contract: contract, Decision: decision}
	if !shouldRoute(decision) {
		return item, nil
	}
	if err := c.router.Route(ctx, item); err != nil {
		var parked *ParkedRoute
		if !errors.As(err, &parked) {
			return item, &routeFailure{item: item, err: err}
		}
		item.Decision = parkedDecision(item.Decision, parked.Reason)
		return item, nil
	}
	result.Routed++
	return item, nil
}

// recordChanged records each identity whose decision differs from the one
// last recorded, or that was absent from the previous scan.
func (c *Custodian) recordChanged(
	ctx context.Context, outcomes []Remediation, cycle scanCycle, result *CycleResult,
) error {
	for _, group := range groupByIdentity(outcomes) {
		hash := decisionHash(group.items)
		if cycle.history.LastHash[group.identity] == hash && c.seenBefore(group.identity) {
			result.Unchanged++
			continue
		}
		for _, record := range group.records(hash) {
			if err := c.recorder.Record(ctx, record); err != nil {
				return fmt.Errorf("record schema remediation: %w", err)
			}
			result.Recorded++
		}
	}
	return nil
}

func (c *Custodian) seenBefore(identity string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen == nil || c.seen[identity]
}

func (c *Custodian) markSeen(outcomes []Remediation) {
	seen := make(map[string]bool, len(outcomes))
	for _, item := range outcomes {
		seen[InvariantIdentity(item.Invariant)] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = seen
}

// parkedDecision records a route that declined to act yet (not a failure):
// nothing was changed, so nothing may be auto-applied or deleted.
func parkedDecision(decision Decision, reason string) Decision {
	decision.Disposition = DispositionPark
	decision.AutoApply = false
	decision.MayDeleteData = false
	decision.Reason = reason
	return decision
}

// recordRouteFailure records the outcomes planned so far and the failed
// route, then returns the route error. A route can refuse verification
// after policy authorization was recorded: the unsuccessful outcome is
// preserved without implying no partial work occurred.
func (c *Custodian) recordRouteFailure(
	ctx context.Context, outcomes []Remediation, cycle scanCycle,
	failure *routeFailure, result *CycleResult,
) error {
	item := failure.item
	item.Decision.Disposition = DispositionPark
	item.Decision.AutoApply = false
	item.Decision.MayDeleteData = false
	item.Decision.Reason = "schema remediation routing failed; " +
		"review runtime error before retrying"
	err := fmt.Errorf("route schema remediation: %w", failure.err)
	if recordErr := c.recordChanged(ctx, outcomes, cycle, result); recordErr != nil {
		err = errors.Join(err, recordErr)
	}
	record := DecisionRecord{Remediation: item, Targets: []string{item.Invariant.Target()},
		Identity: InvariantIdentity(item.Invariant), Hash: decisionHash([]Remediation{item})}
	if recordErr := c.recorder.Record(ctx, record); recordErr != nil {
		return errors.Join(err, fmt.Errorf("record failed schema remediation: %w", recordErr))
	}
	result.Recorded++
	return err
}

func shouldRoute(decision Decision) bool {
	return decision.Disposition == DispositionApply ||
		decision.Disposition == DispositionDryRun ||
		(decision.Disposition == DispositionRecommend &&
			decision.Route == RouteCloneRehearsal)
}

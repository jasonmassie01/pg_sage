package schemaguard

import (
	"context"
	"errors"
	"fmt"
)

type Detector interface {
	Detect(context.Context) ([]Invariant, error)
}

type ContractSource interface {
	Contract(context.Context, Invariant) (TableContract, error)
}

type HistorySource interface {
	History(context.Context, Invariant) (History, error)
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
	Record(context.Context, Remediation) error
}

type CycleResult struct {
	Detected int
	Routed   int
	Recorded int
}

type Custodian struct {
	detector  Detector
	contracts ContractSource
	history   HistorySource
	router    RemediationRouter
	recorder  DecisionRecorder
	policy    Policy
}

func NewCustodian(
	detector Detector, contracts ContractSource, history HistorySource,
	router RemediationRouter, recorder DecisionRecorder, policy Policy,
) *Custodian {
	return &Custodian{detector, contracts, history, router, recorder, policy}
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
	for _, invariant := range invariants {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := c.process(ctx, invariant, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (c *Custodian) validate() error {
	if c == nil || c.detector == nil || c.contracts == nil || c.history == nil ||
		c.router == nil || c.recorder == nil {
		return fmt.Errorf("schema custodian dependencies are incomplete")
	}
	return nil
}

func (c *Custodian) process(
	ctx context.Context, invariant Invariant, result *CycleResult,
) error {
	contract, err := c.contracts.Contract(ctx, invariant)
	if err != nil {
		return fmt.Errorf("read contract for %s.%s: %w", invariant.Schema, invariant.Table, err)
	}
	history, err := c.history.History(ctx, invariant)
	if err != nil {
		return fmt.Errorf("read history for %s.%s: %w", invariant.Schema, invariant.Table, err)
	}
	decision, err := Plan(ctx, Request{
		Invariant: invariant, Contract: contract, Policy: c.policy, History: history,
	})
	if err != nil {
		return err
	}
	item := Remediation{Invariant: invariant, Contract: contract, Decision: decision}
	if shouldRoute(decision) {
		if err := c.router.Route(ctx, item); err != nil {
			return c.recordRouteFailure(ctx, item, result, err)
		}
		result.Routed++
	}
	if err := c.recorder.Record(ctx, item); err != nil {
		return fmt.Errorf("record schema remediation: %w", err)
	}
	result.Recorded++
	return nil
}

func (c *Custodian) recordRouteFailure(
	ctx context.Context, item Remediation, result *CycleResult, routeErr error,
) error {
	// A route can refuse verification after policy authorization was recorded.
	// Preserve that unsuccessful outcome without implying no partial work occurred.
	item.Decision.Disposition = DispositionPark
	item.Decision.AutoApply = false
	item.Decision.MayDeleteData = false
	item.Decision.Reason = "schema remediation routing failed; review runtime error before retrying"
	err := fmt.Errorf("route schema remediation: %w", routeErr)
	if recordErr := c.recorder.Record(ctx, item); recordErr != nil {
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

package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

const lakebaseHistoryConsumerName = "normalized_history_v1"

// lakebaseHistoryConsumer stores provider plans in the monitored database's
// explain cache. The other streams have no sink: they stay durable history in
// the inbox, are acknowledged when stored, and are only counted here.
type lakebaseHistoryConsumer struct {
	pool *pgxpool.Pool
	st   *lakebaseTelemetryState
}

func newLakebaseHistoryConsumer(
	pool *pgxpool.Pool, st *lakebaseTelemetryState,
) *lakebaseHistoryConsumer {
	return &lakebaseHistoryConsumer{pool: pool, st: st}
}

func (c *lakebaseHistoryConsumer) Name() string { return lakebaseHistoryConsumerName }

func (c *lakebaseHistoryConsumer) Streams() []lakebaseobs.Stream {
	return []lakebaseobs.Stream{lakebaseobs.Plans}
}

// Deliver returns only transient errors; the runtime retries those up to
// lakebaseobs.MaxDeliveryAttempts. Rows that can never be stored are counted.
func (c *lakebaseHistoryConsumer) Deliver(ctx context.Context, e lakebaseobs.Event) error {
	var err error
	if e.Stream == lakebaseobs.Plans {
		err = c.storePlan(ctx, e)
	} else {
		_, err = normalizeLakebaseHistory(e)
	}
	outcome, err := lakebaseHistoryOutcome(err)
	if err != nil {
		return err
	}
	c.st.outcome(e.Stream, outcome)
	return nil
}

func (c *lakebaseHistoryConsumer) storePlan(ctx context.Context, e lakebaseobs.Event) error {
	p, err := lakebaseobs.NormalizePlan(e)
	if err != nil {
		return err
	}
	return autoexplain.StoreProviderPlan(ctx, c.pool, autoexplain.ProviderPlan{
		ObservedPlan: autoexplain.ObservedPlan{QueryID: p.QueryID, Query: p.Query, JSON: p.JSON,
			TotalCost: p.TotalCost, ExecutionMS: p.ExecutionMS, CapturedAt: p.CapturedAt},
		ScopeHash: e.ScopeHash, ComputeID: e.ComputeID, EventID: e.ID, ReceivedAt: e.ReceivedAt,
		AnalyzeEnabled: p.AnalyzeEnabled, BuffersEnabled: p.BuffersEnabled,
		TimingEnabled: p.TimingEnabled, Fingerprint: p.Fingerprint,
		DurationSource: p.DurationSource})
}

// Observe counts a stream acknowledged at commit. Normalization has no
// transient failure, so any unclassified error is a rejected row.
func (c *lakebaseHistoryConsumer) Observe(e lakebaseobs.Event) {
	_, err := normalizeLakebaseHistory(e)
	outcome, err := lakebaseHistoryOutcome(err)
	if err != nil {
		outcome = "rejected_row"
	}
	c.st.outcome(e.Stream, outcome)
}

func (c *lakebaseHistoryConsumer) DeliveryFailed(e lakebaseobs.Event) {
	c.st.outcome(e.Stream, "delivery_failed")
}

// lakebaseHistoryOutcome classifies a delivery result. These rows remain
// durable historical evidence; unavailable payloads are classified, never
// fabricated as healthy/no-error observations. Context errors, SQLSTATE
// classes 08, 57 and 40, and every unclassified error stay transient.
func lakebaseHistoryOutcome(err error) (string, error) {
	switch {
	case err == nil:
		return "normalized", nil
	case errors.Is(err, lakebaseobs.ErrMasked):
		return "payload_unavailable", nil
	case errors.Is(err, lakebaseobs.ErrMalformed), errors.Is(err, lakebaseobs.ErrBudget),
		errors.Is(err, autoexplain.ErrInvalidProviderPlan):
		return "rejected_row", nil
	case errors.Is(err, lakebaseobs.ErrSelfStatement),
		errors.Is(err, autoexplain.ErrSelfStatement):
		return "self_excluded", nil
	default:
		return "", err
	}
}

func normalizeLakebaseHistory(e lakebaseobs.Event) (any, error) {
	switch e.Stream {
	case lakebaseobs.Plans:
		return lakebaseobs.NormalizePlan(e)
	case lakebaseobs.Logs:
		return lakebaseobs.NormalizeLog(e)
	case lakebaseobs.ASH:
		return lakebaseobs.NormalizeASH(e)
	case lakebaseobs.DDL:
		return lakebaseobs.NormalizeDDL(e)
	default:
		return lakebaseobs.NormalizeMetrics(e)
	}
}

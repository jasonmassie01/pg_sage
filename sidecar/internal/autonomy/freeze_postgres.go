package autonomy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

type PostgresFreezeCustodian struct {
	pool      *pgxpool.Pool
	database  string
	threshold freeze.Thresholds
	// sampler is the database's one horizon reader and rate state, shared
	// with the runway advisor and the analyzer (Sampler).
	sampler *FreezeSampler
	// bloatMinBytes is the least heap size for a bloat plan;
	// confirmBounds bound each pgstattuple confirmation, which confirm
	// runs (pgstattupleApprox unless a test replaces it).
	bloatMinBytes int64
	confirmBounds catalogread.Timeouts
	confirm       bloatConfirmer
	// detail reads a pinning session's own state before it is signalled
	// (readSessionDetail unless a test replaces it).
	detail sessionDetailReader
	// capFailures counts a red table's capped custodian VACUUMs
	// (readVacuumCapFailures unless a test replaces it); logFn takes the
	// custodian's warnings (WithLogger).
	capFailures capFailureReader
	logFn       func(level, format string, args ...any)
}

// NewPostgresFreezeCustodian builds the custodian with the shipped bloat
// floor (1 GiB) and the default catalog read bounds; WithBloatBounds
// applies the configured ones.
func NewPostgresFreezeCustodian(
	pool *pgxpool.Pool, database string, redBufferPct float64,
) *PostgresFreezeCustodian {
	var source txBeginner // a nil pool stays a nil interface: Read then errs
	if pool != nil {
		source = pool
	}
	c := &PostgresFreezeCustodian{
		pool: pool, database: database,
		threshold:     freeze.BufferThresholds(redBufferPct),
		bloatMinBytes: config.DefaultTableBloatMinBytes,
		confirmBounds: catalogread.Default(),
		sampler:       NewFreezeSampler(source, config.DefaultTableBloatMinBytes, 0),
	}
	c.confirm = c.pgstattupleApprox
	c.detail = c.readSessionDetail
	c.capFailures = c.readVacuumCapFailures
	c.logFn = stdLogf
	return c
}

// Sampler is the custodian's horizon reader, for the runway advisor and
// the analyzer of the same database.
func (c *PostgresFreezeCustodian) Sampler() *FreezeSampler { return c.sampler }

// WithSampleTTL lets readers within ttl of a reading share it.
func (c *PostgresFreezeCustodian) WithSampleTTL(ttl time.Duration) *PostgresFreezeCustodian {
	c.sampler.ttl = max(ttl, 0)
	return c
}

func (c *PostgresFreezeCustodian) Scan(ctx context.Context) ([]Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reading, err := c.sampler.Read(ctx)
	if err != nil {
		return nil, err
	}
	ext, err := c.extensions(ctx)
	if err != nil {
		return nil, err
	}
	horizons := c.freezeRows(reading.Horizons)
	c.confirmBloat(ctx, horizons, ext.pgstattuple)
	return c.plan(ctx, horizons, reading.rates, reading.Holders, ext.pgRepack)
}

// readAges reads the xmin holders and the table horizons of one sampler
// reading. age() measures from a reference XID that stays fixed for a
// transaction, and the sampler reads both in one: XIDs the cluster
// consumes between the two reads cannot make a holder look younger than
// the tables it pins (and a futile freeze look useful).
func (c *PostgresFreezeCustodian) readAges(
	ctx context.Context,
) ([]freeze.XminBlocker, []freezeRow, error) {
	reading, err := c.sampler.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	return reading.Holders, c.freezeRows(reading.Horizons), nil
}

// freezeRows are the horizons the custodian answers: pg_sage's own schema
// is left to autovacuum, as before the shared reader.
func (c *PostgresFreezeCustodian) freezeRows(horizons []probes.FreezeHorizon) []freezeRow {
	rows := make([]freezeRow, 0, len(horizons))
	for _, h := range horizons {
		if h.Schema == "sage" {
			continue
		}
		rows = append(rows, newFreezeRow(h))
	}
	return rows
}

// respond answers one table's horizon: the freeze ladder, then a bloat
// plan from the bloat estimate (never from the dead-tuple ratio). A red
// table also returns the holder that pins it, if any, and its capped
// VACUUMs pick the freeze's profile.
func (c *PostgresFreezeCustodian) respond(
	ctx context.Context, row freezeRow, rates freezeRateSample,
	holders []freeze.XminBlocker, repack bool,
) (Proposal, *freeze.XminBlocker, error) {
	sample := row.sample
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rates.xid, rates.mxid
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return Proposal{}, nil, fmt.Errorf("evaluate freeze response: %w", err)
	}
	input := freeze.ResponseInput{Schema: sample.Schema, Table: sample.Table,
		Urgency: assessment.Urgency, XIDAge: sample.XIDAge, MultiXactAge: sample.MultiXactAge,
		Aggressive: sample.Aggressive, DeadTupleRatio: row.deadRatio,
		BloatRatio: row.bloat, BloatKnown: row.bloatKnown, BloatSource: row.bloatSource,
		SizeBytes: row.heapBytes, MinBloatBytes: c.bloatMinBytes,
		PGRepackAvailable: repack}
	if assessment.Urgency == freeze.UrgencyRed {
		input.Holder = freeze.PinningCandidate(holders, row.userCatalog)
		input.CapFailures = c.capFailureCount(ctx, sample.Schema, sample.Table)
	}
	response, err := freeze.PlanResponse(input)
	if err != nil {
		return Proposal{}, nil, err
	}
	if row.confirmError != "" && response.Evidence != nil {
		response.Evidence["bloat_confirm_error"] = row.confirmError
	}
	proposal := freezeResponseProposal(c.database, sample, assessment.Proposal, response)
	if !rates.known {
		// Without two samples the consumption rate (and so the hard deadline)
		// is unknown: act on urgency, but claim no deadline override.
		proposal.Deadline = nil
	}
	if proposal.Deadline != nil {
		deadlineEvidence(proposal.Evidence, assessment.Deadline, rates.failsafeAge)
	}
	return proposal, response.PinnedBy, nil
}

// deadlineEvidence records which counter set the policy deadline (its
// Kind stays xid, the key of deadline_overrides) and the server's
// vacuum_failsafe_age when it is known (PG14+).
func deadlineEvidence(evidence map[string]any, deadline freeze.Deadline, failsafeAge int64) {
	evidence["deadline_threat"] = string(deadline.Threat)
	if failsafeAge > 0 {
		evidence["vacuum_failsafe_age"] = failsafeAge
	}
}

func freezeResponseProposal(
	database string, sample freeze.HorizonSample,
	deadline freeze.Proposal, response freeze.Response,
) Proposal {
	feature := "freeze"
	switch response.Kind {
	case freeze.ResponsePlanRepack, freeze.ResponseParkBloat:
		feature = "bloat_remediation"
	}
	evidence := response.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	if response.Plan != "" {
		evidence["plan"] = response.Plan
	}
	return Proposal{Database: database, Feature: feature, SQL: response.SQL,
		Plan: response.Plan, ResponseKind: string(response.Kind), Evidence: evidence,
		TargetObjects: []string{sample.Schema + "." + sample.Table},
		Deadline:      freezePolicyDeadline(deadline)}
}

// freezeExtensions are the optional extensions the custodian uses.
type freezeExtensions struct{ pgRepack, pgstattuple bool }

func (c *PostgresFreezeCustodian) extensions(ctx context.Context) (freezeExtensions, error) {
	var ext freezeExtensions
	err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		EXISTS(SELECT 1 FROM pg_extension WHERE extname='pg_repack'),
		EXISTS(SELECT 1 FROM pg_extension WHERE extname='pgstattuple')`).
		Scan(&ext.pgRepack, &ext.pgstattuple)
	if err != nil {
		return freezeExtensions{}, fmt.Errorf("read custodian extensions: %w", err)
	}
	return ext, nil
}

func freezePolicyDeadline(proposal freeze.Proposal) *policy.DeadlineContext {
	if proposal.Urgency != freeze.UrgencyRed {
		return nil
	}
	return &policy.DeadlineContext{
		Kind: policy.DeadlineXID, Urgency: policy.UrgencyCritical,
		HardAt: proposal.Deadline.HardAt,
	}
}

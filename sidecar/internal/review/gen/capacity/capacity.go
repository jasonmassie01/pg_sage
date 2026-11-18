// Package capacity is Sage Review's capacity, cost and sprawl generator
// (review-02f). It gives a review one capacity section:
//
//   - a growth plan: the database's and each listed table's daily sizes
//     (the pack's growth section, evid-01) fitted to a line and projected
//     30, 90 and 365 days ahead, each with its fit error and band (Project);
//   - right-size options from cost.EvaluateRightsize, with a monthly dollar
//     figure only when cost.rates prices the instance, otherwise "unknown:
//     no operator rates" (decision D4: pg_sage ships no price tables);
//   - storage headroom: the 90-day projection against the instance's
//     allocated storage, when cloud telemetry reports it;
//   - the idle-database rule (Idle): no workload calls, pg_sage's own
//     statements excluded, for IdleDays or more observed days.
//
// Every output is advice for a person. An instance class or storage
// change is an operator step at the provider and is never automatic at
// any trust level (spec section 3); nothing here executes, and the only
// write is the forecast log's own record (ForecastLog).
package capacity

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves.
const (
	Name      = "capacity"
	Dimension = "capacity_cost"
)

// Candidate kinds.
const (
	KindGrowthPlan      = "growth_plan"
	KindTableGrowth     = "table_growth"
	KindStorageHeadroom = "storage_headroom"
	KindRightSize       = "right_size"
	KindIdleDatabase    = "idle_database"
)

// Prediction quantities.
const (
	QuantityPoints         = "points"
	QuantitySlope          = "slope_bytes_per_day"
	QuantityFitRMSE        = "fit_rmse_bytes"
	QuantitySizeBytes      = "size_bytes"
	QuantityProjected30    = "projected_30d_bytes"
	QuantityProjected90    = "projected_90d_bytes"
	QuantityProjected365   = "projected_365d_bytes"
	QuantityBand30         = "band_30d_bytes"
	QuantityBand90         = "band_90d_bytes"
	QuantityBand365        = "band_365d_bytes"
	QuantityAllocatedBytes = "allocated_storage_bytes"
	QuantityAllocatedPct   = "projected_allocated_pct"
	QuantityCurrentVCPUs   = "current_vcpus"
	QuantityTargetVCPUs    = "target_vcpus"
	QuantityCPUP95         = "cpu_p95_pct"
	QuantityProjectedP95   = "projected_cpu_p95_pct"
	QuantityQuietDays      = "quiet_days"
	QuantityMonthlyDelta   = "monthly_delta"
	QuantityMonthlyCost    = "monthly_cost"
)

// NoRates is what stands in for every dollar figure without cost.rates.
const NoRates = "unknown: no operator rates"

// Coverage inputs the generator can miss.
const (
	MissingActivity  = "workload_calls"
	MissingRightsize = "rightsize"
)

// coverageEvidence is the pack's coverage fact, cited by the instance
// candidates (their usage is not a pack section).
const coverageEvidence = "coverage/window/all"

// Inputs is what Build judges besides the pack. Days nil means the
// activity was not read; Rightsizing nil with RightsizeErr nil means no
// cloud adapter; Rates nil means cost.rates does not price the database.
type Inputs struct {
	Days         []DayCalls
	Rightsizing  *Rightsizing
	RightsizeErr error
	Rates        *cost.Rates
}

// Config is the generator's readers, each optional: Calls reads the
// window's workload calls by day, Rightsizer judges the instance, Rates
// and Resource find the operator's rates, Log keeps the 30-day forecast.
type Config struct {
	Calls      CallSource
	Rightsizer Rightsizer
	Rates      cost.RateSource
	Resource   string
	Log        *ForecastLog
}

// Generator is the capacity generator.
type Generator struct{ cfg Config }

// New is a generator over cfg's readers.
func New(cfg Config) *Generator { return &Generator{cfg: cfg} }

// Name is "capacity".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate reads the inputs, builds the candidates and, with a log,
// settles the forecasts whose 30-day mark the pack reaches before it
// records this review's. A calls or log failure and a canceled context
// are errors; a right-size failure is carried as unknown.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if pack == nil {
		return nil, candidate.Coverage{}, errors.New("capacity generator: no evidence pack")
	}
	if err := ctx.Err(); err != nil {
		return nil, candidate.Coverage{}, fmt.Errorf("capacity generator for %s: %w",
			env.Database, err)
	}
	in, err := g.read(ctx, pack, env)
	if err != nil {
		return nil, candidate.Coverage{}, err
	}
	cands, cov, err := Build(pack, in, env)
	if err != nil {
		return nil, cov, err
	}
	if err := g.logForecast(ctx, pack, env, &cov); err != nil {
		return nil, cov, err
	}
	return cands, cov, nil
}

func (g *Generator) read(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) (Inputs, error) {
	var in Inputs
	if g.cfg.Calls != nil {
		days, err := g.cfg.Calls.Read(ctx, pack.Window.From, pack.Window.To)
		if err != nil {
			return Inputs{}, fmt.Errorf("capacity generator for %s: read the workload "+
				"calls: %w", env.Database, err)
		}
		if days == nil {
			days = []DayCalls{}
		}
		in.Days = days
	}
	if g.cfg.Rightsizer != nil {
		rs, err := g.cfg.Rightsizer.Rightsize(ctx)
		if err != nil {
			in.RightsizeErr = err
		} else {
			in.Rightsizing = &rs
		}
	}
	if g.cfg.Rates != nil {
		if r, ok := g.cfg.Rates.Rates(g.cfg.Resource, env.Database); ok {
			in.Rates = &r
		}
	}
	return in, nil
}

// Build judges in against the pack. Every candidate is validated.
func Build(pack *workpack.Pack, in Inputs, env candidate.Env) ([]candidate.Candidate,
	candidate.Coverage, error) {
	if err := checkInput(pack, env); err != nil {
		return nil, candidate.Coverage{}, err
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	g, err := readGrowth(pack)
	if err != nil {
		return nil, cov, fmt.Errorf("capacity generator for %s: %w", env.Database, err)
	}
	b := &builder{pack: pack, env: env, in: in, cov: &cov}
	b.growth(g)
	b.storage()
	b.rightsize()
	b.idle(g.DBSize)
	if err := candidate.ValidateAll(pack, b.out); err != nil {
		return nil, cov, fmt.Errorf("capacity generator for %s: %w", env.Database, err)
	}
	return b.out, cov, nil
}

func checkInput(pack *workpack.Pack, env candidate.Env) error {
	switch {
	case pack == nil:
		return errors.New("capacity generator: no evidence pack")
	case env.Database == "":
		return errors.New("capacity generator: no database named")
	case env.WindowDays < 1:
		return fmt.Errorf("capacity generator for %s: window of %d days", env.Database,
			env.WindowDays)
	}
	return nil
}

// builder collects the candidates of one Build.
type builder struct {
	pack *workpack.Pack
	env  candidate.Env
	in   Inputs
	cov  *candidate.Coverage
	db   *Projection
	out  []candidate.Candidate
}

// emit stamps the generator's fields and the id. Every candidate is
// advice and needs a person.
func (b *builder) emit(c candidate.Candidate) {
	c.Generator, c.Dimension = Name, Dimension
	c.ActionType, c.Approval = candidate.ActionAdvice, candidate.ApprovalRequired
	c.ID = candidate.NewID(Name, c.Kind, c.Target, c.IDBody())
	c.Prediction.Tier = candidate.TierNone
	b.out = append(b.out, c)
}

// databaseTarget names the database a candidate is about.
func (b *builder) databaseTarget() string { return "database:" + b.env.Database }

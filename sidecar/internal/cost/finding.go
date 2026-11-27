package cost

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// CategoryRightsize is the advisory right-size finding. It never carries
// SQL and no executor path acts on it.
const CategoryRightsize = "instance_rightsize"

// rightsizeMethod says how the dollar delta was estimated.
const rightsizeMethod = "configured compute price scaled linearly by vCPUs"

// RightsizeFinding is the finding for a downsize or upsize verdict, and
// the monthly delta in US dollars when the rates are in USD (nil
// otherwise: a figure in another currency is never written as dollars).
func RightsizeFinding(resource string, v Verdict) (analyzer.Finding, *float64, bool) {
	if v.Decision != DecisionDownsize && v.Decision != DecisionUpsize {
		return analyzer.Finding{}, nil, false
	}
	detail := map[string]any{"decision": v.Decision, "reason": v.Reason,
		"resource": resource, "current_class": v.CurrentClass,
		"target_class": v.TargetClass, "current_vcpus": v.CurrentVCPUs,
		"target_vcpus": v.TargetVCPUs, "days": v.Days, "coverage": v.Coverage,
		"cpu_p95": v.CPUP95, "cpu_max": v.CPUMax, "projected_cpu_p95": v.ProjectedCPUP95,
		"projected_cpu_max": v.ProjectedCPUMax, "advisory_only": true}
	if v.DBLoadP95 != nil {
		detail["db_load_p95"] = *v.DBLoadP95
	}
	var usd *float64
	if v.MonthlyDelta != nil {
		detail["monthly_delta"], detail["currency"] = *v.MonthlyDelta, v.Currency
		detail["rate_source"] = v.RateSource
		detail["as_of"] = v.AsOf.Format(time.DateOnly)
		detail["method"] = rightsizeMethod
		if v.Currency == "USD" {
			d := *v.MonthlyDelta
			usd = &d
		}
	}
	f := analyzer.Finding{Category: CategoryRightsize, Severity: "info",
		ObjectType: "instance", ObjectIdentifier: "instance:" + resource,
		Title: fmt.Sprintf("%s could %s to %s", v.CurrentClass, v.Decision,
			v.TargetClass),
		Detail: detail,
		Recommendation: fmt.Sprintf("%s. Resizing restarts or fails over the instance: "+
			"check that %s is offered in the region, then resize it yourself in a "+
			"maintenance window. pg_sage never resizes an instance.", v.Reason,
			v.TargetClass)}
	return f, usd, true
}

const setRightsizeCostSQL = `/* pg_sage */ UPDATE sage.findings SET estimated_cost_usd = $1
WHERE category = $2 AND object_identifier = $3 AND status = 'open'`

// ApplyRightsize records a verdict in pool's findings: a move opens or
// refreshes the finding (with its dollar delta), keep resolves it, and
// unknown leaves open findings as they are.
func ApplyRightsize(ctx context.Context, pool *pgxpool.Pool, resource string,
	v Verdict) error {
	if v.Decision == DecisionUnknown {
		return nil
	}
	active := map[string]bool{}
	if f, usd, ok := RightsizeFinding(resource, v); ok {
		if err := analyzer.UpsertFindings(ctx, pool, []analyzer.Finding{f}); err != nil {
			return fmt.Errorf("upsert %s finding for %s: %w", CategoryRightsize, resource, err)
		}
		if _, err := pool.Exec(ctx, setRightsizeCostSQL, usd, CategoryRightsize,
			f.ObjectIdentifier); err != nil {
			return fmt.Errorf("record %s dollar delta for %s: %w", CategoryRightsize,
				resource, err)
		}
		active[f.ObjectIdentifier] = true
	}
	if err := analyzer.ResolveCleared(ctx, pool, active, CategoryRightsize); err != nil {
		return fmt.Errorf("resolve %s findings for %s: %w", CategoryRightsize, resource, err)
	}
	return nil
}

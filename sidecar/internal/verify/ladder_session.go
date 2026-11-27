package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/apppath"
)

// SessionSetting is a reloadable setting step to test on plans: the
// setting, its new value and the target statements.
type SessionSetting struct {
	Name    string
	Value   string
	Queries []string
}

// TxBeginner opens a transaction (a connection or a pool).
type TxBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// settingName is a GUC name: an identifier, or a dotted extension name.
var settingName = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$`)

// placeholder is a bind parameter ($1) of a normalized statement.
var placeholder = regexp.MustCompile(`\$[0-9]+`)

// maxSessionQueries bounds the statements one test plans.
const maxSessionQueries = 20

// sessionCostTolerancePct is how far the planned cost may rise before the
// setting fails its tier-1 test.
const sessionCostTolerancePct = 0.5

// FromSessionSetting is the tier-1 record of a reloadable setting: each
// target read is planned (EXPLAIN, never run) without and then with the
// setting under set_config(..., is_local) in a read-only transaction on
// the application's search_path that is always rolled back, so nothing
// outlives the test. A statement that is not a read, or that needs a
// generic plan before PG16, is not planned and makes the test
// inconclusive; any error is a skipped tier.
func FromSessionSetting(ctx context.Context, db TxBeginner, s SessionSetting,
	at time.Time) LadderRecord {
	if err := checkSessionSetting(s); err != nil {
		return skippedRecord(TierPlanOnly, MethodLadderSession, err.Error(), at)
	}
	if db == nil {
		return skippedRecord(TierPlanOnly, MethodLadderSession,
			"no database connection to plan the targets on", at)
	}
	res, err := planUnderSetting(ctx, db, s)
	if err != nil {
		return skippedRecord(TierPlanOnly, MethodLadderSession, err.Error(), at)
	}
	return sessionRecord(s, res, at)
}

func checkSessionSetting(s SessionSetting) error {
	switch {
	case !settingName.MatchString(s.Name):
		return fmt.Errorf("setting name %q is not a configuration parameter name", s.Name)
	case strings.TrimSpace(s.Value) == "":
		return fmt.Errorf("setting %s has no value to test", s.Name)
	case len(s.Queries) == 0:
		return fmt.Errorf("setting %s has no target statement to plan", s.Name)
	}
	return nil
}

// sessionPlans is what one test planned.
type sessionPlans struct {
	before, after float64
	planned       int
	notPlanned    []string
	pgVersion     int
}

func planUnderSetting(ctx context.Context, db TxBeginner, s SessionSetting) (
	sessionPlans, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return sessionPlans{}, fmt.Errorf("begin the plan-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var res sessionPlans
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT
		set_config('statement_timeout', '5s', true),
		set_config('lock_timeout', '1s', true),
		current_setting('server_version_num')::int`).
		Scan(new(string), new(string), &res.pgVersion); err != nil {
		return sessionPlans{}, fmt.Errorf("bound the plan-only transaction: %w", err)
	}
	// The targets are the application's statements: plan them on its
	// search_path, not the observation session's (security-01c-n).
	if err := apppath.Pin(ctx, tx); err != nil {
		return sessionPlans{}, err
	}
	queries := eligibleQueries(s.Queries, res.pgVersion, &res.notPlanned)
	before, err := planCosts(ctx, tx, queries, res.pgVersion)
	if err != nil {
		return sessionPlans{}, err
	}
	if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT set_config($1, $2, true)`,
		s.Name, s.Value); err != nil {
		return sessionPlans{}, fmt.Errorf("SET LOCAL %s = %s: %w", s.Name, s.Value, err)
	}
	after, err := planCosts(ctx, tx, queries, res.pgVersion)
	if err != nil {
		return sessionPlans{}, err
	}
	for i := range queries {
		res.before += before[i]
		res.after += after[i]
	}
	res.planned = len(queries)
	return res, nil
}

// eligibleQueries keeps the reads that can be planned on this server and
// names the rest in notPlanned.
func eligibleQueries(queries []string, pgVersion int, notPlanned *[]string) []string {
	var out []string
	for i, q := range queries {
		q = strings.TrimSpace(q)
		switch {
		case i >= maxSessionQueries:
			*notPlanned = append(*notPlanned, fmt.Sprintf("statement %d: over the "+
				"limit of %d planned statements", i+1, maxSessionQueries))
		case !isRead(q):
			*notPlanned = append(*notPlanned, fmt.Sprintf("statement %d is not a read; "+
				"only reads are planned", i+1))
		case placeholder.MatchString(q) && pgVersion < 160000:
			*notPlanned = append(*notPlanned, fmt.Sprintf("statement %d has parameters "+
				"and generic plans need PostgreSQL 16", i+1))
		default:
			out = append(out, q)
		}
	}
	return out
}

func isRead(q string) bool {
	upper := strings.ToUpper(q)
	for _, p := range []string{"SELECT ", "SELECT\n", "WITH ", "VALUES ", "TABLE "} {
		if strings.HasPrefix(upper, p) {
			return !strings.Contains(q, ";")
		}
	}
	return false
}

// planCosts is each statement's planned total cost (EXPLAIN without
// ANALYZE: the statement is planned, never executed).
func planCosts(ctx context.Context, tx pgx.Tx, queries []string, pgVersion int) (
	[]float64, error) {
	out := make([]float64, len(queries))
	for i, q := range queries {
		opts := "FORMAT JSON"
		if placeholder.MatchString(q) && pgVersion >= 160000 {
			opts += ", GENERIC_PLAN"
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "EXPLAIN ("+opts+") "+q).Scan(&raw); err != nil {
			return nil, fmt.Errorf("plan statement %d: %w", i+1, err)
		}
		cost, err := totalCost(raw)
		if err != nil {
			return nil, fmt.Errorf("read the plan of statement %d: %w", i+1, err)
		}
		out[i] = cost
	}
	return out, nil
}

func totalCost(raw []byte) (float64, error) {
	var plans []struct {
		Plan struct {
			TotalCost *float64 `json:"Total Cost"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		return 0, err
	}
	if len(plans) == 0 || plans[0].Plan.TotalCost == nil {
		return 0, errors.New("EXPLAIN gave no total cost")
	}
	return *plans[0].Plan.TotalCost, nil
}

func sessionRecord(s SessionSetting, res sessionPlans, at time.Time) LadderRecord {
	if res.planned == 0 {
		why := "no target statement could be planned"
		if len(res.notPlanned) > 0 {
			why += ": " + strings.Join(res.notPlanned, "; ")
		}
		return skippedRecord(TierPlanOnly, MethodLadderSession, why, at)
	}
	r := newRecord(TierPlanOnly, MethodLadderSession, LadderPassed, at)
	r.Metric = LadderMetricPlanCost
	r.Evidence["setting"] = s.Name + "=" + s.Value
	r.Evidence["planned"] = res.planned
	r.Evidence["pg_version"] = res.pgVersion
	if res.before > 0 {
		pct := math.Round((res.after-res.before)/res.before*1000) / 10
		r.Predicted = &pct
		if pct > sessionCostTolerancePct {
			r.Result = LadderFailed
		}
	}
	if len(res.notPlanned) > 0 {
		return inconclusive(r, strings.Join(res.notPlanned, "; "))
	}
	return r
}

package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SREAutonomyConfig configures Sage SRE earned autonomy (M7): a level per
// incident family x action class, promoted only from benchmark, shadow
// and live evidence with an admin's approval, and downgraded on error
// budget burn, failover, stale evidence, concurrent actions or a safety
// regression. The promotion bar (Promotion) defaults to the spec's.
type SREAutonomyConfig struct {
	Enforce                  bool               `yaml:"enforce" doc:"Earned-autonomy ledger restricts self-initiated incident-family actions, custodians included (L1 script, L2 approval, L3 auto). false: the trust ramp decides. Default: true." warning:"false lets incident-family actions run under the elapsed-time trust ramp without earned evidence."`
	BenchResultsPath         string             `yaml:"bench_results_path" doc:"PGIncidentBench report, or a directory searched 3 levels deep, ingested at startup and hourly. A <report>.sigstore.json bundle is verified; reports for another build are refused."`
	EvaluateIntervalMinutes  int                `yaml:"evaluate_interval_minutes" doc:"Minutes between promotion evaluations (pg_sage proposes, an admin approves), 5-1440. Default: 60."`
	ReconcileIntervalSeconds int                `yaml:"reconcile_interval_seconds" doc:"Seconds between recording live outcomes of handed-off and autonomous actions, 10-3600. Default: 60."`
	MaxEvidenceAgeSeconds    int                `yaml:"max_evidence_age_seconds" doc:"Evidence older than this caps an action at L1, 5-3600. Default: 300."`
	ConcurrencyWindowMinutes int                `yaml:"concurrency_window_minutes" doc:"Another pg_sage action on the same object this recent caps an action at L1, 1-1440. Default: 15."`
	SafetyWindowDays         int                `yaml:"safety_window_days" doc:"Days a harmful or unsafe outcome caps its family at L1 and blocks re-promotion, 1-365. Default: 30."`
	FailoverCooldownMinutes  int                `yaml:"failover_cooldown_minutes" doc:"Minutes after a role change autonomy stays at L1, 1-1440. Default: 30."`
	ProposalTTLHours         int                `yaml:"proposal_ttl_hours" doc:"Hours a promotion proposal waits for an admin, 1-720. Default: 168."`
	ReportRetentionDays      int                `yaml:"report_retention_days" doc:"Days a stored bench or game-day report is kept, 30-3650. Ledger and pending-promotion evidence and the newest report per family are always kept. Default: 90."`
	Promotion                SREPromotionConfig `yaml:"promotion"`
	GameDays                 SREGameDaysConfig  `yaml:"game_days"`
	Canary                   SRECanaryConfig    `yaml:"canary"`
}

// SREGameDaysConfig configures game days: PGIncidentBench fault programs
// on a disposable clone (clone.provider) of the customer's database.
type SREGameDaysConfig struct {
	Enabled       bool     `yaml:"enabled" doc:"Run game days. Needs clone.provider (dle or snapshot) or local_dsn. Default: false."`
	IntervalHours int      `yaml:"interval_hours" doc:"Hours between scheduled game days, 24-2160. Default: 168."`
	LocalDSN      string   `yaml:"local_dsn" doc:"Fallback when clone.provider is none: a disposable PostgreSQL database for game days and local bench runs. Never a monitored or the metadata database (refused)." secret:"true"`
	Families      []string `yaml:"families" doc:"Incident families to exercise. Empty: every family the bench covers."`
}

// SRECanaryConfig configures the fleet canary for remediations.
type SRECanaryConfig struct {
	CanaryInstances    int     `yaml:"canary_instances" doc:"Databases changed and measured before the rollout widens, 1-10. Default: 1."`
	RegressionLimitPct float64 `yaml:"regression_limit_pct" doc:"Regression that halts and rolls back the rollout, 0-100. Default: 10."`
	SettleSeconds      int     `yaml:"settle_seconds" doc:"Seconds to wait after each change before measuring it, 0-3600. Default: 60."`
}

func defaultSREAutonomyConfig() SREAutonomyConfig {
	return SREAutonomyConfig{Enforce: true, EvaluateIntervalMinutes: 60,
		ReconcileIntervalSeconds: 60, MaxEvidenceAgeSeconds: 300,
		ConcurrencyWindowMinutes: 15, SafetyWindowDays: 30, FailoverCooldownMinutes: 30,
		ProposalTTLHours: 168, ReportRetentionDays: 90, Promotion: defaultSREPromotionConfig(),
		GameDays: SREGameDaysConfig{IntervalHours: 168},
		Canary:   SRECanaryConfig{CanaryInstances: 1, RegressionLimitPct: 10, SettleSeconds: 60}}
}

var familyNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type rangeCheck struct {
	key           string
	value, lo, hi int
}

func (a SREAutonomyConfig) validate() error {
	for _, c := range []rangeCheck{
		{"evaluate_interval_minutes", a.EvaluateIntervalMinutes, 5, 1440},
		{"reconcile_interval_seconds", a.ReconcileIntervalSeconds, 10, 3600},
		{"max_evidence_age_seconds", a.MaxEvidenceAgeSeconds, 5, 3600},
		{"concurrency_window_minutes", a.ConcurrencyWindowMinutes, 1, 1440},
		{"safety_window_days", a.SafetyWindowDays, 1, 365},
		{"failover_cooldown_minutes", a.FailoverCooldownMinutes, 1, 1440},
		{"proposal_ttl_hours", a.ProposalTTLHours, 1, 720},
		{"report_retention_days", a.ReportRetentionDays, 30, 3650},
		{"game_days.interval_hours", a.GameDays.IntervalHours, 24, 2160},
		{"canary.canary_instances", a.Canary.CanaryInstances, 1, 10},
		{"canary.settle_seconds", a.Canary.SettleSeconds, 0, 3600},
	} {
		if c.value < c.lo || c.value > c.hi {
			return fmt.Errorf("sre.autonomy.%s must be %d-%d, got %d", c.key, c.lo, c.hi,
				c.value)
		}
	}
	if p := a.Canary.RegressionLimitPct; p < 0 || p > 100 {
		return fmt.Errorf("sre.autonomy.canary.regression_limit_pct must be 0-100, got %v", p)
	}
	if err := a.Promotion.validate(); err != nil {
		return err
	}
	return a.GameDays.validate()
}

func (g SREGameDaysConfig) validate() error {
	for _, f := range g.Families {
		if !familyNamePattern.MatchString(f) {
			return fmt.Errorf("sre.autonomy.game_days.families: %q is not a family name", f)
		}
	}
	if strings.TrimSpace(g.LocalDSN) != "" {
		if _, err := pgx.ParseConfig(g.LocalDSN); err != nil {
			return fmt.Errorf("sre.autonomy.game_days.local_dsn is not a PostgreSQL DSN")
		}
	}
	return nil
}

// MaxEvidenceAge is the oldest action evidence that keeps L2/L3.
func (a SREAutonomyConfig) MaxEvidenceAge() time.Duration {
	return time.Duration(a.MaxEvidenceAgeSeconds) * time.Second
}

// ConcurrencyWindow is how recent a same-object action counts.
func (a SREAutonomyConfig) ConcurrencyWindow() time.Duration {
	return time.Duration(a.ConcurrencyWindowMinutes) * time.Minute
}

// SafetyWindow is how long a safety regression caps its family.
func (a SREAutonomyConfig) SafetyWindow() time.Duration {
	return time.Duration(a.SafetyWindowDays) * 24 * time.Hour
}

// FailoverCooldown is how long after a role change autonomy stays down.
func (a SREAutonomyConfig) FailoverCooldown() time.Duration {
	return time.Duration(a.FailoverCooldownMinutes) * time.Minute
}

// ProposalTTL is how long a promotion proposal waits.
func (a SREAutonomyConfig) ProposalTTL() time.Duration {
	return time.Duration(a.ProposalTTLHours) * time.Hour
}

// EvaluateInterval is the promotion evaluation period.
func (a SREAutonomyConfig) EvaluateInterval() time.Duration {
	return time.Duration(a.EvaluateIntervalMinutes) * time.Minute
}

// ReconcileInterval is the outcome reconciliation period.
func (a SREAutonomyConfig) ReconcileInterval() time.Duration {
	return time.Duration(a.ReconcileIntervalSeconds) * time.Second
}

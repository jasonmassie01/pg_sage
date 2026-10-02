// Package slo is Sage SRE's SLO layer (AI-SRE-SPEC §8, Codex §9): the
// canonical SLI is bad_events / eligible_events; registered app SLIs come
// from a Prometheus-compatible query connector or pushed counters, and
// database proxy SLIs (latency, error-class log rate, connection
// refusal, replication lag) are always labeled proxies. Burn rates are
// evaluated over multiple windows (Google SRE workbook defaults); low
// traffic, a zero denominator and absent data are unknown, never "ok".
// Only a registered app SLI earns a customer-impact claim. The error
// budget state per SLO is durable and published to the autonomy layer
// (ErrorBudgetSource), the investigator (slo_status evidence, burn
// triggers) and the recovery predicate.
package slo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind is who owns an SLI: a registered application SLI, or a database
// proxy.
type Kind string

// SLI kinds.
const (
	KindApp   Kind = "app"
	KindProxy Kind = "proxy"
)

// SourceKind is where an SLI's counters come from.
type SourceKind string

// SLI sources.
const (
	SourcePrometheus SourceKind = "prometheus"
	SourcePush       SourceKind = "push"
	SourceProxy      SourceKind = "proxy"
)

// Severity is what a burn-rate rule raises.
type Severity string

// Rule severities.
const (
	SeverityPage   Severity = "page"
	SeverityTicket Severity = "ticket"
)

// State is an SLO's error-budget state.
type State string

// SLO states. Unknown is never "ok".
const (
	StateOK      State = "ok"
	StateTicket  State = "ticket"
	StatePage    State = "page"
	StateUnknown State = "unknown"
)

// Reasons a window, an SLO or a recovery verdict is unknown.
const (
	ReasonNoData                 = "no_data"
	ReasonStale                  = "stale"
	ReasonLowTraffic             = "low_traffic"
	ReasonZeroEligible           = "zero_eligible"
	ReasonPartialWindow          = "partial_window"
	ReasonSourceError            = "source_error"
	ReasonAmbiguous              = "ambiguous_series"
	ReasonInvalidValue           = "invalid_value"
	ReasonNotEvaluated           = "not_evaluated"
	ReasonEvaluationStale        = "evaluation_stale"
	ReasonConnectorNotConfigured = "connector_not_configured"
	ReasonCounterReset           = "counter_reset"
	ReasonTrafficDropped         = "traffic_dropped"
	ReasonInsufficientSamples    = "insufficient_samples"
	ReasonBurning                = "burning"
	// Proxy reasons: why a database proxy could not measure.
	ReasonNoReplicas        = "no_replicas"
	ReasonBaselineBuilding  = "baseline_building"
	ReasonNoNewCapture      = "no_new_capture"
	ReasonStatsReset        = "stats_reset"
	ReasonSourceUnavailable = "source_unavailable"
)

// ProxyPrefix starts every database proxy SLI name.
const ProxyPrefix = "db_"

// Bounds.
const (
	MinWindow         = time.Hour
	MaxWindow         = 90 * 24 * time.Hour
	DefaultStaleAfter = 5 * time.Minute
	// MinCoverage is the share of a window stored samples must cover.
	MinCoverage = 0.9
	maxTextRunes = 300
)

// ErrInvalidObjective reports an invalid SLO definition or rule.
var ErrInvalidObjective = errors.New("invalid SLO")

// Rule is one multi-window burn-rate alert: it fires when both windows
// burn at or above Factor.
type Rule struct {
	Severity Severity
	Long     time.Duration
	Short    time.Duration
	Factor   float64
}

// DefaultRules are the Google SRE workbook defaults.
func DefaultRules() []Rule {
	return []Rule{{SeverityPage, time.Hour, 5 * time.Minute, 14.4},
		{SeverityPage, 6 * time.Hour, 30 * time.Minute, 6},
		{SeverityTicket, 72 * time.Hour, 6 * time.Hour, 1}}
}

// ValidateRules checks a rule set.
func ValidateRules(rs []Rule) error {
	if len(rs) == 0 {
		return fmt.Errorf("%w: at least one burn-rate rule is required", ErrInvalidObjective)
	}
	for i, r := range rs {
		switch {
		case r.Severity != SeverityPage && r.Severity != SeverityTicket:
			return fmt.Errorf("%w: rule %d severity %q", ErrInvalidObjective, i+1, r.Severity)
		case !(r.Factor > 0) || math.IsInf(r.Factor, 0):
			return fmt.Errorf("%w: rule %d factor must be positive", ErrInvalidObjective, i+1)
		case r.Short <= 0 || r.Short >= r.Long:
			return fmt.Errorf("%w: rule %d short window must be positive and shorter "+
				"than the long one", ErrInvalidObjective, i+1)
		case r.Long > MaxWindow:
			return fmt.Errorf("%w: rule %d long window over 90d", ErrInvalidObjective, i+1)
		}
	}
	return nil
}

// Objective is one SLO definition.
type Objective struct {
	Name        string
	Database    string
	Description string
	Owner       string
	Kind        Kind
	Source      SourceKind
	Target      float64
	Window      time.Duration
	// Prometheus queries; $window is replaced by the evaluated window.
	BadQuery      string
	EligibleQuery string
	ResetsQuery   string
	// MinEligible is the least eligible events a window needs to be known.
	MinEligible float64
	// StaleAfter is how old the newest stored sample may be (zero: 5m).
	StaleAfter time.Duration
	// Proxy names the database proxy of a proxy SLI.
	Proxy string
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ValidName reports whether s is a well-formed SLO name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// Validate checks the definition.
func (o Objective) Validate() error {
	checks := []struct {
		ok      bool
		problem string
	}{
		{ValidName(o.Name), "name must match " + namePattern.String()},
		{o.Target > 0 && o.Target < 1, "target must be in (0, 1)"},
		{o.Window >= MinWindow && o.Window <= MaxWindow, "window must be 1h-90d"},
		{o.MinEligible >= 1 && !math.IsInf(o.MinEligible, 0), "min eligible must be >= 1"},
		{o.StaleAfter >= 0, "stale-after must not be negative"},
		{textOK(o.Description) && textOK(o.Owner), "description and owner must be " +
			"at most 300 characters without control characters"},
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%w: %s: %s", ErrInvalidObjective, o.Name, c.problem)
		}
	}
	return o.validateSource()
}

func (o Objective) validateSource() error {
	queries := o.BadQuery != "" || o.EligibleQuery != "" || o.ResetsQuery != ""
	var problem string
	switch {
	case o.Kind == KindApp && strings.HasPrefix(o.Name, ProxyPrefix):
		problem = "names starting with " + ProxyPrefix + " are reserved for database proxies"
	case o.Kind == KindApp && o.Source == SourcePrometheus:
		problem = promQueryProblem(o)
	case o.Kind == KindApp && o.Source == SourcePush:
		if queries {
			problem = "a pushed SLI takes no queries"
		}
	case o.Kind == KindProxy && o.Source == SourceProxy:
		if queries || o.Proxy == "" {
			problem = "a proxy SLI names its proxy and takes no queries"
		}
	default:
		problem = fmt.Sprintf("kind %q with source %q", o.Kind, o.Source)
	}
	if problem != "" {
		return fmt.Errorf("%w: %s: %s", ErrInvalidObjective, o.Name, problem)
	}
	return nil
}

func promQueryProblem(o Objective) string {
	for _, q := range []string{o.BadQuery, o.EligibleQuery} {
		if !strings.Contains(q, "$window") {
			return "bad and eligible queries must use $window"
		}
	}
	if o.ResetsQuery != "" && !strings.Contains(o.ResetsQuery, "$window") {
		return "the resets query must use $window"
	}
	return ""
}

func textOK(s string) bool {
	return utf8.RuneCountInString(s) <= maxTextRunes && utf8.ValidString(s) &&
		strings.IndexFunc(s, unicode.IsControl) < 0
}

func (o Objective) staleAfter() time.Duration {
	if o.StaleAfter <= 0 {
		return DefaultStaleAfter
	}
	return o.StaleAfter
}

// FormatWindow renders a duration as a Prometheus range ("5m", "1h", "3d").
func FormatWindow(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// Hash identifies the definition: a changed target, window, source or
// query is a new definition.
func (o Objective) Hash() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{o.Name, string(o.Kind),
		string(o.Source), strconv.FormatFloat(o.Target, 'g', -1, 64), o.Window.String(),
		o.BadQuery, o.EligibleQuery, o.ResetsQuery, o.Proxy,
		strconv.FormatFloat(o.MinEligible, 'g', -1, 64)}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

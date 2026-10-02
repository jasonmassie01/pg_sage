package config

import (
	"fmt"
	"time"
)

// SREConfig configures the Sage SRE investigator (M2-M4): read-only
// investigations of RCA incidents and plan regressions, started
// automatically by default (M4). Investigations run catalog probes and
// the deterministic causal graph, with a validated model turn when an
// LLM is configured; they never execute actions.
type SREConfig struct {
	AutomaticStart         bool `yaml:"automatic_start" doc:"Start read-only, bounded investigations from RCA incidents, plan_regression findings and the reactive detector (checkpoint, temp-file, LWLock). Never executes. false = on request only. Default: true."`
	TriggerIntervalSeconds int  `yaml:"trigger_interval_seconds" doc:"Seconds between checks for new triggers and pending investigations, 5-600. Default: 15."`
	SampleIntervalSeconds  int  `yaml:"sample_interval_seconds" doc:"Seconds between the two samples connection and WAL investigations compare, 1-30. Default: 5."`
	EvidenceRetentionDays  int  `yaml:"evidence_retention_days" doc:"Days a finished, unpinned investigation keeps its probe evidence (a tombstone records the delete). 1 to timeline_retention_days. Default: 30."`
	TimelineRetentionDays  int  `yaml:"timeline_retention_days" doc:"Days a finished, unpinned investigation is kept at all, leaving a tombstone. evidence_retention_days to 3650. Pinned and running ones are kept. Default: 90."`
	// LLM is the model turn (M3).
	LLM SRELLMConfig `yaml:"llm"`
	// SLO is SLO burn-rate alerting (M5); ChangeEvents the change feed.
	SLO          SRESLOConfig          `yaml:"slo"`
	ChangeEvents SREChangeEventsConfig `yaml:"change_events"`
	// Actions are approved, evidence-matched actions (M5).
	Actions SREActionsConfig `yaml:"actions"`
	// Runways are the pre-incident runways (M6).
	Runways RunwayConfig `yaml:"runways"`
	// Autonomy is earned autonomy (M7).
	Autonomy SREAutonomyConfig `yaml:"autonomy"`
	// Poolers are external poolers whose telemetry connection
	// investigations read (CHECK-04); empty = none.
	Poolers []SREPoolerConfig `yaml:"poolers" doc:"External connection poolers (PgBouncer admin consoles) whose pool telemetry connection investigations read, read-only. Empty = none."`
}

// SRELLMConfig configures the investigator's model turn: with an LLM
// configured, the model ranks the causal graph's hypotheses, may ask for
// one catalog probe while the graph is inconclusive, and narrates cited
// claims. Every reply is validated; the graph stays the authority and a
// rejected reply falls back to the deterministic result.
type SRELLMConfig struct {
	Enabled bool `yaml:"enabled" doc:"Model turn in investigations: rank the graph's hypotheses, propose one catalog probe, narrate cited claims. Used whenever an LLM is configured; false = deterministic only. Default: true."`
}

// Sage SRE defaults and bounds.
const (
	DefaultSRETriggerIntervalSeconds = 15
	DefaultSRESampleIntervalSeconds  = 5
	DefaultSREEvidenceRetentionDays  = 30
	DefaultSRETimelineRetentionDays  = 90
	maxSRERetentionDays              = 3650
)

func defaultSREConfig() SREConfig {
	return SREConfig{AutomaticStart: true,
		TriggerIntervalSeconds: DefaultSRETriggerIntervalSeconds,
		SampleIntervalSeconds:  DefaultSRESampleIntervalSeconds,
		EvidenceRetentionDays:  DefaultSREEvidenceRetentionDays,
		TimelineRetentionDays:  DefaultSRETimelineRetentionDays,
		LLM:                    SRELLMConfig{Enabled: true},
		SLO:                    defaultSRESLOConfig(),
		ChangeEvents:           defaultSREChangeEventsConfig(),
		Actions:                defaultSREActionsConfig(),
		Runways:                defaultRunwayConfig(),
		Autonomy:               defaultSREAutonomyConfig()}
}

// TriggerInterval is the coordinator poll period.
func (s SREConfig) TriggerInterval() time.Duration {
	return time.Duration(s.TriggerIntervalSeconds) * time.Second
}

// SampleInterval is the gap between compared samples.
func (s SREConfig) SampleInterval() time.Duration {
	return time.Duration(s.SampleIntervalSeconds) * time.Second
}

// EvidenceRetention is how long finished investigations keep evidence.
func (s SREConfig) EvidenceRetention() time.Duration {
	return time.Duration(s.EvidenceRetentionDays) * 24 * time.Hour
}

// TimelineRetention is how long finished investigations are kept.
func (s SREConfig) TimelineRetention() time.Duration {
	return time.Duration(s.TimelineRetentionDays) * 24 * time.Hour
}

func (s SREConfig) validate() error {
	checks := []struct {
		ok      bool
		problem string
	}{
		{s.TriggerIntervalSeconds >= 5 && s.TriggerIntervalSeconds <= 600,
			fmt.Sprintf("sre.trigger_interval_seconds must be 5-600, got %d",
				s.TriggerIntervalSeconds)},
		{s.SampleIntervalSeconds >= 1 && s.SampleIntervalSeconds <= 30,
			fmt.Sprintf("sre.sample_interval_seconds must be 1-30, got %d",
				s.SampleIntervalSeconds)},
		{s.EvidenceRetentionDays >= 1, fmt.Sprintf(
			"sre.evidence_retention_days must be at least 1, got %d",
			s.EvidenceRetentionDays)},
		{s.TimelineRetentionDays <= maxSRERetentionDays, fmt.Sprintf(
			"sre.timeline_retention_days must be at most %d, got %d",
			maxSRERetentionDays, s.TimelineRetentionDays)},
		{s.TimelineRetentionDays >= s.EvidenceRetentionDays, fmt.Sprintf(
			"sre.timeline_retention_days (%d) must be at least "+
				"sre.evidence_retention_days (%d)", s.TimelineRetentionDays,
			s.EvidenceRetentionDays)},
	}
	if err := s.Actions.validate(); err != nil {
		return err
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%s", c.problem)
		}
	}
	if err := s.SLO.validate(); err != nil {
		return err
	}
	if err := s.ChangeEvents.validate(); err != nil {
		return err
	}
	if err := s.Runways.validate(); err != nil {
		return err
	}
	if err := validatePoolers(s.Poolers); err != nil {
		return err
	}
	return s.Autonomy.validate()
}

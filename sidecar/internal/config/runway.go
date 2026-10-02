package config

import (
	"fmt"
	"time"
)

// RunwayConfig configures Sage SRE runways (M6): on the collector tick
// the runway monitor samples the XID and multixact counters, WAL
// position, retained WAL per slot, database and disk usage and sequences;
// when a measured trend projects a limit inside its horizon it opens a
// forecast finding and a read-only pre-incident investigation. Disk
// projections use forecaster.disk_capacity_bytes.
type RunwayConfig struct {
	Enabled                 bool `yaml:"enabled" doc:"Sample runway series on the collector tick and open forecast findings when a runway crosses its horizon (wraparound, disk/WAL, sequences). Read-only. Default: true."`
	Investigate             bool `yaml:"investigate" doc:"Open a read-only pre-incident investigation for each runway finding (one per finding and severity). Independent of sre.automatic_start. Default: true."`
	IntervalSeconds         int  `yaml:"interval_seconds" doc:"Seconds between runway samples, 15-3600. Default: 60 (the collector tick)."`
	LookbackHours           int  `yaml:"lookback_hours" doc:"Hours of samples a trend is measured over, 1 to sample_retention_hours (and at most 168). Default: 6."`
	MinSamples              int  `yaml:"min_samples" doc:"Fewest samples a trend needs before it projects a runway, 3-1000. Default: 10."`
	MinSpanMinutes          int  `yaml:"min_span_minutes" doc:"Shortest span (minutes) of samples a trend needs, 1 to the lookback. Default: 30."`
	WraparoundHorizonHours  int  `yaml:"wraparound_horizon_hours" doc:"Open a wraparound runway finding when the measured runway to the XID or multixact warning limit is this short (hours), up to 8760. Default: 336 (14 days)."`
	WraparoundCriticalHours int  `yaml:"wraparound_critical_hours" doc:"A wraparound runway this short (hours) is critical; at most wraparound_horizon_hours. Default: 72."`
	DiskHorizonHours        int  `yaml:"disk_horizon_hours" doc:"Open a disk/WAL runway finding when disk usage reaches the declared capacity, or a slot reaches its WAL maximum, this soon (hours), up to 8760. Default: 72."`
	DiskCriticalHours       int  `yaml:"disk_critical_hours" doc:"A disk/WAL runway this short (hours) is critical; at most disk_horizon_hours. Default: 24."`
	SequenceHorizonDays     int  `yaml:"sequence_horizon_days" doc:"Open a sequence runway finding when a sequence reaches its binding limit this soon (days), up to 3650. Default: 30."`
	SequenceCriticalDays    int  `yaml:"sequence_critical_days" doc:"A sequence runway this short (days) is critical; at most sequence_horizon_days. Default: 7."`
	SampleRetentionHours    int  `yaml:"sample_retention_hours" doc:"Hours runway samples are kept, lookback_hours to 720. Default: 48."`
}

func defaultRunwayConfig() RunwayConfig {
	return RunwayConfig{Enabled: true, Investigate: true, IntervalSeconds: 60,
		LookbackHours: 6, MinSamples: 10, MinSpanMinutes: 30,
		WraparoundHorizonHours: 336, WraparoundCriticalHours: 72,
		DiskHorizonHours: 72, DiskCriticalHours: 24,
		SequenceHorizonDays: 30, SequenceCriticalDays: 7, SampleRetentionHours: 48}
}

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

// Interval is the sampling period.
func (r RunwayConfig) Interval() time.Duration {
	return time.Duration(r.IntervalSeconds) * time.Second
}

// Lookback is the window trends are measured over.
func (r RunwayConfig) Lookback() time.Duration { return hours(r.LookbackHours) }

// MinSpan is the shortest span a trend needs.
func (r RunwayConfig) MinSpan() time.Duration {
	return time.Duration(r.MinSpanMinutes) * time.Minute
}

// WraparoundHorizon opens a wraparound runway finding.
func (r RunwayConfig) WraparoundHorizon() time.Duration { return hours(r.WraparoundHorizonHours) }

// WraparoundCritical makes a wraparound runway critical.
func (r RunwayConfig) WraparoundCritical() time.Duration {
	return hours(r.WraparoundCriticalHours)
}

// DiskHorizon opens a disk/WAL runway finding.
func (r RunwayConfig) DiskHorizon() time.Duration { return hours(r.DiskHorizonHours) }

// DiskCritical makes a disk/WAL runway critical.
func (r RunwayConfig) DiskCritical() time.Duration { return hours(r.DiskCriticalHours) }

// SequenceHorizon opens a sequence runway finding.
func (r RunwayConfig) SequenceHorizon() time.Duration { return hours(24 * r.SequenceHorizonDays) }

// SequenceCritical makes a sequence runway critical.
func (r RunwayConfig) SequenceCritical() time.Duration {
	return hours(24 * r.SequenceCriticalDays)
}

// SampleRetention is how long samples are kept.
func (r RunwayConfig) SampleRetention() time.Duration { return hours(r.SampleRetentionHours) }

func (r RunwayConfig) validate() error {
	checks := []struct {
		ok   bool
		key  string
		want string
		got  int
	}{
		{between(r.IntervalSeconds, 15, 3600), "interval_seconds", "15-3600", r.IntervalSeconds},
		{between(r.LookbackHours, 1, min(168, r.SampleRetentionHours)), "lookback_hours",
			"1 to sample_retention_hours (at most 168)", r.LookbackHours},
		{between(r.MinSamples, 3, 1000), "min_samples", "3-1000", r.MinSamples},
		{between(r.MinSpanMinutes, 1, 60*r.LookbackHours), "min_span_minutes",
			"1 to the lookback", r.MinSpanMinutes},
		{between(r.WraparoundHorizonHours, r.WraparoundCriticalHours, 8760),
			"wraparound_horizon_hours", "wraparound_critical_hours-8760",
			r.WraparoundHorizonHours},
		{r.WraparoundCriticalHours >= 1, "wraparound_critical_hours", "at least 1",
			r.WraparoundCriticalHours},
		{between(r.DiskHorizonHours, r.DiskCriticalHours, 8760), "disk_horizon_hours",
			"disk_critical_hours-8760", r.DiskHorizonHours},
		{r.DiskCriticalHours >= 1, "disk_critical_hours", "at least 1", r.DiskCriticalHours},
		{between(r.SequenceHorizonDays, r.SequenceCriticalDays, 3650),
			"sequence_horizon_days", "sequence_critical_days-3650", r.SequenceHorizonDays},
		{r.SequenceCriticalDays >= 1, "sequence_critical_days", "at least 1",
			r.SequenceCriticalDays},
		{between(r.SampleRetentionHours, r.LookbackHours, 720), "sample_retention_hours",
			"lookback_hours-720", r.SampleRetentionHours},
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("sre.runways.%s must be %s, got %d", c.key, c.want, c.got)
		}
	}
	return nil
}

func between(v, lo, hi int) bool { return v >= lo && v <= hi }

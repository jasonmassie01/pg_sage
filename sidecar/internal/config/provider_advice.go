package config

// ProviderAdviceConfig is the managed provider's own advisor
// recommendations reconciled with pg_sage's index findings (market-09):
// the Cloud SQL / AlloyDB index advisor view, Azure flexible server index
// tuning and Supabase performance advisors. Evidence only: advice never
// raises a finding's confidence, risk tier or autonomy.
type ProviderAdviceConfig struct {
	Enabled *bool `yaml:"enabled" doc:"Read the provider's own advisor advice (Cloud SQL/AlloyDB index advisor, Azure index tuning, Supabase advisors) hourly and show where pg_sage agrees. Evidence only. Nil = enabled."`
}

// IsEnabled reports whether provider advice is read (default: on).
func (c ProviderAdviceConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

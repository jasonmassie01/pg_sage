package cases

import "time"

// ProviderAdvice is the managed provider's own advice on a case's finding
// and pg_sage's verdict on it (market-09). It is evidence only: it never
// changes the case's severity, its candidates' risk or their autonomy.
type ProviderAdvice struct {
	Provider string    `json:"provider"`
	Kind     string    `json:"kind"`
	Verdict  string    `json:"verdict"`
	Reason   string    `json:"reason"`
	Object   string    `json:"object,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// AttachProviderAdvice sets c's provider advice block.
func AttachProviderAdvice(c *Case, advice []ProviderAdvice) {
	if c != nil {
		c.ProviderAdvice, c.ProviderAdviceUnavailable = advice, false
	}
}

// MarkProviderAdviceUnavailable records that c's provider advice could not
// be read, so a missing block is not read as "the provider agrees" or
// "the provider has no advice".
func MarkProviderAdviceUnavailable(c *Case) {
	if c != nil {
		c.ProviderAdvice, c.ProviderAdviceUnavailable = nil, true
	}
}

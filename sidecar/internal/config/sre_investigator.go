package config

import "fmt"

// sre.llm.mode (roadmap 2.1): how investigations use the model when an
// LLM is configured. "investigator" (the default) runs the tool-calling
// investigator: the model plans read-only catalog probes, pg_stat views
// and plan-only EXPLAIN within narrow or broad budgets, and concludes
// with cited evidence; its root cause stays advisory unless the family's
// root authority was earned on the held-out bench. "review" keeps the M3
// single review turn (rank the graph's hypotheses, one probe, cited
// claims), which spends fewer tokens.
const (
	SRELLMModeInvestigator = "investigator"
	SRELLMModeReview       = "review"
)

func (l SRELLMConfig) validate() error {
	switch l.Mode {
	case SRELLMModeInvestigator, SRELLMModeReview:
		return nil
	}
	return fmt.Errorf("sre.llm.mode must be %q or %q, got %q", SRELLMModeInvestigator,
		SRELLMModeReview, l.Mode)
}

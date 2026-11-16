package onboarding

import (
	"fmt"
	"time"
)

// StepModel is the checklist's model step (ux-04b): LLM features are on by
// default, so the first run asks for a model key. It is optional, since
// pg_sage runs without a model, and done only once the configured model
// answered its last call.
const StepModel = "model"

// ModelStatus is what the checklist knows about the model a database's
// LLM features use. Answered is true when its last call (a probe or real
// work) succeeded; LastErrorClass is the class of a failure since then.
type ModelStatus struct {
	Configured         bool
	ConfigurationError string
	Answered           bool
	LastErrorClass     string
}

// ModelClient is the part of an LLM client's status ModelStatusOf reads
// (llm.ClientStatus has these fields).
type ModelClient struct {
	Enabled            bool
	ConfigurationError string
	LastErrorClass     string
	LastErrorAt        *time.Time
	LastSuccessAt      *time.Time
}

// ModelStatusOf reads a client's status: it answered its last call when
// a success exists and no error came at or after it (fail closed on a tie).
func ModelStatusOf(c ModelClient) ModelStatus {
	if !c.Enabled {
		return ModelStatus{}
	}
	s := ModelStatus{Configured: true, ConfigurationError: c.ConfigurationError}
	if c.ConfigurationError != "" {
		return s
	}
	errorLast := c.LastErrorAt != nil &&
		(c.LastSuccessAt == nil || !c.LastSuccessAt.After(*c.LastErrorAt))
	switch {
	case errorLast:
		s.LastErrorClass = c.LastErrorClass
	case c.LastSuccessAt != nil:
		s.Answered = true
	}
	return s
}

func modelStep(in ChecklistInput) Step {
	s := Step{ID: StepModel, Label: "Model", Optional: true, Link: "#/settings"}
	m := in.Model
	switch {
	case m == nil:
		s.Detail = "Model status unknown."
	case !m.Configured:
		s.Detail = "No model is set: LLM features stay off until a model is set " +
			"(provider endpoint and key in Settings)."
	case m.ConfigurationError != "":
		s.Detail = "The model configuration is invalid: " + m.ConfigurationError
	case m.Answered:
		s.Done, s.Detail = true, "The model answered its last call."
	case m.LastErrorClass != "":
		s.Detail = fmt.Sprintf("The model's last call failed (%s); fix it and test "+
			"again in Settings.", m.LastErrorClass)
	default:
		s.Detail = "A model is set but has not answered yet; test it to finish this step."
	}
	return s
}

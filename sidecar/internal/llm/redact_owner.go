package llm

import (
	"context"

	"github.com/pg-sage/sidecar/internal/config"
)

// RedactionOwner is the config reconfiguration owner of
// llm.redact_numeric_literals: a live change flips the process-wide
// switch without touching any client, so no request is cancelled.
type RedactionOwner struct{}

func (RedactionOwner) Name() string { return config.LLMRedactionOwner }

func (RedactionOwner) Prepare(
	_ context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	return preparedRedaction{previous: redactionOf(active), next: redactionOf(desired)}, nil
}

// redactionOf is the snapshot's switch; no config fails closed to on.
func redactionOf(s config.ConfigSnapshot) bool {
	return s.Config == nil || s.Config.LLM.RedactNumericLiterals
}

type preparedRedaction struct{ previous, next bool }

func (p preparedRedaction) Commit(context.Context) error {
	SetRedactNumericLiterals(p.next)
	return nil
}

func (p preparedRedaction) Rollback(context.Context) error {
	SetRedactNumericLiterals(p.previous)
	return nil
}

func (preparedRedaction) Drain(context.Context) error { return nil }

// ApplyRedactionConfig sets the switch from cfg at startup; nil fails
// closed to on.
func ApplyRedactionConfig(cfg *config.LLMConfig) {
	SetRedactNumericLiterals(cfg == nil || cfg.RedactNumericLiterals)
}

package agentloop

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Validate checks a run before any model call.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Task) == "" {
		return fmt.Errorf("%w: empty task", ErrInvalidConfig)
	}
	switch c.Protocol {
	case ProtocolAuto, ProtocolNative, ProtocolJSON:
	default:
		return fmt.Errorf("%w: unknown protocol %q", ErrInvalidConfig, c.Protocol)
	}
	if err := c.Budget.validate(); err != nil {
		return err
	}
	if err := c.validateTools(); err != nil {
		return err
	}
	for i, e := range c.Seed {
		if e.ID == "" {
			return fmt.Errorf("%w: seed evidence %d has no id", ErrInvalidConfig, i+1)
		}
	}
	return nil
}

func (b Budget) validate() error {
	switch {
	case b.MaxSteps < 1:
		return fmt.Errorf("%w: max steps must be at least 1", ErrInvalidConfig)
	case b.MaxCalls < 0 || b.MaxCost < 0:
		return fmt.Errorf("%w: negative call or cost budget", ErrInvalidConfig)
	case b.Wall <= 0 || b.StepTimeout <= 0:
		return fmt.Errorf("%w: wall clock and step timeout must be positive",
			ErrInvalidConfig)
	case b.MaxTokens < 1 || b.StepTokens < 1:
		return fmt.Errorf("%w: token budgets must be positive", ErrInvalidConfig)
	}
	return nil
}

func (c Config) validateTools() error {
	if !toolName.MatchString(c.Final.Name) || !schemaObject(c.Final.Parameters) {
		return fmt.Errorf("%w: the final tool needs a name and an object schema",
			ErrInvalidConfig)
	}
	seen := map[string]bool{c.Final.Name: true}
	for _, t := range c.Tools {
		switch {
		case !toolName.MatchString(t.Name):
			return fmt.Errorf("%w: invalid tool name %q", ErrInvalidConfig, t.Name)
		case seen[t.Name]:
			return fmt.Errorf("%w: duplicate tool %q", ErrInvalidConfig, t.Name)
		case t.Run == nil || t.Cost < 0:
			return fmt.Errorf("%w: tool %q needs a run function and a cost >= 0",
				ErrInvalidConfig, t.Name)
		case len(t.Parameters) > 0 && !schemaObject(t.Parameters):
			return fmt.Errorf("%w: tool %q parameters must be a JSON object",
				ErrInvalidConfig, t.Name)
		}
		seen[t.Name] = true
	}
	if len(c.Tools)+1 > llm.MaxTools {
		return fmt.Errorf("%w: %d tools over the client's %d", ErrInvalidConfig,
			len(c.Tools)+1, llm.MaxTools)
	}
	return nil
}

func schemaObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

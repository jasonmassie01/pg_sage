package api

import (
	"bytes"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/llm"
)

// redactPlanJSON is a captured EXPLAIN document as a viewer may see it:
// every string value has its SQL string literals redacted (the redaction
// of text sent to an LLM), numbers kept exact. An auto_explain plan comes
// from a logged statement with its real parameter values, so a filter
// such as (email = 'a@b.c') would otherwise reach every viewer. A document
// that does not decode is left out (nil), never shown unredacted.
func redactPlanJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	out, err := json.Marshal(redactPlanValue(v))
	if err != nil {
		return nil
	}
	return out
}

func redactPlanValue(v any) any {
	switch x := v.(type) {
	case string:
		return llm.RedactSQLLiterals(x)
	case map[string]any:
		for k, val := range x {
			x[k] = redactPlanValue(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactPlanValue(x[i])
		}
		return x
	}
	return v
}

package api

import (
	"strconv"
	"strings"
)

// JSON numbers cannot carry a signed64 query hash through a browser unchanged.
// Restore compatible legacy detail numbers from the finding's exact identity,
// and expose IDs as strings without changing any measured numeric fields.
func normalizeFindingQueryID(detail any, objectType, object string) {
	fields, ok := detail.(map[string]any)
	if !ok || objectType != "query" || !strings.HasPrefix(object, "queryid:") {
		return
	}
	text := strings.TrimPrefix(object, "queryid:")
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || strconv.FormatInt(id, 10) != text {
		return
	}
	value, exists := fields["queryid"]
	if !exists || value == nil {
		return
	}
	compatible := false
	switch value := value.(type) {
	case float64:
		compatible = value == float64(id)
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		compatible = err == nil && parsed == id
	}
	if compatible {
		fields["queryid"] = text
		return
	}
	// Conflicting identifiers must not become a plausible but wrong artifact.
	fields["queryid"] = nil
	fields["queryid_unavailable"] = "detail and finding identifiers do not agree"
}

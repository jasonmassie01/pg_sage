package notify

import (
	"encoding/json"
	"fmt"
)

// parseConfig unmarshals JSONB config bytes into a string map. A malformed
// config is returned as an error rather than swallowed into an empty map,
// which previously surfaced as a misleading "missing webhook_url" instead
// of the real "bad config" cause (audit L2).
func parseConfig(data []byte) (map[string]string, error) {
	m := make(map[string]string)
	if len(data) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing channel config: %w", err)
	}
	return m, nil
}

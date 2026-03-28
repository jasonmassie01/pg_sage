package notify

import "encoding/json"

// parseConfig unmarshals JSONB config bytes into a string map.
func parseConfig(data []byte) map[string]string {
	m := make(map[string]string)
	if len(data) == 0 {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

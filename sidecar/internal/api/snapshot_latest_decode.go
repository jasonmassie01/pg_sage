package api

import (
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/collector"
)

// decodeLatestSnapshot turns a stored category document into the value
// /snapshots/latest serves. foreign_keys goes through the collector's
// decoder, so a row stored before collection-04 (one element per column,
// no schema) is served in the current one-element-per-constraint shape.
// Every other category is served as stored.
func decodeLatestSnapshot(metric string, data []byte) (any, error) {
	if metric == "foreign_keys" {
		fks, err := collector.DecodeForeignKeys(data)
		if err != nil {
			return nil, err
		}
		return fks, nil
	}
	var parsed any
	_ = json.Unmarshal(data, &parsed)
	return parsed, nil
}

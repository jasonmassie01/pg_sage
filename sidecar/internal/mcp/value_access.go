package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/value"
)

// ValueReader produces the fleet-wide value report (value.FleetService).
type ValueReader interface {
	Get(context.Context, value.Filter) (value.Report, error)
}

// FleetValueAccess serves the get_value tool from the fleet-wide value
// report, so an agent sees every monitored database's ledger (D3).
type FleetValueAccess struct{ reader ValueReader }

func NewValueAccess(reader ValueReader) *FleetValueAccess {
	return &FleetValueAccess{reader: reader}
}

func (access *FleetValueAccess) GetValue(ctx context.Context) (map[string]any, error) {
	if access == nil || access.reader == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	report, err := access.reader.Get(ctx, value.Filter{})
	if err != nil {
		return nil, fmt.Errorf("read MCP value: %w", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("encode MCP value: %w", err)
	}
	result := make(map[string]any)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode MCP value: %w", err)
	}
	return result, nil
}

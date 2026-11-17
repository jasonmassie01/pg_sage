package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/pressure"
)

// check_pressure (guard-01a) answers a backfill or bulk write between
// batches: proceed, slow (with a batch size and a sleep) or stop, with
// each input's value, limit and freshness; the same check as GET
// /api/v1/databases/{db}/pressure. It reads bounded catalog views (cached
// for 2 s per table) and never acts: the caller decides.

const toolCheckPressure = "check_pressure"

// PressureRequest is the resolved database and the check's arguments.
type PressureRequest struct {
	Database string `json:"database,omitempty"`
	Table    string `json:"table,omitempty"`
	Job      string `json:"job,omitempty"`
	Batch    int    `json:"batch,omitempty"`
}

// PressureBackend serves check_pressure.
type PressureBackend interface {
	CheckPressure(context.Context, PressureRequest) (any, error)
}

// ErrPressureDatabaseNotFound is a database that is not monitored.
var ErrPressureDatabaseNotFound = errors.New("database not found")

func pressureTools() []Tool {
	return []Tool{{Name: toolCheckPressure, Description: "Ask whether a backfill or bulk " +
		"write should proceed, slow down or stop. Call it between batches: the answer " +
		"is proceed, slow (halve the batch for each input in its slow band, with a " +
		"suggested batch size and a sleep in seconds) or stop, with each input's value, " +
		"limits and freshness: replica replay lag, transaction and multixact ID age " +
		"against the freeze maximum, a vacuum on the target table, WAL bytes per second, " +
		"the lock queue on the target table and free storage. Limits are the policy's " +
		"pressure block, by default half of each alert threshold, so the signal comes " +
		"before the alert. Answers are cached for 2 s per table. Read-only: it never " +
		"acts on the database or the job",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"table":{"type":"string","maxLength":260,"description":"the table the job ` +
			`writes, schema-qualified (resolved with to_regclass)"},` +
			`"job":{"type":"string","maxLength":128,"description":"the job's name, ` +
			`echoed in the answer"},` +
			`"batch":{"type":"integer","minimum":0,"description":"the current batch ` +
			`size, for the suggested size"}},"additionalProperties":false}`)}}
}

func (s *Server) callPressureTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var req PressureRequest
	if !decodeStrict(raw, &req) || req.Batch < 0 {
		return nil, failure(codeInvalidParams, "invalid arguments: check_pressure takes "+
			"table (a string), job (a string) and batch (an integer >= 0)")
	}
	if db, ok := DatabaseFromContext(ctx); ok {
		req.Database = db
	}
	backend, ok := s.backend.(PressureBackend)
	if !ok {
		return nil, failure(codeInternal, "pressure check unavailable")
	}
	result, err := backend.CheckPressure(ctx, req)
	switch {
	case errors.Is(err, pressure.ErrUnknownTable):
		return nil, failure(codeNotFound, "table not found: "+err.Error())
	case errors.Is(err, ErrPressureDatabaseNotFound):
		return nil, failure(codeNotFound, "database not found")
	case errors.Is(err, pressure.ErrInvalidRequest):
		return nil, failure(codeInvalidParams, "invalid arguments: "+err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, failure(codeCancelled, "request cancelled")
	case err != nil:
		return nil, failure(codeInternal, "pressure not checked: "+err.Error())
	}
	return result, nil
}

// CheckPressure serves check_pressure.
func (backend *ProductionBackend) CheckPressure(ctx context.Context,
	request PressureRequest) (any, error) {
	if backend.dependencies.Pressure == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Pressure.CheckPressure(ctx, request)
}

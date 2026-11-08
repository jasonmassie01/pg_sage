package lakebaseobs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/databricks/databricks-sdk-go/service/sql"
)

// StatementClient is the only SDK surface used. SDK authentication refresh and
// workspace containment stay owned by databricksauth, never payload consumers.
type StatementClient interface {
	ExecuteStatement(context.Context, sql.ExecuteStatementRequest) (*sql.StatementResponse, error)
	GetStatement(context.Context, sql.GetStatementRequest) (*sql.StatementResponse, error)
	GetStatementResultChunkN(context.Context, sql.GetStatementResultChunkNRequest) (*sql.ResultData, error)
	CancelExecution(context.Context, sql.CancelExecutionRequest) error
}

type SDKReader struct {
	client  StatementClient
	options Options
}

var statementID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func NewSDKReader(client StatementClient, options Options) (*SDKReader, error) {
	options = options.defaults()
	if client == nil {
		return nil, errors.New("lakebase telemetry SDK client required")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &SDKReader{client, options}, nil
}

func (r *SDKReader) ReadWindow(parent context.Context, stream Stream, scope Scope, w Window) (Batch, error) {
	stmt, params, columns, err := query(stream, scope, w, r.options)
	if err != nil {
		return Batch{}, err
	}
	ctx, cancel := context.WithTimeout(parent, r.options.QueryTimeout)
	defer cancel()
	response, err := r.client.ExecuteStatement(ctx, sql.ExecuteStatementRequest{WarehouseId: r.options.WarehouseID, Statement: stmt, Parameters: params, Disposition: sql.DispositionInline, Format: sql.FormatJsonArray, WaitTimeout: "0s", OnWaitTimeout: sql.ExecuteStatementRequestOnWaitTimeoutContinue, ByteLimit: int64(r.options.MaxBytes), RowLimit: int64(r.options.MaxRows + 1)})
	if err != nil {
		return Batch{}, sdkError(ctx, "submit")
	}
	if response == nil || !statementID.MatchString(response.StatementId) {
		return Batch{}, ErrSchema
	}
	id := response.StatementId
	complete := false
	defer func() {
		if !complete {
			cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_ = r.client.CancelExecution(cleanup, sql.CancelExecutionRequest{StatementId: id})
		}
	}()
	for {
		if response.Status == nil {
			return Batch{}, ErrSchema
		}
		switch response.Status.State {
		case sql.StatementStateSucceeded:
			complete = true
		case sql.StatementStatePending, sql.StatementStateRunning:
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Batch{}, ctx.Err()
			case <-timer.C:
			}
			response, err = r.client.GetStatement(ctx, sql.GetStatementRequest{StatementId: id})
			if err != nil {
				return Batch{}, sdkError(ctx, "poll")
			}
			if response == nil || response.StatementId != id {
				return Batch{}, ErrSchema
			}
			continue
		case sql.StatementStateFailed, sql.StatementStateCanceled, sql.StatementStateClosed:
			complete = true
			return Batch{}, errors.New("lakebase telemetry statement failed or unavailable")
		default:
			return Batch{}, ErrSchema
		}
		break
	}
	if response.Manifest == nil || response.Manifest.Schema == nil {
		return Batch{}, ErrSchema
	}
	if response.Manifest.Truncated || response.Manifest.TotalRowCount > int64(r.options.MaxRows) {
		return Batch{}, ErrBudget
	}
	if response.Manifest.Format != sql.FormatJsonArray || len(response.Manifest.Schema.Columns) != len(columns) {
		return Batch{}, ErrSchema
	}
	for i, c := range response.Manifest.Schema.Columns {
		if c.Name != columns[i] || c.TypeName != sql.ColumnInfoTypeNameString {
			return Batch{}, ErrSchema
		}
	}
	b := Batch{QueryID: id}
	chunk := response.Result
	if chunk == nil && response.Manifest.TotalRowCount == 0 {
		chunk = &sql.ResultData{}
	}
	last := -1
	for {
		if chunk == nil || len(chunk.ExternalLinks) > 0 || chunk.ChunkIndex <= last {
			return Batch{}, ErrSchema
		}
		last = chunk.ChunkIndex
		for _, values := range chunk.DataArray {
			if len(values) != len(columns) {
				return Batch{}, ErrSchema
			}
			if len(b.Events) >= r.options.MaxRows {
				return Batch{}, ErrBudget
			}
			fields := map[string]string{}
			for i, v := range values {
				b.Bytes += len(v)
				if b.Bytes > r.options.MaxBytes {
					return Batch{}, ErrBudget
				}
				fields[columns[i]] = v
			}
			e, eerr := NewEvent(stream, scope, fields, time.Now(), r.options.AllowPayload)
			if eerr != nil {
				return Batch{}, eerr
			}
			if e.OccurredAt.Before(w.From) || !e.OccurredAt.Before(w.To) {
				return Batch{}, ErrScope
			}
			b.Events = append(b.Events, e)
		}
		if chunk.NextChunkIndex == 0 && chunk.NextChunkInternalLink == "" {
			break
		}
		if chunk.NextChunkIndex <= last {
			return Batch{}, ErrSchema
		}
		chunk, err = r.client.GetStatementResultChunkN(ctx, sql.GetStatementResultChunkNRequest{StatementId: id, ChunkIndex: chunk.NextChunkIndex})
		if err != nil {
			return Batch{}, sdkError(ctx, "fetch")
		}
	}
	if int64(len(b.Events)) != response.Manifest.TotalRowCount {
		return Batch{}, ErrSchema
	}
	return b, nil
}

// Provider errors may embed SQL or result content; return operation-only errors.
func sdkError(ctx context.Context, op string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("lakebase telemetry SDK %s failed (verify warehouse, UC access and schema)", op)
}

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrBackendApprovalRequired = errors.New("backend signal requires approval")
	ErrBackendEvidenceStale    = errors.New("backend evidence is stale")
)

type backendSignalEvidence struct {
	PID        int       `json:"pid"`
	QueryID    int64     `json:"query_id"`
	QueryStart time.Time `json:"query_start"`
	Query      string    `json:"query"`
	AppName    string    `json:"app_name"`
}

func (e *Executor) executeApprovedBackendSignal(
	ctx context.Context,
	sql string,
	detail json.RawMessage,
	approvedBy *int,
) error {
	operation, pid, ok := parseBackendSignal(sql)
	if !ok {
		return fmt.Errorf("invalid backend signal SQL")
	}
	if approvedBy == nil {
		return ErrBackendApprovalRequired
	}
	evidence, err := parseBackendEvidence(detail, pid)
	if err != nil {
		return err
	}
	return e.signalMatchingBackend(ctx, operation, evidence)
}

func parseBackendEvidence(
	detail json.RawMessage,
	pid int,
) (backendSignalEvidence, error) {
	var evidence backendSignalEvidence
	if err := json.Unmarshal(detail, &evidence); err != nil {
		return evidence, fmt.Errorf("backend evidence is invalid: %w", err)
	}
	if evidence.PID != pid || evidence.PID <= 0 ||
		evidence.QueryStart.IsZero() || strings.TrimSpace(evidence.Query) == "" {
		return evidence, fmt.Errorf("%w: incomplete or mismatched evidence", ErrBackendEvidenceStale)
	}
	return evidence, nil
}

func (e *Executor) signalMatchingBackend(
	ctx context.Context,
	operation string,
	evidence backendSignalEvidence,
) error {
	query := cancelMatchingBackendSQL
	if operation == "terminate" {
		query = terminateMatchingBackendSQL
	}
	var signaled bool
	err := e.pool.QueryRow(
		ctx,
		query,
		evidence.PID,
		evidence.QueryStart,
		evidence.QueryID,
		evidence.AppName,
		evidence.Query,
	).Scan(&signaled)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !signaled) {
		return fmt.Errorf("%w: backend no longer matches", ErrBackendEvidenceStale)
	}
	if err != nil {
		return fmt.Errorf("signaling validated backend: %w", err)
	}
	return nil
}

const cancelMatchingBackendSQL = `/* pg_sage */
SELECT pg_cancel_backend(a.pid)
  FROM pg_stat_activity AS a
 WHERE a.pid = $1
   AND a.query_start = $2
   AND COALESCE(a.query_id, 0) = $3
   AND a.application_name = $4
   AND LEFT(a.query, 200) = $5
   AND a.state = 'active'
   AND a.application_name NOT ILIKE '%pg_sage%'`

const terminateMatchingBackendSQL = `/* pg_sage */
SELECT pg_terminate_backend(a.pid)
  FROM pg_stat_activity AS a
  JOIN pg_roles AS r ON r.rolname = a.usename
 WHERE a.pid = $1
   AND a.query_start = $2
   AND COALESCE(a.query_id, 0) = $3
   AND a.application_name = $4
   AND LEFT(a.query, 200) = $5
   AND a.state = 'active'
   AND a.backend_type = 'client backend'
   AND NOT r.rolsuper
   AND a.application_name NOT ILIKE '%pg_sage%'
   AND a.application_name NOT ILIKE '%autovacuum%'`

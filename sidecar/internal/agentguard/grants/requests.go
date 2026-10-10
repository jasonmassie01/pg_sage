package grants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// Request statuses (sage.guard_grant_requests.status).
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
	RequestExpired  = "expired"
	RequestFailed   = "failed"
	// RequestRecorded is an L1 proposal: recorded, never approvable.
	RequestRecorded = "recorded"
)

// ErrRequestNotPending is a decision on a request that is no longer
// pending (decided, expired or recorded): approvals are single use.
var ErrRequestNotPending = errors.New("grants: request is not pending")

// Request is one agent capability request.
type Request struct {
	ID              int64           `json:"id"`
	DatabaseID      string          `json:"database_id"`
	PrincipalID     string          `json:"principal_id"`
	Capability      string          `json:"capability"`
	Objects         []ObjectRequest `json:"objects"`
	DurationMinutes int             `json:"duration_minutes"`
	Reason          string          `json:"reason"`
	TaskID          string          `json:"task_id,omitempty"`
	Status          string          `json:"status"`
	ReasonCode      string          `json:"reason_code,omitempty"`
	Detail          string          `json:"detail,omitempty"`
	RequestedAt     time.Time       `json:"requested_at"`
	ExpiresAt       time.Time       `json:"expires_at"`
	DecidedBy       *int            `json:"decided_by,omitempty"`
	DecidedAt       *time.Time      `json:"decided_at,omitempty"`
	GrantIDs        []int64         `json:"grant_ids"`
}

const requestColumns = `id, database_id::text, principal_id, capability, objects,
  duration_minutes, reason, COALESCE(task_id, ''), status, COALESCE(reason_code, ''),
  COALESCE(detail, ''), requested_at, expires_at, decided_by, decided_at, grant_ids`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	var objects []byte
	err := row.Scan(&r.ID, &r.DatabaseID, &r.PrincipalID, &r.Capability, &objects,
		&r.DurationMinutes, &r.Reason, &r.TaskID, &r.Status, &r.ReasonCode, &r.Detail,
		&r.RequestedAt, &r.ExpiresAt, &r.DecidedBy, &r.DecidedAt, &r.GrantIDs)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(objects, &r.Objects); err != nil {
		return r, fmt.Errorf("grants: request %d objects: %w", r.ID, err)
	}
	return r, nil
}

// GetRequest reads one request.
func GetRequest(ctx context.Context, q Querier, id int64) (Request, error) {
	r, err := scanRequest(q.QueryRow(ctx, `/* pg_sage guard_grant_request v1 */ SELECT `+
		requestColumns+` FROM sage.guard_grant_requests WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, fmt.Errorf("%w: request %d", agentguard.ErrNotFound, id)
	}
	if err != nil {
		return Request{}, fmt.Errorf("grants: reading request %d: %w", id, err)
	}
	return r, nil
}

// insertRequest stores a request; ttl is the approval window (0 for a
// recorded proposal, which never waits).
func insertRequest(ctx context.Context, q Querier, databaseID, principalID string,
	in CapabilityRequest, status string, ttl time.Duration) (Request, error) {
	objects, err := json.Marshal(in.Objects)
	if err != nil {
		return Request{}, fmt.Errorf("grants: encoding request objects: %w", err)
	}
	code := ""
	if status == RequestRecorded {
		code = "agent_proposal_recorded"
	}
	r, err := scanRequest(q.QueryRow(ctx, `/* pg_sage guard_grant_request v1 */
		INSERT INTO sage.guard_grant_requests (database_id, principal_id, capability,
		  objects, duration_minutes, reason, task_id, status, reason_code, expires_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, NULLIF($9, ''),
		  now() + make_interval(secs => $10))
		RETURNING `+requestColumns, databaseID, principalID, in.Capability, objects,
		in.DurationMinutes, in.Reason, in.TaskID, status, code, ttl.Seconds()))
	if err != nil {
		return Request{}, fmt.Errorf("grants: recording request: %w", err)
	}
	return r, nil
}

// claimSQL decides a pending request in one statement (single use): a
// request past its expiry becomes expired instead.
const claimSQL = `/* pg_sage guard_grant_request v1 */
UPDATE sage.guard_grant_requests SET
  status = CASE WHEN expires_at > now() THEN $2::text ELSE 'expired' END,
  decided_by = CASE WHEN expires_at > now() THEN $3::int END,
  decided_at = CASE WHEN expires_at > now() THEN now() END
WHERE id = $1 AND principal_id = $4 AND status = 'pending'
RETURNING ` + requestColumns

// claimRequest moves principalID's pending request to status for userID.
func claimRequest(ctx context.Context, q Querier, principalID string, id int64,
	status string, userID int) (Request, error) {
	r, err := scanRequest(q.QueryRow(ctx, claimSQL, id, status, userID, principalID))
	if errors.Is(err, pgx.ErrNoRows) {
		got, gerr := GetRequest(ctx, q, id)
		if gerr != nil {
			return Request{}, gerr
		}
		if got.PrincipalID != principalID {
			return Request{}, fmt.Errorf("%w: request %d of principal %s",
				agentguard.ErrNotFound, id, principalID)
		}
		return Request{}, fmt.Errorf("%w: request %d", ErrRequestNotPending, id)
	}
	if err != nil {
		return Request{}, fmt.Errorf("grants: deciding request %d: %w", id, err)
	}
	if r.Status == RequestExpired {
		return r, fmt.Errorf("%w: request %d expired at %s", ErrRequestNotPending, id,
			r.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return r, nil
}

// finishRequest records what the approval did.
func finishRequest(ctx context.Context, q Querier, id int64, grantErr error,
	grantIDs []int64) error {
	status, code, detail := RequestApproved, "", ""
	if grantErr != nil {
		status, detail = RequestFailed, truncate(grantErr.Error(), 4000)
		if d, ok := agentguard.IsDenied(grantErr); ok {
			code = string(d.Reason)
		}
	}
	if grantIDs == nil {
		grantIDs = []int64{}
	}
	_, err := q.Exec(ctx, `/* pg_sage guard_grant_request v1 */
		UPDATE sage.guard_grant_requests SET status = $2, reason_code = NULLIF($3, ''),
		  detail = NULLIF($4, ''), grant_ids = $5 WHERE id = $1`,
		id, status, code, detail, grantIDs)
	if err != nil {
		return fmt.Errorf("grants: recording the outcome of request %d: %w", id, err)
	}
	return nil
}

// RequestFilter selects a principal's requests, newest first.
type RequestFilter struct {
	PrincipalID string
	Status      string // "" is every status
	Limit       int    // 1-200
	Cursor      string
}

// RequestPage is one page of requests.
type RequestPage struct {
	Items      []Request `json:"items"`
	NextCursor string    `json:"next_cursor"`
}

func validStatus(s string) bool {
	switch s {
	case "", RequestPending, RequestApproved, RequestDenied, RequestExpired, RequestFailed,
		RequestRecorded:
		return true
	}
	return false
}

// ListRequests pages a principal's requests (guard_grant_requests_principal_idx).
func ListRequests(ctx context.Context, q Querier, f RequestFilter) (RequestPage, error) {
	if !agentguard.ValidID(f.PrincipalID) || f.Limit < 1 || f.Limit > 200 ||
		!validStatus(f.Status) {
		return RequestPage{}, invalidf("principal, limit 1-200 and a known status")
	}
	before, err := parseCursor(f.Cursor)
	if err != nil {
		return RequestPage{}, err
	}
	rows, err := q.Query(ctx, `/* pg_sage guard_grant_request v1 */ SELECT `+requestColumns+`
		FROM sage.guard_grant_requests WHERE principal_id = $1
		  AND ($2 = '' OR status = $2) AND ($3 = 0 OR id < $3)
		ORDER BY id DESC LIMIT $4`, f.PrincipalID, f.Status, before, f.Limit+1)
	if err != nil {
		return RequestPage{}, fmt.Errorf("grants: listing requests: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) {
		return scanRequest(r)
	})
	if err != nil {
		return RequestPage{}, fmt.Errorf("grants: listing requests: %w", err)
	}
	page := RequestPage{Items: items}
	if len(items) > f.Limit {
		page.Items = items[:f.Limit]
		page.NextCursor = strconv.FormatInt(page.Items[f.Limit-1].ID, 10)
	}
	if page.Items == nil {
		page.Items = []Request{}
	}
	return page, nil
}

package lakebaseobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// MaxDeliveryAttempts bounds how often one event is offered to one consumer.
// After the last failed attempt the event is acknowledged as delivery_failed,
// so one poison event cannot stall the consumer for the payload's retention.
const MaxDeliveryAttempts = 5

// maxFailureStreak stops a poll after this many consecutive failed events.
// A sink-wide fault that is not classified as an outage (a full disk, a
// read-only or failed-over sink, a revoked grant) then spends attempts on a
// few events per poll instead of exhausting the whole backlog as
// delivery_failed, while isolated poison events still never stall the rest.
const maxFailureStreak = 3

const (
	retryBaseDelay = 30 * time.Second
	retryMaxDelay  = 15 * time.Minute
	// backlogLimit bounds the backlog count; a count at the limit means
	// "at least this many".
	backlogLimit = 10001
	// sqlstateChars are the characters a SQLSTATE code may contain.
	sqlstateChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// errAckUnsupported fails a commit that must acknowledge events for a
// consumer but goes through a store that cannot do so in the same transaction.
var errAckUnsupported = errors.New("lakebase telemetry store cannot acknowledge events at commit")

// StreamConsumer is an optional Consumer extension. A consumer that lists
// streams is delivered only those. Events of the runtime's other streams are
// acknowledged for it inside the batch commit that stores them, under the same
// policy fence, and are never offered to Deliver. A consumer without Streams,
// or with an empty list, is delivered every stream.
type StreamConsumer interface {
	Consumer
	Streams() []Stream
}

// OutcomeObserver is an optional Consumer extension for in-process counts.
type OutcomeObserver interface {
	// Observe sees each newly stored event acknowledged at commit for this
	// consumer, once, after that commit succeeds. It must not write a sink.
	Observe(Event)
	// DeliveryFailed sees an event acknowledged after MaxDeliveryAttempts
	// failed deliveries.
	DeliveryFailed(Event)
}

// Backlog counts a consumer's deliverable, unacknowledged events. Rows equal
// to backlogLimit sets Capped and means at least that many. Oldest is zero
// when nothing is pending.
type Backlog struct {
	Rows   int
	Capped bool
	Oldest time.Time
}

// DeliveryStatus is a consumer's backlog as measured at the end of the last
// poll. It includes events waiting for a retry.
type DeliveryStatus struct {
	Consumer                string     `json:"consumer"`
	PendingRows             int        `json:"pending_rows"`
	PendingRowsCapped       bool       `json:"pending_rows_capped"`
	OldestPendingAt         *time.Time `json:"oldest_pending_at,omitempty"`
	OldestPendingAgeSeconds *int64     `json:"oldest_pending_age_seconds,omitempty"`
}

// retryDelay is the wait after the attempt-th failed delivery of an event.
func retryDelay(attempt int) time.Duration {
	d := retryBaseDelay
	for i := 1; i < attempt && d < retryMaxDelay; i++ {
		d *= 2
	}
	return min(d, retryMaxDelay)
}

// failureClass names a delivery failure for durable state and operators.
// It never carries error text, which can hold SQL or payload values.
func failureClass(err error) string {
	var pg *pgconn.PgError
	var connect *pgconn.ConnectError
	switch {
	case err == nil:
		return "sink_error"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pg):
		if len(pg.Code) != 5 || strings.Trim(pg.Code, sqlstateChars) != "" {
			return "sqlstate_unknown"
		}
		return "sqlstate_" + pg.Code
	case errors.As(err, &connect):
		return "connect"
	case errors.Is(err, ErrSchema):
		return "schema"
	default:
		return "sink_error"
	}
}

// sinkUnavailable reports a failure of the sink or control database rather
// than of one event: a timeout, cancellation, failed connection, or SQLSTATE
// class 08 (connection exception) or 57 (operator intervention). Such a
// failure spends no attempt; delivery stops and resumes on the next poll.
func sinkUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var connect *pgconn.ConnectError
	if errors.As(err, &connect) {
		return true
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	return strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "57")
}

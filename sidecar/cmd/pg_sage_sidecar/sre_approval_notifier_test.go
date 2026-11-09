package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// policy-08: a Sage SRE approval request is a queue approval event for its
// queue item, sent through the database's approval-card notifier, so it
// gets a channel-bound, hash-bound, expiring card like every other
// action; the retired proposal-id buttons are never requested.

type approvalEventLog struct {
	events []notify.Event
	err    error
}

func (l *approvalEventLog) Dispatch(_ context.Context, evt notify.Event) error {
	l.events = append(l.events, evt)
	return l.err
}

func sreRequest() sreaction.ApprovalRequest {
	return sreaction.ApprovalRequest{Database: "orders",
		ProposalID: "44444444-4444-4444-8444-444444444444", QueueID: 42,
		Title: "Cancel blocking backend pid 5151", SQL: "SELECT pg_cancel_backend(5151)",
		Summary: "orders: pid 5151 blocks 2 sessions", Risk: "moderate",
		ExpiresAt: time.Now().Add(15 * time.Minute)}
}

func TestSREApprovalIsAQueueApprovalEvent(t *testing.T) {
	log := &approvalEventLog{}
	if err := (dispatcherNotifier{d: log}).ApprovalRequested(context.Background(),
		sreRequest()); err != nil {
		t.Fatalf("approval requested: %v", err)
	}
	if len(log.events) != 1 {
		t.Fatalf("events = %d, want 1", len(log.events))
	}
	evt := log.events[0]
	if evt.Type != "approval_needed" || evt.Data["queue_id"] != 42 ||
		evt.Data["database"] != "orders" || evt.Data["sql"] != "SELECT pg_cancel_backend(5151)" ||
		evt.DedupKey != "approval:orders:42" ||
		!strings.Contains(evt.Subject, "Cancel blocking backend pid 5151") ||
		!strings.Contains(evt.Body, "pid 5151 blocks 2 sessions") {
		t.Fatalf("event = %+v", evt)
	}
	if _, legacy := evt.Data["approval_proposal_id"]; legacy {
		t.Fatalf("the event still asks for proposal-id buttons: %v", evt.Data)
	}
}

func TestSREApprovalNotifierErrors(t *testing.T) {
	log := &approvalEventLog{err: errors.New("rules unavailable")}
	err := (dispatcherNotifier{d: log}).ApprovalRequested(context.Background(), sreRequest())
	if err == nil || !strings.Contains(err.Error(), "rules unavailable") {
		t.Fatalf("dispatch error = %v, want it returned", err)
	}
	bad := sreRequest()
	bad.QueueID = 0
	if err := (dispatcherNotifier{d: &approvalEventLog{}}).ApprovalRequested(context.Background(),
		bad); err == nil {
		t.Fatal("an approval request without a queue item was sent")
	}
	if err := (dispatcherNotifier{}).ApprovalRequested(context.Background(),
		sreRequest()); err == nil {
		t.Fatal("a notifier without a dispatcher reported success")
	}
}

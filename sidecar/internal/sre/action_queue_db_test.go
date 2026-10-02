package sre

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The approval queue adapter writes into the existing approval flow: one
// open sage.findings row (category sre_action, no recommended SQL, so the
// manual "take action" path can never run it) and one sage.action_queue
// row per proposal, in any state.

func approvalItem(id UUID) ApprovalItem {
	return ApprovalItem{ProposalID: id, InvestigationID: NewUUID(), PID: 5151,
		SQL: "SELECT pg_cancel_backend(5151)", Title: "Cancel blocking backend pid 5151",
		Detail: "pid 5151 blocks 2 sessions", ExpiresAt: time.Now().Add(15 * time.Minute)}
}

func TestPGApprovalQueueEnqueueIsIdempotent(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	q := NewPGApprovalQueue(pool, nil)
	id := NewUUID()
	first, err := q.Enqueue(ctx, approvalItem(id))
	if err != nil || !first.Created || first.QueueID <= 0 || first.FindingID <= 0 {
		t.Fatalf("first enqueue = %+v, %v", first, err)
	}
	again, err := q.Enqueue(ctx, approvalItem(id))
	if err != nil || again.Created || again.QueueID != first.QueueID ||
		again.FindingID != first.FindingID {
		t.Fatalf("second enqueue = %+v (%v), want the first item", again, err)
	}
	var category, severity, objectID string
	var recommended *string
	var detail map[string]any
	if err := pool.QueryRow(ctx, `SELECT category, severity, object_identifier,
		recommended_sql, detail FROM sage.findings WHERE id = $1`, first.FindingID).
		Scan(&category, &severity, &objectID, &recommended, &detail); err != nil {
		t.Fatalf("finding: %v", err)
	}
	if category != "sre_action" || severity != "critical" ||
		objectID != ApprovalIdentityPrefix+string(id) || recommended != nil {
		t.Fatalf("finding = %s %s %s %v", category, severity, objectID, recommended)
	}
	for _, key := range []string{"pid", "query", "query_start", "backend_start"} {
		if _, leaked := detail[key]; leaked {
			t.Fatalf("finding detail carries legacy signal evidence %q: %v", key, detail)
		}
	}
	if detail["proposal_id"] != string(id) {
		t.Fatalf("finding detail = %v, want the proposal id", detail)
	}
	status, err := q.Status(ctx, first.QueueID)
	if err != nil || status.Status != "pending" || status.DecidedBy != 0 {
		t.Fatalf("status = %+v, %v", status, err)
	}
}

func TestPGApprovalQueueConcurrentEnqueueCreatesOneItem(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	q := NewPGApprovalQueue(pool, nil)
	id := NewUUID()
	var wg sync.WaitGroup
	created := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := q.Enqueue(ctx, approvalItem(id))
			if err != nil {
				t.Errorf("enqueue: %v", err)
				return
			}
			created <- got.Created
		}()
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_queue WHERE identity_key = $1`,
		ApprovalIdentityPrefix+string(id)).Scan(&rows)
	if n != 1 || rows != 1 {
		t.Fatalf("created %d, rows %d; want exactly one", n, rows)
	}
}

func TestPGApprovalQueueValidatesItems(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	q := NewPGApprovalQueue(pool, nil)
	for name, mutate := range map[string]func(*ApprovalItem){
		"no proposal": func(i *ApprovalItem) { i.ProposalID = "" },
		"no pid":      func(i *ApprovalItem) { i.PID = 0 },
		"other sql":   func(i *ApprovalItem) { i.SQL = "SELECT pg_terminate_backend(5151)" },
		"sql for another pid": func(i *ApprovalItem) {
			i.SQL = "SELECT pg_cancel_backend(1)"
		},
		"expired": func(i *ApprovalItem) { i.ExpiresAt = time.Now().Add(-time.Second) },
	} {
		item := approvalItem(NewUUID())
		mutate(&item)
		if _, err := q.Enqueue(ctx, item); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Enqueue = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := q.Status(ctx, 0); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("status of item 0 = %v, want ErrProposalNotFound", err)
	}
}

func TestPGApprovalQueueVerificationStatus(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	q := NewPGApprovalQueue(pool, nil)
	item, err := q.Enqueue(ctx, approvalItem(NewUUID()))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.SetVerification(ctx, item.QueueID, "verified"); err != nil {
		t.Fatalf("set verification: %v", err)
	}
	if s, _ := q.Status(ctx, item.QueueID); s.VerificationStatus != "verified" {
		t.Fatalf("verification = %+v", s)
	}
	if err := q.SetVerification(ctx, item.QueueID, "rm -rf"); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("unknown verification status = %v, want ErrInvalidRequest", err)
	}
}

func TestPGApprovalQueueResolveClosesTheAnchorFinding(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	q := NewPGApprovalQueue(pool, nil)
	item, err := q.Enqueue(ctx, approvalItem(NewUUID()))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for run := 0; run < 2; run++ {
		if err := q.Resolve(ctx, item.QueueID); err != nil {
			t.Fatalf("resolve %d: %v", run, err)
		}
	}
	var status string
	var resolved *time.Time
	_ = pool.QueryRow(ctx, `SELECT status, resolved_at FROM sage.findings WHERE id = $1`,
		item.FindingID).Scan(&status, &resolved)
	if status != "resolved" || resolved == nil {
		t.Fatalf("finding = %s %v, want resolved", status, resolved)
	}
	if err := q.Resolve(ctx, 0); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("resolve item 0 = %v, want ErrProposalNotFound", err)
	}
}

func TestApprovalIdentityKeyRoundTrip(t *testing.T) {
	id := NewUUID()
	got, ok := ProposalIDFromIdentityKey(ApprovalIdentityPrefix + string(id))
	if !ok || got != id {
		t.Fatalf("round trip = %s %v", got, ok)
	}
	for _, bad := range []string{"", "sre_proposal:", "sre_proposal:x", string(id),
		"other:" + string(id)} {
		if _, ok := ProposalIDFromIdentityKey(bad); ok {
			t.Errorf("%q parsed as a proposal key", bad)
		}
	}
}

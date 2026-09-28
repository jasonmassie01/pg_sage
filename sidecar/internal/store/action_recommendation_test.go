package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

var (
	recPoolOnce sync.Once
	recPool     *pgxpool.Pool
	recPoolErr  error
)

// recStorePool is a bootstrapped pool for the approval tests, which run in
// the untagged suite.
func recStorePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	recPoolOnce.Do(func() {
		recPool, recPoolErr = pgxpool.New(ctx, dsn)
		if recPoolErr == nil {
			recPoolErr = schema.Bootstrap(ctx, recPool)
		}
	})
	if recPoolErr != nil {
		t.Fatalf("test database: %v", recPoolErr)
	}
	return recPool, ctx
}

type queuedRec struct {
	rec     recommendation.Recommendation
	queueID int
}

// queueRecommendation proposes a recommendation for a fresh open finding
// and queues it for approval pinned to its current revision.
func queueRecommendation(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, forward string,
) (queuedRec, recommendation.Proposal) {
	t.Helper()
	target := fmt.Sprintf("public.approval_%d", time.Now().UnixNano())
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql)
		VALUES ('approval_pin', 'warning', 'index', $1, 't', '{}', $2) RETURNING id`,
		target, forward).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	p := recommendation.Proposal{DatabaseName: "approval_db", Category: "approval_pin",
		Target: target, ForwardSQL: forward}
	res, err := recommendation.NewStore(pool).Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	rec := res.Recommendation
	id, err := NewActionStore(pool).ProposeWithMetadata(ctx, nil, findingID, forward, "",
		"safe", ActionProposalMetadata{RecommendationID: rec.ID,
			RecommendationRevision: rec.Revision, ContentHash: rec.ContentHash})
	if err != nil {
		t.Fatal(err)
	}
	return queuedRec{rec: rec, queueID: id}, p
}

func TestApproveQueueApprovesThePinnedRevision(t *testing.T) {
	pool, ctx := recStorePool(t)
	q, _ := queueRecommendation(t, ctx, pool, "CREATE INDEX CONCURRENTLY a ON t (a)")
	action, err := NewActionStore(pool).Approve(ctx, q.queueID, 11)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if action.RecommendationID == nil || *action.RecommendationID != q.rec.ID ||
		action.ContentHash != q.rec.ContentHash {
		t.Fatalf("approved queue row = %+v, want pinned to %d", action, q.rec.ID)
	}
	got, _ := recommendation.NewStore(pool).Get(ctx, q.rec.ID)
	if got.State != recommendation.StateApproved || got.ApprovedBy != "user:11" ||
		got.ApprovedHash != q.rec.ContentHash {
		t.Fatalf("recommendation after queue approval = %+v", got)
	}
}

// C04 at the approval surface: a queued approval of content A cannot
// approve the recommendation once it has been revised to B.
func TestApproveQueueRefusesRevisedRecommendation(t *testing.T) {
	pool, ctx := recStorePool(t)
	q, p := queueRecommendation(t, ctx, pool, "CREATE INDEX CONCURRENTLY a ON t (a)")
	recs := recommendation.NewStore(pool)
	// Revision B arrives; the store supersedes the queued row for A.
	p.InverseSQL = "DROP INDEX CONCURRENTLY a"
	if res, err := recs.Propose(ctx, p); err != nil ||
		res.Outcome != recommendation.OutcomeRevised {
		t.Fatalf("revise: %+v, %v", res, err)
	}
	if _, err := NewActionStore(pool).Approve(ctx, q.queueID, 11); !errors.Is(
		err, recommendation.ErrRevised) {
		t.Fatalf("approving a superseded proposal: err=%v, want ErrRevised", err)
	}
	// A row for A that escaped supersession (a racing writer) is still refused.
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET status='pending'
		WHERE id=$1`, q.queueID); err != nil {
		t.Fatal(err)
	}
	_, err := NewActionStore(pool).Approve(ctx, q.queueID, 11)
	if !errors.Is(err, recommendation.ErrRevised) {
		t.Fatalf("approve stale pin: err=%v, want ErrRevised", err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM sage.action_queue WHERE id=$1`,
		q.queueID).Scan(&status)
	got, _ := recs.Get(ctx, q.rec.ID)
	if status != "pending" || got.State != recommendation.StateProposed || got.Revision != 2 {
		t.Fatalf("refused approval changed state: queue=%s rec=%s rev=%d",
			status, got.State, got.Revision)
	}
}

func TestApproveQueueWithoutRecommendationStillWorks(t *testing.T) {
	pool, ctx := recStorePool(t)
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql)
		VALUES ('approval_legacy', 'warning', 'index', $1, 't', '{}', 'VACUUM t')
		RETURNING id`, fmt.Sprintf("public.l_%d", time.Now().UnixNano())).
		Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	s := NewActionStore(pool)
	id, err := s.Propose(ctx, nil, findingID, "VACUUM t", "", "safe")
	if err != nil {
		t.Fatal(err)
	}
	action, err := s.Approve(ctx, id, 3)
	if err != nil || action.Status != "approved" || action.RecommendationID != nil {
		t.Fatalf("legacy approval = %+v, %v", action, err)
	}
}

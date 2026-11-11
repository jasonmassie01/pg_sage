package auditchain_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// audit-11: the chain proved what ran, not why it was allowed. The
// decision ledger is chained: what was decided, at which tier and under
// which policy is sealed; the outcome columns pg_sage updates later
// (verdict, reason, evidence, queue link, resolution) are state.

// insertDecision records one decision the way the ledger does and returns
// its id.
func insertDecision(t *testing.T, pool *pgxpool.Pool, verdict, tier, fingerprint string) int64 {
	t.Helper()
	var id int64
	var fp any
	if fingerprint != "" {
		fp = fingerprint
	}
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.decision (database_id,
		feature, intent, target_objects, policy_version, verdict, risk_tier, reason,
		evidence, evidence_id, fingerprint) VALUES (7, 'index', 'create_index',
		'["public.orders"]', 3, $1, $2, 'gate reason', '{"budget": "index"}',
		'ev-' || md5(random()::text), $3) RETURNING id`, verdict, tier, fp).Scan(&id)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return id
}

func decisionLinks(t *testing.T, pool *pgxpool.Pool, id int64) []auditchain.Link {
	t.Helper()
	links, err := auditchain.LinksForRow(context.Background(), pool, "decision", id)
	if err != nil {
		t.Fatalf("links of decision %d: %v", id, err)
	}
	return links
}

// The decision chain is built in: a sealed authority, a separate state.
func TestDecisionChainIsInstalled(t *testing.T) {
	pool := freshDB(t, "chain_decision_installed")
	spec, ok := auditchain.SpecFor("decision")
	if !ok || spec.Table != "sage.decision" || len(spec.State) == 0 {
		t.Fatalf("decision spec = %+v (found %v)", spec, ok)
	}
	id := insertDecision(t, pool, "queue_approval", "high", "")
	links := decisionLinks(t, pool, id)
	if len(links) != 1 || links[0].Op != "I" || links[0].V != 1 {
		t.Fatalf("links of a new decision = %+v, want one insert link", links)
	}
	rep := verify(t, pool, auditchain.DecisionLedger, auditchain.Window{})
	if !rep.OK() || rep.Links != 1 || rep.RowsChecked != 1 {
		t.Fatalf("clean decision chain: %+v", rep)
	}
}

// Rewriting a decision's tier is reported as an edit, whether the
// triggers saw it or not; rewriting its verdict behind the chain is
// reported as a state edit. The pack of such a change no longer reads
// clean.
func TestDecisionChainReportsAuthorityRewrites(t *testing.T) {
	pool := freshDB(t, "chain_decision_tamper")
	tier := insertDecision(t, pool, "queue_approval", "high", "")
	verdict := insertDecision(t, pool, "queue_approval", "high", "")
	plain := insertDecision(t, pool, "queue_approval", "high", "")
	clean := insertDecision(t, pool, "queue_approval", "high", "")
	bypass(t, pool, "UPDATE sage.decision SET risk_tier = 'safe' WHERE id = $1", tier)
	bypass(t, pool, "UPDATE sage.decision SET verdict = 'execute' WHERE id = $1", verdict)
	// A plain UPDATE of a sealed column changes no state: no link, an edit.
	exec(t, pool, "UPDATE sage.decision SET risk_tier = 'safe' WHERE id = $1", plain)
	rep := verify(t, pool, auditchain.DecisionLedger, auditchain.Window{})
	kinds := problemKinds(rep)
	if rep.OK() || !containsKind(kinds[tier], auditchain.KindRowEdited) ||
		!containsKind(kinds[verdict], auditchain.KindStateEdited) ||
		!containsKind(kinds[plain], auditchain.KindRowEdited) || len(kinds[clean]) != 0 {
		t.Fatalf("authority rewrites: problems %+v", rep.Problems)
	}
}

// Linking a queue item and resolving the decision is one state change: one
// U link, and the chain verifies. Turning an execute into the block it
// became (vacuum admission, maintenance precheck) is a state change too.
func TestDecisionChainStateTransitions(t *testing.T) {
	pool := freshDB(t, "chain_decision_state")
	queued := insertDecision(t, pool, "queue_approval", "moderate", "")
	var queueID int
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_queue
		(proposed_sql, action_risk) VALUES ('CREATE INDEX x', 'moderate')
		RETURNING id`).Scan(&queueID); err != nil {
		t.Fatalf("insert queue item: %v", err)
	}
	exec(t, pool, `UPDATE sage.decision SET queue_id = $2,
		resolved_at = COALESCE(resolved_at, now()) WHERE id = $1`, queued, queueID)
	blocked := insertDecision(t, pool, "execute", "safe", "")
	exec(t, pool, `UPDATE sage.decision SET verdict = 'blocked', reason = 'vacuum running',
		evidence = evidence || '{"vacuum_progress": {"pid": 1}}' WHERE id = $1`, blocked)
	for _, id := range []int64{queued, blocked} {
		links := decisionLinks(t, pool, id)
		if len(links) != 2 || links[0].Op != "I" || links[1].Op != "U" ||
			links[0].SealedHash != links[1].SealedHash || links[0].State == links[1].State {
			t.Fatalf("decision %d links = %+v, want insert then one state link", id, links)
		}
	}
	rep := verify(t, pool, auditchain.DecisionLedger, auditchain.Window{})
	if !rep.OK() || rep.Links != 4 {
		t.Fatalf("after state transitions: %+v", rep)
	}
}

// An identical parked repeat (the fingerprint upsert's counters only)
// appends no link: repeat_count and last_seen_at are telemetry. A repeat
// whose reason changed appends one.
func TestDecisionChainRepeatsAppendOnlyStateChanges(t *testing.T) {
	pool := freshDB(t, "chain_decision_repeat")
	id := insertDecision(t, pool, "parked", "moderate", "fp_repeat")
	for i := 0; i < 5; i++ {
		exec(t, pool, `UPDATE sage.decision SET repeat_count = repeat_count + 1,
			last_seen_at = now(), reason = 'gate reason',
			evidence = '{"budget": "index"}' WHERE id = $1`, id)
	}
	if n := linkCount(t, pool, "decision"); n != 1 {
		t.Fatalf("5 identical repeats: %d links, want the insert only", n)
	}
	exec(t, pool, `UPDATE sage.decision SET repeat_count = repeat_count + 1,
		reason = 'outside maintenance window' WHERE id = $1`, id)
	if n := linkCount(t, pool, "decision"); n != 2 {
		t.Fatalf("a repeat with a new reason: %d links, want 2", n)
	}
	if rep := verify(t, pool, auditchain.DecisionLedger, auditchain.Window{}); !rep.OK() {
		t.Fatalf("after repeats: %+v", rep.Problems)
	}
}

// 8 writers inserting decisions while 2 resolve them leave a gap-free
// chain: one link per insert and one per resolution.
func TestDecisionChainConcurrentInsertAndResolve(t *testing.T) {
	pool := freshDB(t, "chain_decision_concurrent")
	ctx := context.Background()
	const writers, perWriter = 8, 15
	var resolved atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter+64)
	done := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_, err := pool.Exec(ctx, `INSERT INTO sage.decision (feature, intent,
					verdict, risk_tier, reason, evidence_id) VALUES ('index', 'i',
					'queue_approval', 'moderate', 'r', $1)`, fmt.Sprintf("ev-%d-%d", w, i))
				errs <- err
			}
		}(w)
	}
	var rwg sync.WaitGroup
	for r := 0; r < 2; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			resolveUntil(ctx, pool, done, &resolved, errs)
		}()
	}
	wg.Wait()
	close(done)
	rwg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent decision write: %v", err)
		}
	}
	rep := verify(t, pool, auditchain.DecisionLedger, auditchain.Window{})
	want := writers*perWriter + int(resolved.Load())
	if !rep.OK() || rep.Links != want || rep.HeadSeq != int64(want) || resolved.Load() == 0 {
		t.Fatalf("%d inserts, %d resolutions: links %d head %d problems %+v",
			writers*perWriter, resolved.Load(), rep.Links, rep.HeadSeq, rep.Problems)
	}
}

// resolveUntil resolves one open decision at a time until done, counting
// the rows it resolved.
func resolveUntil(ctx context.Context, pool *pgxpool.Pool, done <-chan struct{},
	resolved *atomic.Int64, errs chan<- error) {
	for {
		select {
		case <-done:
			return
		default:
		}
		var id int64
		err := pool.QueryRow(ctx, `UPDATE sage.decision SET resolved_at = now()
			WHERE id = (SELECT id FROM sage.decision WHERE resolved_at IS NULL
			            ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING id`).Scan(&id)
		if err == nil {
			resolved.Add(1)
			continue
		}
		if err != pgx.ErrNoRows {
			errs <- err
			return
		}
	}
}

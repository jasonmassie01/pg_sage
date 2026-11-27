package pressure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// guard-01b review on a real server: an enforcement finding is resolved
// once its approval item ends (run, failed, denied or expired) and stays
// open while the item can still be approved.
func TestPressureJobProposer_SettlesFindingsOfEndedItems(t *testing.T) {
	pool, ctx := jobsPool(t)
	q := NewPGProposer(pool, nil)
	base := time.Now().UnixNano() % 1_000_000_000
	cases := []struct {
		status   string
		expires  time.Duration
		resolved bool
	}{
		{"pending", time.Hour, false},
		{"approved", time.Hour, false},
		{"scheduled", time.Hour, false},
		{"pending", -time.Minute, true},
		{"executed", time.Hour, true},
		{"failed", time.Hour, true},
		{"rejected", time.Hour, true},
		{"expired", -time.Minute, true},
	}
	findings := make([]int, len(cases))
	for i, c := range cases {
		p := Proposal{Job: Job{ID: base + int64(i) + 1, ApplicationName: "settle_job"},
			Backend: writingBackend(int32(1000 + i)), Stops: 2,
			ExpiresAt: time.Now().Add(time.Hour)}
		id, created, err := q.Propose(ctx, p)
		if err != nil || !created {
			t.Fatalf("case %d: propose = %d, %v, %v", i, id, created, err)
		}
		if err := pool.QueryRow(ctx, `UPDATE sage.action_queue SET status = $2,
			expires_at = now() + make_interval(secs => $3) WHERE id = $1
			RETURNING finding_id`, id, c.status, c.expires.Seconds()).
			Scan(&findings[i]); err != nil {
			t.Fatalf("case %d: set status %s: %v", i, c.status, err)
		}
	}
	n, err := q.Settle(ctx)
	if err != nil || n < 5 {
		t.Fatalf("settle = %d, %v; want at least the 5 ended items", n, err)
	}
	for i, c := range cases {
		if got := findingResolved(t, ctx, pool, findings[i]); got != c.resolved {
			t.Errorf("item %s (expires %s): resolved = %v, want %v", c.status, c.expires,
				got, c.resolved)
		}
	}
	if n, err := q.Settle(ctx); err != nil || n != 0 {
		t.Fatalf("a second settle = %d, %v; want nothing left to resolve", n, err)
	}
}

func findingResolved(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int) bool {
	t.Helper()
	var status string
	var resolvedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, resolved_at FROM sage.findings
		WHERE id = $1`, id).Scan(&status, &resolvedAt); err != nil {
		t.Fatalf("finding %d: %v", id, err)
	}
	if (status == "resolved") != (resolvedAt != nil) {
		t.Fatalf("finding %d is %s with resolved_at %v", id, status, resolvedAt)
	}
	return status == "resolved"
}

// settlingProposer is a fakeProposer that also settles.
type settlingProposer struct {
	*fakeProposer
	calls int
	err   error
}

func (s *settlingProposer) Settle(context.Context) (int64, error) {
	s.calls++
	return 0, s.err
}

// A settle failure reaches the tick's error; the jobs were still checked.
func TestPressureJobEnforcer_SettleErrorPropagates(t *testing.T) {
	rig := newEnforcerRig(t, enforceJob(true))
	settling := &settlingProposer{fakeProposer: rig.proposals,
		err: context.DeadlineExceeded}
	e, err := NewEnforcer(EnforcerOptions{Database: "orders", Jobs: rig.jobs,
		Pressure: rig.verdicts, Backends: rig.backends, Proposals: settling})
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	res, err := e.Tick(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tick error = %v, want the settle failure", err)
	}
	if res.Checked != 1 || settling.calls != 1 {
		t.Fatalf("checked %d, settled %d times; want 1 and 1", res.Checked, settling.calls)
	}
}

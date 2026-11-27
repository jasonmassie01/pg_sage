package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// The approval PR fixture (cli-02b): the GitOps fixture's control
// database, an operator, a viewer and an admin with code-host logins, a
// fake code host, and a queue that approves through the real action store
// (single use, content match) and counts each execution.

const prRepo = "acme/config"

// secretToken is a code-host token the tests prove never leaves the host.
const secretToken = "ghp_SECRETapprovalTOKEN0123456789abcdef"

type prFixture struct {
	*fixture
	host  *codehosttest.Fake
	queue *storeQueue
	prs   *ApprovalPRs
	root  string
	now   time.Time
}

func newPRFixture(t *testing.T) *prFixture {
	t.Helper()
	f := &prFixture{fixture: newFixture(t), host: codehosttest.New(),
		now: time.Now().UTC()}
	for _, u := range []struct{ email, role string }{{"olive@example.com", "operator"},
		{"vic@example.com", "viewer"}} {
		var id int
		if err := f.pool.QueryRow(context.Background(), `INSERT INTO sage.users
			(email, password, role) VALUES ($1, 'x', $2) RETURNING id`, u.email,
			u.role).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		f.users[u.email] = id
	}
	f.root = t.TempDir()
	f.writeUsers(t, "octo-op: olive@example.com\nview-er: vic@example.com\n"+
		"boss: carol@example.com\nghost: nobody@example.com\n")
	f.queue = &storeQueue{as: store.NewActionStore(f.pool), database: "orders"}
	f.prs = &ApprovalPRs{Host: f.host, Pool: f.pool, Queue: f.queue,
		Users:  &DirectoryUsers{Pool: f.pool, Root: f.root},
		Config: ApprovalPRConfig{Repo: prRepo, Path: "approvals/", Expiry: 72 * time.Hour},
		Now:    func() time.Time { return f.now }}
	return f
}

// writeUsers writes principals/users.yaml from "login: email" lines.
func (f *prFixture) writeUsers(t *testing.T, pairs string) {
	t.Helper()
	body := "kind: CodeHostUsers\nschema_version: 1\nusers:\n"
	for _, line := range splitLines(pairs) {
		var login, email string
		if _, err := fmt.Sscanf(line, "%s %s", &login, &email); err != nil {
			t.Fatalf("users line %q: %v", line, err)
		}
		body += fmt.Sprintf("  - login: %s\n    email: %s\n", login[:len(login)-1], email)
	}
	dir := filepath.Join(f.root, "principals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// queueChange queues one change on the fixture's database.
func (f *prFixture) queueChange(t *testing.T, sql, risk string) int {
	t.Helper()
	ctx := context.Background()
	var findingID int
	if err := f.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('approval_pr', 'warning', 'table', $1, 'pr', '{}', 'open') RETURNING id`,
		fmt.Sprintf("public.t_%d", time.Now().UnixNano())).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	id, err := f.queue.as.Propose(ctx, nil, findingID, sql, "SELECT 1", risk)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *prFixture) sync(t *testing.T) SyncResult {
	t.Helper()
	res, err := f.prs.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return res
}

func (f *prFixture) row(t *testing.T, queueID int) ApprovalPRRecord {
	t.Helper()
	rec, err := LookupApprovalPR(context.Background(), f.pool, "orders", queueID)
	if err != nil || rec == nil {
		t.Fatalf("approval PR row for %d: %+v, %v", queueID, rec, err)
	}
	return *rec
}

func (f *prFixture) status(t *testing.T, queueID int) (string, int) {
	t.Helper()
	var status string
	var by *int
	if err := f.pool.QueryRow(context.Background(), `SELECT status, decided_by
		FROM sage.action_queue WHERE id = $1`, queueID).Scan(&status, &by); err != nil {
		t.Fatal(err)
	}
	if by == nil {
		return status, 0
	}
	return status, *by
}

// audits counts the transition audit entries of one change, by new state.
func (f *prFixture) audits(t *testing.T, queueID int) map[string]int {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT detail->>'to', count(*)
		FROM sage.auth_audit WHERE event = $1 AND detail->>'database' = 'orders'
		  AND (detail->>'queue_id')::int = $2 GROUP BY 1`, AuditApprovalPR, queueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var to string
		var n int
		if err := rows.Scan(&to, &n); err != nil {
			t.Fatal(err)
		}
		out[to] = n
	}
	return out
}

// storeQueue is an ApprovalQueue over the real action store on one
// database: Approve is the store's single-use, content-matched approval,
// and runs counts the executions that followed an approval.
type storeQueue struct {
	as       *store.ActionStore
	database string
	mu       sync.Mutex
	runs     int
	approver []int
}

func (q *storeQueue) item(a store.QueuedAction) ApprovalItem {
	return ApprovalItem{Tier: policy.RiskTier(a.ActionRisk), Card: approvalcard.Card{
		QueueID: a.ID, Database: q.database, Title: "change " + a.ActionRisk,
		Status: a.Status, SQL: a.ProposedSQL, CardHash: approvalcard.ContentHash(a),
		Rollback: approvalcard.Rollback{SQL: a.RollbackSQL}, ExpiresAt: a.ExpiresAt,
		Risk: approvalcard.Risk{Tier: a.ActionRisk}}}
}

func (q *storeQueue) Pending(ctx context.Context) ([]ApprovalItem, error) {
	list, err := q.as.ListPending(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ApprovalItem, 0, len(list))
	for _, a := range list {
		out = append(out, q.item(a))
	}
	return out, nil
}

func (q *storeQueue) Item(ctx context.Context, database string, id int) (ApprovalItem,
	error) {
	if database != q.database {
		return ApprovalItem{}, ErrItemNotFound
	}
	a, err := q.as.GetByID(ctx, id)
	if err != nil {
		return ApprovalItem{}, ErrItemNotFound
	}
	return q.item(*a), nil
}

func (q *storeQueue) Approve(ctx context.Context, database string, id, userID int,
	expectSQL string) (string, error) {
	if _, err := q.as.ApproveExpecting(ctx, id, userID, expectSQL); err != nil {
		if a, getErr := q.as.GetByID(ctx, id); getErr == nil && a.Status != "pending" {
			return "", fmt.Errorf("%w (%s)", ErrAlreadyDecided, a.Status)
		}
		return "", err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.runs++
	q.approver = append(q.approver, userID)
	return "ran (verification: monitoring)", nil
}

func (q *storeQueue) Reject(ctx context.Context, database string, id, userID int,
	reason string) error {
	if err := q.as.Reject(ctx, id, userID, reason); err != nil {
		return fmt.Errorf("%w: %v", ErrAlreadyDecided, err)
	}
	return nil
}

func (q *storeQueue) executions() (int, []int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.runs, append([]int(nil), q.approver...)
}

// checkOpenedPR checks the one PR opened for change id: its file and label.
func (f *prFixture) checkOpenedPR(t *testing.T, id int) {
	t.Helper()
	opened := f.host.Opened()
	if len(opened) != 1 {
		t.Fatalf("host received %d PRs, want 1", len(opened))
	}
	pr := opened[0]
	wantPath := fmt.Sprintf("approvals/orders/%d.yaml", id)
	if len(pr.Files) != 1 || pr.Files[0].Path != wantPath {
		t.Fatalf("PR files = %+v, want one file %s", pr.Files, wantPath)
	}
	for _, want := range []string{"kind: ApprovalRequest", "card_hash: ",
		"sql: ANALYZE public.orders", "rollback: SELECT 1", "risk_tier: safe",
		"expires_at: "} {
		if !strings.Contains(pr.Files[0].Content, want) {
			t.Errorf("approval file lacks %q:\n%s", want, pr.Files[0].Content)
		}
	}
	if len(pr.Labels) != 1 || pr.Labels[0] != ApprovalPRLabel {
		t.Fatalf("labels = %v, want [%s]", pr.Labels, ApprovalPRLabel)
	}
}

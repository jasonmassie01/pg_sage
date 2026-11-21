package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/verify"
)

// cli-02b unit tests: the login map file, the approval file, the status
// a failure is shown as, and the retry backoff. No concurrent access
// tests here: these functions are pure (the DB tests race a merge).

func writeUsersFile(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "principals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadCodeHostUsers_MapsLoginsCaseInsensitively(t *testing.T) {
	root := writeUsersFile(t, "kind: CodeHostUsers\nschema_version: 1\nusers:\n"+
		"  - login: Octo-DBA\n    email: Alice@Example.com\n")
	logins, err := LoadCodeHostUsers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(logins) != 1 || logins["octo-dba"] != "alice@example.com" {
		t.Fatalf("logins = %v, want octo-dba -> alice@example.com", logins)
	}
}

func TestLoadCodeHostUsers_RefusesInvalidFiles(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"wrong kind": {"kind: Principal\nschema_version: 1\nusers: []\n",
			"kind CodeHostUsers"},
		"wrong version": {"kind: CodeHostUsers\nschema_version: 2\nusers: []\n",
			"schema_version 1"},
		"duplicate login": {"kind: CodeHostUsers\nschema_version: 1\nusers:\n" +
			"  - {login: a, email: x@example.com}\n  - {login: A, email: y@example.com}\n",
			"listed twice"},
		"empty email": {"kind: CodeHostUsers\nschema_version: 1\nusers:\n" +
			"  - {login: a, email: ''}\n", "login and email"},
		"unknown key": {"kind: CodeHostUsers\nschema_version: 1\nusers:\n" +
			"  - {login: a, email: x@example.com, role: admin}\n", "role"},
		"not yaml": {"kind: [\n", "principals/users.yaml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadCodeHostUsers(writeUsersFile(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadCodeHostUsers_MissingFileOrRootIsEmptyMapError(t *testing.T) {
	if _, err := LoadCodeHostUsers(t.TempDir()); !errors.Is(err, ErrUnmappedLogin) {
		t.Fatalf("missing users.yaml: err = %v, want ErrUnmappedLogin", err)
	}
	if _, err := LoadCodeHostUsers(""); !errors.Is(err, ErrUnmappedLogin) {
		t.Fatalf("empty root: err = %v, want ErrUnmappedLogin", err)
	}
}

func TestDirectoryUsers_NilPoolAndUnmappedLoginFailClosed(t *testing.T) {
	root := writeUsersFile(t, "kind: CodeHostUsers\nschema_version: 1\nusers:\n"+
		"  - {login: a, email: x@example.com}\n")
	u := &DirectoryUsers{Root: root}
	if _, err := u.Resolve(context.Background(), "a", "orders"); err == nil {
		t.Fatal("a resolver without a pool resolved a user")
	}
	if _, err := u.Resolve(context.Background(), "b", "orders"); !errors.Is(err,
		ErrUnmappedLogin) {
		t.Fatalf("unmapped login: err = %v, want ErrUnmappedLogin", err)
	}
	if _, err := u.Resolve(context.Background(), "", "orders"); !errors.Is(err,
		ErrUnmappedLogin) {
		t.Fatalf("empty login: err = %v, want ErrUnmappedLogin", err)
	}
}

func TestApprovalFile_HoldsTheCardAndChangesWithIt(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	c := approvalcard.Card{QueueID: 7, Database: "orders", Title: "Analyze orders",
		SQL: "ANALYZE public.orders", CardHash: "abc",
		Rollback: approvalcard.Rollback{Class: "none", SQL: ""},
		Risk:     approvalcard.Risk{Tier: "safe"},
		Ladder: []verify.LadderRecord{{Tier: 1, Name: "clone", Result: "passed",
			At: at}}}
	body, err := renderApprovalFile(ApprovalItem{Card: c, Tier: "safe"}, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: ApprovalRequest", "schema_version: 1",
		"database: orders", "queue_id: 7", "card_hash: abc", "risk_tier: safe",
		"sql: ANALYZE public.orders", "name: clone", "result: passed",
		"expires_at: 2026-11-20T10:00:00Z"} {
		if !strings.Contains(body, want) {
			t.Errorf("file lacks %q:\n%s", want, body)
		}
	}
	c.SQL = "ANALYZE public.billing"
	other, err := renderApprovalFile(ApprovalItem{Card: c, Tier: "safe"}, at)
	if err != nil || fileHash(other) == fileHash(body) {
		t.Fatalf("a different SQL gave the same file hash (%v)", err)
	}
}

func TestApprovalPRPath_RefusesUnsafeDatabaseNames(t *testing.T) {
	cfg := ApprovalPRConfig{Path: "approvals/"}
	if p, err := cfg.filePath("orders", 7); err != nil || p != "approvals/orders/7.yaml" {
		t.Fatalf("path = %q, %v", p, err)
	}
	for _, db := range []string{"", "..", "a/b", `a\b`} {
		if _, err := cfg.filePath(db, 7); err == nil {
			t.Errorf("database %q gave a path", db)
		}
	}
	if _, err := cfg.filePath("orders", 0); err == nil {
		t.Error("queue id 0 gave a path")
	}
	if p, _ := (ApprovalPRConfig{}).filePath("orders", 3); p != "approvals/orders/3.yaml" {
		t.Errorf("empty path setting gave %q, want the approvals/ default", p)
	}
}

func TestOpenFailureStatus_NamesTheHTTPStatusOrClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: x: HTTP 503: down", codehost.ErrUnavailable), "HTTP 503"},
		{fmt.Errorf("%w: token cannot write (HTTP 401)", codehost.ErrForbidden), "HTTP 401"},
		{&codehost.RateLimitError{Host: "h", RetryAfter: time.Minute}, "rate limited"},
		{fmt.Errorf("%w: dial tcp: refused", codehost.ErrUnavailable), "code host unavailable"},
		{fmt.Errorf("%w: bad branch", codehost.ErrInvalid), "request refused"},
		{errors.New("other"), "error"},
	}
	for _, tc := range cases {
		if got := openFailureStatus(tc.err); got != tc.want {
			t.Errorf("openFailureStatus(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestRetryBackoff_GrowsAndIsCapped(t *testing.T) {
	if retryBackoff(0) != 30*time.Second || retryBackoff(1) != 30*time.Second {
		t.Fatalf("first backoff = %s, want 30s", retryBackoff(1))
	}
	if retryBackoff(2) != time.Minute || retryBackoff(3) != 2*time.Minute {
		t.Fatalf("backoff does not double: %s, %s", retryBackoff(2), retryBackoff(3))
	}
	for _, n := range []int{7, 8, 40, 1 << 20} {
		if got := retryBackoff(n); got != 30*time.Minute {
			t.Fatalf("retryBackoff(%d) = %s, want the 30m cap", n, got)
		}
	}
}

func TestApprovalPRs_NilPartsDoNothing(t *testing.T) {
	var p *ApprovalPRs
	if res, err := p.Sync(context.Background()); err != nil || res != (SyncResult{}) {
		t.Fatalf("nil Sync = %+v, %v", res, err)
	}
	if st := p.Status(); st.Enabled {
		t.Fatalf("nil Status() = %+v, want disabled", st)
	}
	p = &ApprovalPRs{Config: ApprovalPRConfig{Repo: "o/r"}}
	if _, err := p.Sync(context.Background()); err == nil {
		t.Fatal("Sync without a host, pool or queue did not fail")
	}
}

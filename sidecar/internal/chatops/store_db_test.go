package chatops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The approver is an authenticated chat user mapped explicitly to a
// pg_sage user; unmapped users are refused, the mapped user's current
// role decides, and a callback is processed at most once.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/chatops"))
}

func liveStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return NewStore(pool), pool, ctx
}

func newUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) int {
	t.Helper()
	id, err := auth.CreateUser(ctx, pool, fmt.Sprintf("%s-%d@test.local", role,
		time.Now().UnixNano()), "correct horse battery", role)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func TestIdentityLinkResolveUnlink(t *testing.T) {
	s, pool, ctx := liveStore(t)
	user := newUser(t, ctx, pool, auth.RoleOperator)
	admin := newUser(t, ctx, pool, auth.RoleAdmin)
	got, err := s.Link(ctx, Identity{Provider: ProviderSlack, TeamID: "T1",
		ExternalUserID: "U42", UserID: user, CreatedBy: admin})
	if err != nil || got.ID <= 0 || got.UserID != user || got.CreatedAt.IsZero() {
		t.Fatalf("link = %+v, %v", got, err)
	}
	u, err := s.Resolve(ctx, ProviderSlack, "T1", "U42")
	if err != nil || u.ID != user || u.Role != auth.RoleOperator {
		t.Fatalf("resolve = %+v, %v", u, err)
	}
	for name, key := range map[string][3]string{
		"other team":     {ProviderSlack, "T2", "U42"},
		"other user":     {ProviderSlack, "T1", "U43"},
		"other provider": {ProviderTelegram, "T1", "U42"},
	} {
		if _, err := s.Resolve(ctx, key[0], key[1], key[2]); !errors.Is(err, ErrUnmapped) {
			t.Errorf("%s: resolve = %v, want ErrUnmapped", name, err)
		}
	}
	list, err := s.List(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.Unlink(ctx, got.ID); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := s.Resolve(ctx, ProviderSlack, "T1", "U42"); !errors.Is(err, ErrUnmapped) {
		t.Fatalf("resolve after unlink = %v, want ErrUnmapped", err)
	}
	if err := s.Unlink(ctx, got.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unlink = %v, want ErrNotFound", err)
	}
}

func TestIdentityLinkValidates(t *testing.T) {
	s, pool, ctx := liveStore(t)
	user := newUser(t, ctx, pool, auth.RoleOperator)
	for name, id := range map[string]Identity{
		"unknown provider": {Provider: "irc", ExternalUserID: "x", UserID: user},
		"blank user id":    {Provider: ProviderSlack, TeamID: "T1", UserID: user},
		"slack w/o team":   {Provider: ProviderSlack, ExternalUserID: "U1", UserID: user},
		"telegram team":    {Provider: ProviderTelegram, TeamID: "T1", ExternalUserID: "1", UserID: user},
		"no such user":     {Provider: ProviderSlack, TeamID: "T1", ExternalUserID: "U9", UserID: 999999},
		"oversized id": {Provider: ProviderSlack, TeamID: "T1",
			ExternalUserID: string(make([]byte, 300)), UserID: user},
	} {
		if _, err := s.Link(ctx, id); !errors.Is(err, ErrInvalidIdentity) {
			t.Errorf("%s: link = %v, want ErrInvalidIdentity", name, err)
		}
	}
	first := Identity{Provider: ProviderTelegram, ExternalUserID: "555", UserID: user}
	if _, err := s.Link(ctx, first); err != nil {
		t.Fatalf("link: %v", err)
	}
	other := newUser(t, ctx, pool, auth.RoleAdmin)
	first.UserID = other
	if _, err := s.Link(ctx, first); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("relinking a chat user to another account = %v, want ErrIdentityConflict",
			err)
	}
}

// The role at approval time decides, and deleting the account removes
// its chat identity.
func TestResolveReadsTheCurrentAccount(t *testing.T) {
	s, pool, ctx := liveStore(t)
	user := newUser(t, ctx, pool, auth.RoleOperator)
	if _, err := s.Link(ctx, Identity{Provider: ProviderTelegram, ExternalUserID: "777",
		UserID: user}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.users SET role = 'viewer' WHERE id = $1`,
		user); err != nil {
		t.Fatalf("demote: %v", err)
	}
	u, err := s.Resolve(ctx, ProviderTelegram, "", "777")
	if err != nil || u.Role != auth.RoleViewer {
		t.Fatalf("resolve after demotion = %+v, %v", u, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.Resolve(ctx, ProviderTelegram, "", "777"); !errors.Is(err, ErrUnmapped) {
		t.Fatalf("resolve after the account is gone = %v, want ErrUnmapped", err)
	}
}

func TestMarkSeenRefusesReplays(t *testing.T) {
	s, pool, ctx := liveStore(t)
	nonce := fmt.Sprintf("T1:U1:%d", time.Now().UnixNano())
	if err := s.MarkSeen(ctx, ProviderSlack, nonce); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.MarkSeen(ctx, ProviderSlack, nonce); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay = %v, want ErrReplay", err)
	}
	if err := s.MarkSeen(ctx, ProviderTelegram, nonce); err != nil {
		t.Fatalf("same nonce from another provider: %v", err)
	}
	if err := s.MarkSeen(ctx, ProviderSlack, ""); !errors.Is(err, ErrMalformed) {
		t.Fatalf("empty nonce = %v, want ErrMalformed", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.chatops_replay (provider, nonce, seen_at)
		VALUES ('slack', 'ancient', now() - interval '3 days')`); err != nil {
		t.Fatalf("seed old nonce: %v", err)
	}
	if err := s.MarkSeen(ctx, ProviderSlack, nonce+"-next"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	var old int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.chatops_replay
		WHERE nonce = 'ancient'`).Scan(&old)
	if old != 0 {
		t.Fatal("nonces older than the replay window are never pruned")
	}
}

func TestMarkSeenConcurrentDeliveriesProcessOnce(t *testing.T) {
	s, _, ctx := liveStore(t)
	nonce := fmt.Sprintf("update:%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.MarkSeen(ctx, ProviderTelegram, nonce) }()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrReplay):
			t.Fatalf("delivery = %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d deliveries processed, want 1", ok)
	}
}

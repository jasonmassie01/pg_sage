package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-02 on a real session store: valid sessions pass, unknown ones are
// 401, and an outage of the store mid-session is 503 until it returns, after
// which the same cookie works with no new login.

// cutProxy forwards TCP to the database and can drop every connection and
// refuse new ones, the way a restarting or failing-over server does.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	down   bool
	conns  map[net.Conn]struct{}
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &cutProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { _ = ln.Close(); p.cut(true) })
	go p.serve()
	return p
}

func (p *cutProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.forward(c)
	}
}

func (p *cutProxy) forward(c net.Conn) {
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	up, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.conns[c], p.conns[up] = struct{}{}, struct{}{}
	p.mu.Unlock()
	go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
	_, _ = io.Copy(c, up)
	_ = c.Close()
}

// cut sets the outage state; going down closes every live connection.
func (p *cutProxy) cut(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if !down {
		return
	}
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func proxiedPool(t *testing.T, direct *pgxpool.Pool) (*pgxpool.Pool, *cutProxy) {
	t.Helper()
	cfg := direct.Config().Copy()
	target := net.JoinHostPort(cfg.ConnConfig.Host,
		strconv.Itoa(int(cfg.ConnConfig.Port)))
	proxy := newCutProxy(t, target)
	host, port, _ := net.SplitHostPort(proxy.ln.Addr().String())
	cfg.ConnConfig.Host = host
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatalf("proxy port %q: %v", port, err)
	}
	cfg.ConnConfig.Port = uint16(n)
	cfg.ConnConfig.ConnectTimeout = 2 * time.Second
	cfg.ConnConfig.Fallbacks = nil
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("proxied pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, proxy
}

func echoUser() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]string{"email": UserFromContext(r.Context()).Email})
	})
}

func sessionFor(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM sage.users WHERE email = $1", email); err != nil {
		t.Fatalf("clearing %s: %v", email, err)
	}
	uid, err := auth.CreateUser(ctx, pool, email, "local-disposable-fixture-only",
		auth.RoleViewer)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok, err := auth.CreateSession(ctx, pool, uid)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return tok
}

func TestSessionAuth_DBUnknownAndValidSessions(t *testing.T) {
	pool := surfacePool(t)
	captureSessionStoreLog(t, nil)
	tok := sessionFor(t, pool, "store-valid@fixture.invalid")
	h := SessionAuthMiddleware(pool)(echoUser())
	if w := callWithCookie(h, "/api/v1/auth/me", tok); w.Code != http.StatusOK ||
		!jsonHas(w.Body.Bytes(), "email", "store-valid@fixture.invalid") {
		t.Fatalf("valid session = %d %s, want 200 with the user", w.Code, w.Body.String())
	}
	assertUnauthorized(t, callWithCookie(h, "/api/v1/auth/me",
		wellFormedSessionCookie(t)), "session invalid or expired")
	assertUnauthorized(t, callWithCookie(h, "/api/v1/auth/me", "'; DROP"),
		"session invalid or expired")
	if _, err := pool.Exec(context.Background(), `UPDATE sage.sessions
		SET expires_at = now() - interval '1 second'
		WHERE token_hash = $1`, auth.HashSessionToken(tok)); err != nil {
		t.Fatalf("expiring session: %v", err)
	}
	assertUnauthorized(t, callWithCookie(h, "/api/v1/auth/me", tok),
		"session invalid or expired")
}

func TestSessionAuth_StoreOutageMidSessionKeepsTheSession(t *testing.T) {
	direct := surfacePool(t)
	sink := captureSessionStoreLog(t, nil)
	tok := sessionFor(t, direct, "store-outage@fixture.invalid")
	pool, proxy := proxiedPool(t, direct)
	h := SessionAuthMiddleware(pool)(echoUser())
	if w := callWithCookie(h, "/api/v1/findings", tok); w.Code != http.StatusOK {
		t.Fatalf("before the outage = %d %s, want 200", w.Code, w.Body.String())
	}

	proxy.cut(true)
	for i := 0; i < 5; i++ { // the dashboard keeps polling through the outage
		assertStoreUnavailable(t, callWithCookie(h, "/api/v1/findings", tok))
	}
	if sink.String() == "" {
		t.Fatal("the outage was not logged")
	}

	proxy.cut(false)
	deadline := time.Now().Add(15 * time.Second)
	for {
		w := callWithCookie(h, "/api/v1/findings", tok)
		if w.Code == http.StatusOK {
			if !jsonHas(w.Body.Bytes(), "email", "store-outage@fixture.invalid") {
				t.Fatalf("recovered as the wrong user: %s", w.Body.String())
			}
			return
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("during recovery = %d %s; only 503 or 200 keep the user signed in",
				w.Code, w.Body.String())
		}
		if time.Now().After(deadline) {
			t.Fatal("the same cookie never worked again after the store returned")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func jsonHas(raw []byte, key, want string) bool {
	var body map[string]string
	return json.Unmarshal(raw, &body) == nil && body[key] == want
}

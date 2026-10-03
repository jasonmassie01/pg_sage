//go:build perfgate

package main

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// perfEndpoints are the API list endpoints the dashboard polls.
var perfEndpoints = []string{
	"/api/v1/findings", "/api/v1/cases", "/api/v1/actions",
	"/api/v1/investigations", "/api/v1/value", "/api/v1/incidents",
	"/api/v1/recommendations",
}

// perfLogCapture tees the runtime's log output (stderr) and keeps the
// lines that report a statement cut off by a timeout: pg_stat_statements
// never records a cancelled statement, so the log is the only witness.
type perfLogCapture struct {
	mu       sync.Mutex
	timeouts []string
}

var perfTimeoutMarkers = []string{
	"statement timeout", "canceling statement", "context deadline exceeded",
	"lock timeout",
}

func capturePerfLogs(t *testing.T) *perfLogCapture {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("log pipe: %v", err)
	}
	real := os.Stderr
	os.Stderr = w
	c := &perfLogCapture{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(real, line)
			c.observe(line)
		}
	}()
	t.Cleanup(func() {
		os.Stderr = real
		_ = w.Close()
		<-done
		_ = r.Close()
	})
	return c
}

func (c *perfLogCapture) observe(line string) {
	if !strings.Contains(line, "[WARN]") && !strings.Contains(line, "[ERROR]") {
		return
	}
	lower := strings.ToLower(line)
	for _, marker := range perfTimeoutMarkers {
		if strings.Contains(lower, marker) {
			c.mu.Lock()
			c.timeouts = append(c.timeouts, perfLogMessage(line))
			c.mu.Unlock()
			return
		}
	}
}

// perfLogMessage drops the timestamp so repeated lines deduplicate.
func perfLogMessage(line string) string {
	if i := strings.Index(line, " ["); i > 0 {
		return line[i+1:]
	}
	return line
}

func (c *perfLogCapture) drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.timeouts
	c.timeouts = nil
	return out
}

// perfAPISession creates an admin and a session before measuring starts.
func perfAPISession(t *testing.T, ctx context.Context, harness *pgxpool.Pool) string {
	t.Helper()
	id, err := auth.CreateUser(ctx, harness, "perfgate@test.local",
		"perfgate-not-a-secret-123", "admin")
	if err != nil {
		t.Fatalf("create API user: %v", err)
	}
	session, err := auth.CreateSession(ctx, harness, id)
	if err != nil {
		t.Fatalf("create API session: %v", err)
	}
	return session
}

// callPerfEndpoints calls every list endpoint through the production
// router wiring and records its status and latency.
func callPerfEndpoints(t *testing.T, session string) []perfgate.Endpoint {
	t.Helper()
	router := wireRouter(WireParams{Cfg: cfg, Pool: pool, FleetMgr: fleetMgr,
		Actions: struct {
			Store    *store.ActionStore
			Executor *executor.Executor
		}{Store: actionStore, Executor: exec}}).Handler
	out := make([]perfgate.Endpoint, 0, len(perfEndpoints))
	for _, path := range perfEndpoints {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "sage_session", Value: session})
		w := httptest.NewRecorder()
		start := time.Now()
		router.ServeHTTP(w, req)
		out = append(out, perfgate.Endpoint{Path: path, Status: w.Code,
			Duration: time.Since(start)})
	}
	return out
}

// startPerfWorkload runs a light application workload on the hot tables
// so the collector, query store and analyzer have live statements to
// process. Its statements carry perfgate.WorkloadTag and are excluded.
func startPerfWorkload(t *testing.T, dsn string, hot []string) func() {
	t.Helper()
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("workload dsn: %v", err)
	}
	pc.MaxConns = 2
	pc.ConnConfig.RuntimeParams["application_name"] = "perfgate_workload"
	wp, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("workload pool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPerfWorkload(ctx, t, wp, hot)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() { cancel(); <-done; wp.Close() })
	}
	t.Cleanup(stop)
	return stop
}

func runPerfWorkload(ctx context.Context, t *testing.T, wp *pgxpool.Pool, hot []string) {
	tag := "/* " + perfgate.WorkloadTag + " */ "
	shapes := []string{
		tag + "SELECT count(*) FROM %s WHERE tenant_id = $1",
		tag + "SELECT id, payload FROM %s WHERE created_at > now() - interval '1 hour' " +
			"AND tenant_id = $1 ORDER BY created_at DESC LIMIT 10",
		tag + "UPDATE %s SET payload = md5(random()::text) WHERE id = $1",
		tag + "INSERT INTO %s (tenant_id, payload) VALUES ($1, 'w')",
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		table := hot[rand.IntN(len(hot))]
		shape := shapes[rand.IntN(len(shapes))]
		_, err := wp.Exec(ctx, fmt.Sprintf(shape, table), rand.IntN(100)+1)
		if err != nil && ctx.Err() == nil {
			t.Errorf("workload statement on %s: %v", table, err)
			return
		}
	}
}

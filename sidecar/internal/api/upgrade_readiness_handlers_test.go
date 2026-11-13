package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// fakeUpgrade records the run it was asked for and answers with rep/err.
type fakeUpgrade struct {
	mu     sync.Mutex
	calls  int
	target upgradecheck.Target
	opts   upgradecheck.Options
	rep    upgradecheck.Report
	err    error
}

func (f *fakeUpgrade) run(_ context.Context, _ *pgxpool.Pool, t upgradecheck.Target,
	o upgradecheck.Options) (upgradecheck.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.target, f.opts = t, o
	return f.rep, f.err
}

var stubPool = new(pgxpool.Pool) // never dialled: the fake runner ignores it

func upgradeDeps(f *fakeUpgrade) upgradeReadinessDeps {
	return upgradeReadinessDeps{
		pool: func(db string) (*pgxpool.Pool, error) {
			switch db {
			case "", "prod":
				return stubPool, nil
			case "ghost":
				return nil, errLintUnknownDatabase
			}
			return nil, errLintNoDatabase
		},
		member: func(name string) (*pgxpool.Pool, error) {
			if name == "pg18" {
				return stubPool, nil
			}
			return nil, errUnknownTargetDatabase
		},
		provider: func(db string) string {
			if db == "prod" {
				return "rds"
			}
			return "aurora"
		},
		run:   f.run,
		slots: make(chan struct{}, 1),
	}
}

func getUpgrade(t *testing.T, d upgradeReadinessDeps, query string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, upgradeReadinessPath+"?"+query, nil)
	upgradeReadinessHandler(d).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestUpgradeReadiness_InvalidInputIs400(t *testing.T) {
	f := &fakeUpgrade{}
	cases := map[string]string{
		"":                                      "target_major is required",
		"target_major=":                         "target_major is required",
		"target_major=eighteen":                 "must be an integer",
		"target_major=19":                       "newer than pg_sage knows",
		"target_major=0":                        "positive major",
		"target_major=18&method=dump":           "method",
		"target_major=18&database=a%3Bb":        "database",
		"target_major=18&target_database=x%20y": "target_database",
	}
	for query, want := range cases {
		code, body := getUpgrade(t, upgradeDeps(f), query)
		if code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Fatalf("%q = %d %s, want 400 naming %q", query, code, body, want)
		}
	}
	if f.calls != 0 {
		t.Fatalf("invalid input reached the runner %d times", f.calls)
	}
}

func TestUpgradeReadiness_TargetAtOrBelowServerIs400(t *testing.T) {
	f := &fakeUpgrade{err: fmt.Errorf("%w: target_major 16 must be above the server's "+
		"major 16", upgradecheck.ErrInvalidTarget)}
	code, body := getUpgrade(t, upgradeDeps(f), "target_major=16")
	if code != http.StatusBadRequest || !strings.Contains(body, "must be above") {
		t.Fatalf("at-or-below = %d %s, want 400", code, body)
	}
}

func TestUpgradeReadiness_HappyPathPassesTargetAndProvider(t *testing.T) {
	f := &fakeUpgrade{rep: upgradecheck.Report{SourceMajor: 14, Checks: []upgradecheck.Check{
		{ID: "U4", Status: upgradecheck.Block, Rows: []upgradecheck.Row{}}}}}
	code, body := getUpgrade(t, upgradeDeps(f),
		"database=prod&target_major=18&target_database=pg18")
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, body)
	}
	if f.target != (upgradecheck.Target{Major: 18, Method: "pg_upgrade", Provider: "rds"}) {
		t.Fatalf("target = %+v, want 18 via pg_upgrade on rds", f.target)
	}
	if f.opts.TargetDB != stubPool || f.opts.TargetDatabase != "pg18" {
		t.Fatalf("options = %+v, want the pg18 member", f.opts)
	}
	var rep upgradecheck.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c, ok := rep.Check("U4"); !ok || c.Status != upgradecheck.Block {
		t.Fatalf("body = %s, want U4 block", body)
	}
}

func TestUpgradeReadiness_DatabaseResolution(t *testing.T) {
	f := &fakeUpgrade{}
	cases := []struct {
		query string
		code  int
	}{
		{"database=ghost&target_major=18", http.StatusNotFound},
		{"database=down&target_major=18", http.StatusServiceUnavailable},
		{"target_major=18&target_database=nope", http.StatusNotFound},
	}
	for _, c := range cases {
		if code, body := getUpgrade(t, upgradeDeps(f), c.query); code != c.code {
			t.Fatalf("%s = %d %s, want %d", c.query, code, body, c.code)
		}
	}
	if f.calls != 0 {
		t.Fatalf("unresolved databases reached the runner %d times", f.calls)
	}
}

func TestUpgradeReadiness_RunErrorIs503(t *testing.T) {
	f := &fakeUpgrade{err: errors.New("upgrade readiness: begin read-only transaction: " +
		"connection refused")}
	code, body := getUpgrade(t, upgradeDeps(f), "target_major=18")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "connection refused") {
		t.Fatalf("run error = %d %s, want 503 with the cause", code, body)
	}
}

func TestUpgradeReadiness_RequestEndsWhileWaitingForSlot(t *testing.T) {
	d := upgradeDeps(&fakeUpgrade{})
	d.slots <- struct{}{} // the only slot is taken
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, upgradeReadinessPath+"?target_major=18", nil)
	upgradeReadinessHandler(d).ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(),
		"waiting for a slot") {
		t.Fatalf("busy = %d %s, want 503", rec.Code, rec.Body.String())
	}
}

// CHECK-02 on a real server: target 18 returns U4 block exactly when the
// server's data checksums are off.
func TestUpgradeReadiness_LiveChecksumsBlock(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var versionNum int
	var checksums string
	if err := pool.QueryRow(context.Background(), `SELECT
		current_setting('server_version_num')::int, current_setting('data_checksums')`).
		Scan(&versionNum, &checksums); err != nil {
		t.Fatalf("server: %v", err)
	}
	d := upgradeDeps(&fakeUpgrade{})
	d.run = upgradecheck.Run
	d.pool = func(string) (*pgxpool.Pool, error) { return pool, nil }
	code, body := getUpgrade(t, d, "database=x&target_major=18")
	if versionNum/10000 >= 18 {
		if code != http.StatusBadRequest {
			t.Fatalf("PG18 server = %d %s, want 400: 18 is not above it", code, body)
		}
		return
	}
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, body)
	}
	var rep upgradecheck.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u4, _ := rep.Check("U4")
	if (u4.Status == upgradecheck.Block) != (checksums == "off") || len(rep.Checks) != 7 {
		t.Fatalf("checksums %s: U4 = %+v (%d checks)", checksums, u4, len(rep.Checks))
	}
}

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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// drill-02a: GET /api/v1/databases/{db}/upgrade-brief builds the brief
// read-only and stores it only when it changed. No DB test here: the
// build and the store are upgradecheck's (brief_db_test.go).

type fakeBrief struct {
	mu       sync.Mutex
	builds   int
	stores   int
	target   upgradecheck.Target
	opts     upgradecheck.BriefOptions
	brief    upgradecheck.Brief
	buildErr error
	stored   upgradecheck.StoredBrief
	storeErr error
}

func (f *fakeBrief) build(_ context.Context, _ *pgxpool.Pool, t upgradecheck.Target,
	o upgradecheck.BriefOptions) (upgradecheck.Brief, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds++
	f.target, f.opts = t, o
	return f.brief, f.buildErr
}

func (f *fakeBrief) store(_ context.Context, _ *pgxpool.Pool,
	b upgradecheck.Brief) (upgradecheck.StoredBrief, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stores++
	return f.stored, f.storeErr
}

func briefDeps(f *fakeBrief) upgradeBriefDeps {
	up := upgradeDeps(&fakeUpgrade{})
	return upgradeBriefDeps{pool: up.pool, member: up.member, provider: up.provider,
		options: func(_ context.Context, db string, _ *pgxpool.Pool) upgradecheck.BriefOptions {
			return upgradecheck.BriefOptions{SlotConsumers: map[string]string{"cdc": db}}
		},
		build: f.build, store: f.store, slots: make(chan struct{}, 1)}
}

func getBrief(t *testing.T, d upgradeBriefDeps, db, query string) (int, string,
	http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/databases/"+db+"/upgrade-brief?"+
		query, nil)
	req.SetPathValue("db", db)
	upgradeBriefHandler(d).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

func TestUpgradeBrief_InvalidInputIs400(t *testing.T) {
	f := &fakeBrief{}
	cases := map[string][2]string{
		"bad database":    {"a;b", ""},
		"all":             {"all", ""},
		"major not int":   {"prod", "target_major=eighteen"},
		"major too new":   {"prod", "target_major=99"},
		"major zero":      {"prod", "target_major=0"},
		"bad method":      {"prod", "method=dump"},
		"bad target name": {"prod", "target_database=x%20y"},
	}
	for name, c := range cases {
		code, body, _ := getBrief(t, briefDeps(f), c[0], c[1])
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s, want 400", name, code, body)
		}
	}
	if f.builds != 0 || f.stores != 0 {
		t.Fatalf("invalid input reached the builder %d / store %d times", f.builds, f.stores)
	}
}

func TestUpgradeBrief_HappyPathBuildsStoresAndReturns(t *testing.T) {
	builtAt := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	f := &fakeBrief{brief: upgradecheck.Brief{Database: "prod", Hash: strings.Repeat("a", 64),
		Checklist: []upgradecheck.ChecklistStep{{Order: 1, Step: "resolve_blockers"}}},
		stored: upgradecheck.StoredBrief{ID: 7, Stored: true, BuiltAt: builtAt}}
	code, body, _ := getBrief(t, briefDeps(f), "prod", "target_database=pg18&method=logical")
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, body)
	}
	if f.target != (upgradecheck.Target{Method: "logical", Provider: "rds"}) {
		t.Fatalf("target = %+v, want the default major via logical on rds", f.target)
	}
	if f.opts.TargetDB != stubPool || f.opts.TargetDatabase != "pg18" ||
		f.opts.SlotConsumers["cdc"] != "prod" {
		t.Fatalf("options = %+v, want the pg18 member and prod's consumers", f.opts)
	}
	var got upgradeBriefView
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if got.Database != "prod" || got.BriefID != 7 || !got.Stored ||
		!got.StoredAt.Equal(builtAt) || len(got.Checklist) != 1 || got.StoreError != "" {
		t.Fatalf("view = %+v", got)
	}
}

func TestUpgradeBrief_ExplicitTargetMajorIsPassed(t *testing.T) {
	f := &fakeBrief{}
	if code, body, _ := getBrief(t, briefDeps(f), "prod", "target_major=17"); code != 200 {
		t.Fatalf("status = %d %s", code, body)
	}
	if f.target.Major != 17 || f.target.Method != upgradecheck.PGUpgrade {
		t.Fatalf("target = %+v, want 17 via pg_upgrade", f.target)
	}
}

func TestUpgradeBrief_DatabaseResolution(t *testing.T) {
	f := &fakeBrief{}
	for _, c := range []struct {
		db, query string
		code      int
	}{
		{"ghost", "", http.StatusNotFound},
		{"down", "", http.StatusServiceUnavailable},
		{"prod", "target_database=nope", http.StatusNotFound},
	} {
		if code, body, _ := getBrief(t, briefDeps(f), c.db, c.query); code != c.code {
			t.Fatalf("%s?%s = %d %s, want %d", c.db, c.query, code, body, c.code)
		}
	}
	if f.builds != 0 {
		t.Fatalf("unresolved databases reached the builder %d times", f.builds)
	}
}

func TestUpgradeBrief_BuildErrors(t *testing.T) {
	invalid := &fakeBrief{buildErr: fmt.Errorf("%w: target_major 17 must be above the "+
		"server's major 17", upgradecheck.ErrInvalidTarget)}
	if code, body, _ := getBrief(t, briefDeps(invalid), "prod", ""); code != 400 ||
		!strings.Contains(body, "must be above") {
		t.Fatalf("invalid target = %d %s, want 400", code, body)
	}
	down := &fakeBrief{buildErr: errors.New("upgrade readiness: connection refused")}
	code, body, h := getBrief(t, briefDeps(down), "prod", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "connection refused") ||
		h.Get("Retry-After") == "" {
		t.Fatalf("build error = %d %s, want 503 with the cause and Retry-After", code, body)
	}
	if invalid.stores+down.stores != 0 {
		t.Fatal("a failed build was stored")
	}
}

func TestUpgradeBrief_StoreErrorStillReturnsTheBrief(t *testing.T) {
	f := &fakeBrief{brief: upgradecheck.Brief{Database: "prod"},
		storeErr: errors.New("store upgrade brief: permission denied for table upgrade_briefs")}
	code, body, _ := getBrief(t, briefDeps(f), "prod", "")
	var got upgradeBriefView
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if code != http.StatusOK || got.Stored || got.BriefID != 0 || got.Database != "prod" ||
		!strings.Contains(got.StoreError, "permission denied") {
		t.Fatalf("store error = %d %+v, want the brief with store_error", code, got)
	}
}

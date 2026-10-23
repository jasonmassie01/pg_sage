package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
)

// 'pg_sage review' end to end: the CLI over real MCP Streamable HTTP
// (mcp.Runtime) to a real Change Review service on Postgres, with the fake
// analyzer standing in for the impact engine (CHECK-SRF-03 integration).

type crLegacyBackend struct{}

var errCRLegacy = errors.New("not used by change review")

func (crLegacyBackend) GetPolicy(context.Context, mcp.PolicyRequest) (mcp.PolicyResult,
	error) {
	return mcp.PolicyResult{}, errCRLegacy
}

func (crLegacyBackend) ProposePolicyChange(context.Context, mcp.PolicyProposalRequest) (
	mcp.PolicyProposalResult, error) {
	return mcp.PolicyProposalResult{}, errCRLegacy
}

func (crLegacyBackend) RequestChange(context.Context, mcp.ChangeRequest) (mcp.ChangeResult,
	error) {
	return mcp.ChangeResult{}, errCRLegacy
}

func (crLegacyBackend) GetLedger(context.Context, mcp.LedgerRequest) (mcp.LedgerResult,
	error) {
	return mcp.LedgerResult{}, errCRLegacy
}

type crE2EBackend struct {
	crLegacyBackend
	mcp.ChangeReviewBackend
}

// newCRStack serves MCP over HTTP with bearer crTestToken bound to an
// agent principal holding read and propose on database "d".
func newCRStack(t *testing.T) (*httptest.Server, *changereview.Service) {
	t.Helper()
	pool := crfake.Pool(t)
	svc := crfake.Start(t, pool, "d", &crfake.Analyzer{}, nil)
	reg := changereview.NewRegistry()
	reg.Set("d", svc)
	server := mcp.NewServer(&crE2EBackend{ChangeReviewBackend: mcp.NewChangeReviewBackend(reg)})
	rt, err := mcp.NewRuntime(config.MCPConfig{Enabled: true, Transport: "http"}, server,
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := rt.HTTPHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mcp" || r.Header.Get("Authorization") !=
			"Bearer "+crTestToken {
			http.Error(w, `{"code":"mcp_token_required"}`, http.StatusUnauthorized)
			return
		}
		p := mcp.Principal{Actor: "token:ci", Kind: mcp.KindAgent, TokenID: "ci",
			Scopes: []mcp.Scope{mcp.ScopeRead, mcp.ScopePropose}, Databases: []string{"d"}}
		inner.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	return srv, svc
}

func TestReviewCLIEndToEndOverMCP(t *testing.T) {
	srv, svc := newCRStack(t)
	e := newCREnv(t)
	safe := writeFile(t, t.TempDir(), "safe.sql", "CREATE TABLE notes (id bigint);")
	if code := e.run(t, "submit", "--url", srv.URL, "--database", "d", "--sql-file", safe,
		"--pr-url", "https://github.com/o/r/pull/7", "--format", "json", "--wait"); code != 0 {
		t.Fatalf("safe: exit %d (%s)", code, e.stderr)
	}
	var out map[string]any
	if err := json.Unmarshal(e.stdout.Bytes(), &out); err != nil || out["verdict"] != "safe" {
		t.Fatalf("safe output: %v %s", err, e.stdout)
	}
	stored, err := svc.Get(context.Background(), out["id"].(string))
	if err != nil || stored.Source != "mcp" || stored.Actor != "mcp:token:ci" ||
		stored.PRURL != "https://github.com/o/r/pull/7" || stored.Verdict != "safe" {
		t.Fatalf("stored review %+v %v", stored, err)
	}
	e = newCREnv(t)
	risky := writeFile(t, t.TempDir(), "risky.sql", "ALTER TABLE orders DROP COLUMN email; "+
		crfake.MarkRisky)
	if code := e.run(t, "submit", "--url", srv.URL, "--database", "d", "--sql-file", risky,
		"--fail-on", "risky", "--wait"); code != 1 {
		t.Fatalf("risky: exit %d (%s)", code, e.stderr)
	}
	if !strings.Contains(e.stdout.String(), "risky") {
		t.Fatalf("risky markdown %q", e.stdout)
	}
	e = newCREnv(t)
	if code := e.run(t, "submit", "--url", srv.URL, "--database", "other", "--sql-file",
		safe); code != 3 || !strings.Contains(e.stderr.String(), "not permitted") {
		t.Fatalf("a token bound to d reviewing other: exit %d %q", code, e.stderr)
	}
	e = newCREnv(t)
	e.env.now = time.Now // the service checks deployed_at against its own clock
	if code := e.run(t, "deployed", "--url", srv.URL, "--database", "d", "--pr-url",
		"https://github.com/o/r/pull/7"); code != 0 {
		t.Fatalf("deployed: exit %d (%s)", code, e.stderr)
	}
	landed, _ := svc.Get(context.Background(), out["id"].(string))
	if landed.LandingState != "landed" || landed.LandingSource != "reported" {
		t.Fatalf("after deployed: %s/%s", landed.LandingState, landed.LandingSource)
	}
	if strings.Contains(e.stdout.String()+e.stderr.String(), crTestToken) {
		t.Fatal("the token leaked into the output")
	}
}

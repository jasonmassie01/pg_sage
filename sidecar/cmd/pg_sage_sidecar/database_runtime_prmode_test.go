package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
)

// pr-01b: the PR-mode pass follows each candidate's PR, or opens one,
// only on a writable primary, and logs a finding's note when it changes.

type recordingPR struct {
	mu      sync.Mutex
	follow  cases.PROutcome
	runs    []int64
	follows []int64
	err     error
}

func (r *recordingPR) Run(_ context.Context, c cases.PRCandidate) (cases.PROutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, c.FindingID)
	return cases.PROutcome{Status: cases.PROpened, Note: "opened as acme/shop#1"}, r.err
}

func (r *recordingPR) Follow(_ context.Context, c cases.PRCandidate) (cases.PROutcome,
	error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.follows = append(r.follows, c.FindingID)
	return r.follow, nil
}

func loaderOf(ids ...int64) prCandidateLoader {
	return func(context.Context) ([]cases.PRCandidate, error) {
		var out []cases.PRCandidate
		for _, id := range ids {
			out = append(out, cases.PRCandidate{FindingID: id})
		}
		return out, nil
	}
}

func TestPRModePassRunsWhenNothingToFollow(t *testing.T) {
	m, _ := scriptedHAMonitor(false)
	checkHA(context.Background(), m)
	r := &recordingPR{}
	var logs bytes.Buffer
	old := setLogOutput(&logs)
	defer setLogOutput(old)
	pass := prModePass("orders", r, loaderOf(3, 4), m)
	pass(context.Background())
	pass(context.Background())
	if len(r.follows) != 4 || len(r.runs) != 4 {
		t.Fatalf("follows %v runs %v, want each candidate followed then run, twice",
			r.follows, r.runs)
	}
	if n := strings.Count(logs.String(), "opened as acme/shop#1"); n != 2 {
		t.Fatalf("note logged %d times, want once per finding:\n%s", n, logs.String())
	}
}

func TestPRModePassFollowsWithoutRunning(t *testing.T) {
	m, _ := scriptedHAMonitor(false)
	checkHA(context.Background(), m)
	r := &recordingPR{follow: cases.PROutcome{Status: cases.PRDeclined,
		Note: "declined in acme/shop#1"}}
	prModePass("orders", r, loaderOf(3), m)(context.Background())
	if len(r.follows) != 1 || len(r.runs) != 0 {
		t.Fatalf("follows %v runs %v, want a followed PR not reopened", r.follows, r.runs)
	}
}

func TestPRModePassSkipsWhenNotWritableOrTheLoadFails(t *testing.T) {
	r := &recordingPR{}
	prModePass("orders", r, loaderOf(3), nil)(context.Background())
	replica, _ := scriptedHAMonitor(true)
	checkHA(context.Background(), replica)
	prModePass("orders", r, loaderOf(3), replica)(context.Background())
	m, _ := scriptedHAMonitor(false)
	checkHA(context.Background(), m)
	failing := func(context.Context) ([]cases.PRCandidate, error) {
		return nil, errors.New("read PR candidates: boom")
	}
	var logs bytes.Buffer
	old := setLogOutput(&logs)
	defer setLogOutput(old)
	prModePass("orders", r, failing, m)(context.Background())
	if len(r.follows)+len(r.runs) != 0 {
		t.Fatalf("follows %v runs %v, want none", r.follows, r.runs)
	}
	if !strings.Contains(logs.String(), "read PR candidates: boom") {
		t.Fatalf("load error not logged: %s", logs.String())
	}
}

func TestBuildPRModeNeedsACodeHost(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.PRMode = config.AgentsPRModeConfig{Enabled: true, MigrationsPath: "db",
		Label: "pg_sage"}
	if _, err := buildPRMode(cfg, nil, nil, false); err == nil ||
		!strings.Contains(err.Error(), "agents.code_host") {
		t.Fatalf("err = %v, want agents.code_host is not configured", err)
	}
	t.Setenv(config.CodeHostTokenEnv, "tok-for-test")
	cfg.Agents.CodeHost = config.AgentsCodeHostConfig{Provider: "github", Repo: "acme/shop",
		TimeoutSeconds: 5}
	m, err := buildPRMode(cfg, nil, nil, false)
	if err != nil || m == nil {
		t.Fatalf("buildPRMode = %v, %v; want PR mode on acme/shop", m, err)
	}
}

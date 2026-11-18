package cases

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestPRModeMigrationOpensOnePRWithTheSectionsInOrder(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	c := migrationCandidate()
	out, err := m.Run(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, PROpened, out.Status)
	require.Equal(t, 1, out.Number)
	require.Equal(t, "opened as acme/shop#1", out.Note)
	opened := host.Opened()
	require.Equal(t, 1, len(opened))
	pr := opened[0]
	require.Equal(t, []string{"pg_sage"}, pr.Labels)
	require.Equal(t, 1, len(pr.Files), "one new file")
	require.Equal(t, "db/migrate/20261117093015_pg_sage_add_a_not_valid_check_on_orders_total.sql",
		pr.Files[0].Path)
	require.Equal(t, c.MigrationSQL+"\n", pr.Files[0].Content)
	require.Equal(t, "pg_sage/"+c.Key(), pr.Branch)
	require.Equal(t, "pg_sage: Add a NOT VALID check on orders.total", pr.Title)
	sections := []string{"## What this changes", "## Why", "## Expected effect",
		"## Risk and rollback", "## Call site", "<!-- pg_sage:key=" + c.Key() + " -->"}
	last := -1
	for _, s := range sections {
		at := strings.Index(pr.Body, s)
		require.True(t, at > last, "section %q out of order in:\n%s", s, pr.Body)
		last = at
	}
	for _, want := range []string{"finding:41", "decision:7", "Tier 1 (plan_only)",
		"-35.0% mean_exec_time", "Risk tier: high", c.RollbackSQL,
		"a new migration file, " + pr.Files[0].Path} {
		require.Contains(t, pr.Body, want)
	}
	row, ok := rec.row(41)
	require.True(t, ok)
	require.Equal(t, "open", row.PRState)

	again, err := m.Run(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, PRCovered, again.Status)
	require.Equal(t, 1, len(host.Opened()), "a second run opens nothing")
}

func TestPRModeSecondRunFindsTheMarkerWithoutTheRecord(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	_, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	rec.rows = map[int64]SourceFixPR{} // the row was lost; the marker remains
	out, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRCovered, out.Status)
	require.Contains(t, out.Note, "acme/shop#1")
	require.Equal(t, 1, len(host.Opened()))
	_, ok := rec.row(41)
	require.True(t, ok, "the open PR found by its marker is recorded again")
}

func TestNewPRModeRefusesAnUnsafeConfig(t *testing.T) {
	deps := PRModeDeps{Host: codehosttest.New(), Store: newMemRecords(),
		Recorder: newMemRecords()}
	for name, cfg := range map[string]PRModeConfig{
		"dot dot":   {Repo: prRepo, MigrationsPath: "db/../../etc"},
		"absolute":  {Repo: prRepo, MigrationsPath: "/db/migrate"},
		"bad repo":  {Repo: "acme", MigrationsPath: "db/migrate"},
		"bad label": {Repo: prRepo, MigrationsPath: "db", Label: "a b"},
		"no path":   {Repo: prRepo},
	} {
		_, err := NewPRMode(cfg, deps)
		require.Error(t, err, name)
		require.ErrorContains(t, err, "pr_mode", name)
	}
	_, err := NewPRMode(PRModeConfig{Repo: prRepo, MigrationsPath: "db"}, PRModeDeps{})
	require.ErrorContains(t, err, "code host")
}

func TestPRModeOffLeavesTheCaseUnchanged(t *testing.T) {
	m, err := NewPRMode(PRModeConfig{}, PRModeDeps{})
	require.NoError(t, err)
	require.Nil(t, m, "no repository configured: PR mode is off")
	out, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PROutcome{}, out)
	c := Case{ID: "c1", Title: "t"}
	require.Equal(t, c, ApplyPROutcome(c, out))
	follow, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PROutcome{}, follow)
}

func TestApplyPROutcomeAddsTheNoteToTheCandidate(t *testing.T) {
	c := Case{ActionCandidates: []ActionCandidate{{ActionType: "ddl_preflight"}}}
	got := ApplyPROutcome(c, PROutcome{Status: PRNotOpened, Note: "PR not opened: x"})
	require.Equal(t, "PR not opened: x", got.ActionCandidates[0].PRNote)
	require.Equal(t, "", c.ActionCandidates[0].PRNote, "the input case is not changed")
}

func TestPRModeOpenFailureRecordsNothing(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	host.FailOpenPR(&codehost.RateLimitError{Host: "github api.github.com"})
	m := newTestPRMode(host, rec, nil, nil)
	out, err := m.Run(context.Background(), migrationCandidate())
	require.ErrorIs(t, err, codehost.ErrRateLimited)
	require.Equal(t, PRNotOpened, out.Status)
	require.Contains(t, out.Note, "PR not opened: codehost github api.github.com: rate limited")
	_, ok := rec.row(41)
	require.False(t, ok, "nothing recorded in sage.source_fix")
	require.Equal(t, 0, rec.recordCalls)
}

func TestPRModeRecordFailureIsRetriedWithoutASecondPR(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	rec.recordFails = 1
	m := newTestPRMode(host, rec, nil, nil)
	out, err := m.Run(context.Background(), migrationCandidate())
	require.ErrorIs(t, err, errRecord)
	require.Equal(t, PROpened, out.Status)
	require.Contains(t, out.Note, "recording it failed")
	again, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRCovered, again.Status)
	require.Equal(t, 1, len(host.Opened()), "the marker search prevents a second PR")
	require.Equal(t, 2, rec.recordCalls)
	row, ok := rec.row(41)
	require.True(t, ok)
	require.Equal(t, 1, row.Number)
}

func TestPRModeNoSourceFixPacketOpensNoPR(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	rec.cannot = "finding 41 has no source change"
	out, err := newTestPRMode(host, rec, nil, nil).Run(context.Background(),
		migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRNotOpened, out.Status)
	require.Equal(t, "PR not opened: finding 41 has no source change", out.Note)
	require.Empty(t, host.Opened())
}

func TestPRModeRefusesAnIncompleteCandidate(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	for name, mutate := range map[string]func(*PRCandidate){
		"no finding": func(c *PRCandidate) { c.FindingID = 0 },
		"no sql":     func(c *PRCandidate) { c.MigrationSQL = "  " },
		"unknown":    func(c *PRCandidate) { c.Kind = "index" },
		"no title":   func(c *PRCandidate) { c.Title = "" },
	} {
		c := migrationCandidate()
		mutate(&c)
		out, err := m.Run(context.Background(), c)
		require.NoError(t, err, name)
		require.Equal(t, PRNotOpened, out.Status, name)
		require.True(t, strings.HasPrefix(out.Note, "PR not opened: "), name)
	}
	require.Equal(t, 0, host.Calls(), "an incomplete candidate reaches no host")
}

func TestPRModeHostErrorsPropagateWithTheirKind(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	host.Fail(codehost.ErrForbidden)
	out, err := newTestPRMode(host, rec, nil, nil).Run(context.Background(),
		migrationCandidate())
	require.ErrorIs(t, err, codehost.ErrForbidden)
	require.Equal(t, PRNotOpened, out.Status)
	require.Contains(t, out.Note, "PR not opened:")
}

func TestPRModeTenCyclesOpenExactlyOnePR(t *testing.T) {
	// CHECK-02: ten repeated cycles, one OpenPR call reaches the host.
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	for i := 0; i < 10; i++ {
		_, err := m.Run(context.Background(), migrationCandidate())
		require.NoError(t, err, "cycle %d", i)
	}
	require.Equal(t, 1, len(host.Opened()))
}

func TestPRModeRacingCyclesOpenOnePR(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Run(context.Background(), migrationCandidate()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Run: %v", err)
	}
	require.Equal(t, 1, len(host.Opened()))
}

func TestPRModeBodyIsCutAtASectionBoundaryUnderTheLimit(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	c := migrationCandidate()
	c.RollbackSQL = strings.Repeat("-- a rollback line that is long enough\n", 2000)
	_, err := newTestPRMode(host, rec, nil, nil).Run(context.Background(), c)
	require.NoError(t, err)
	body := host.Opened()[0].Body
	require.True(t, len([]rune(body)) <= MaxPRBody, "body %d runes", len([]rune(body)))
	require.Contains(t, body, "## Expected effect")
	require.NotContains(t, body, "## Risk and rollback")
	require.NotContains(t, body, "## Call site")
	require.Contains(t, body, "Cut to fit the host's limit: Risk and rollback, Call site")
	require.True(t, strings.HasSuffix(strings.TrimSpace(body),
		"<!-- pg_sage:key="+c.Key()+" -->"), "the marker survives the cut")
}

func TestPRModeBodyAtTheLimitIsNotCut(t *testing.T) {
	sections := []prSection{{"A", strings.Repeat("a", 100)}, {"B", "b"}}
	marker := prMarker("k")
	full := renderPRBody(sections, marker, 1<<20)
	exact := renderPRBody(sections, marker, len([]rune(full)))
	require.Equal(t, full, exact, "a body exactly at the limit is kept whole")
	cut := renderPRBody(sections, marker, len([]rune(full))-1)
	require.NotContains(t, cut, "## B")
	require.True(t, len([]rune(cut)) <= len([]rune(full))-1)
}

func TestPRCandidateKeyIsStableAndSafe(t *testing.T) {
	a, b := migrationCandidate(), migrationCandidate()
	require.Equal(t, a.Key(), b.Key())
	b.MigrationSQL += " -- changed"
	require.NotEqual(t, a.Key(), b.Key(), "a different change is a different candidate")
	require.True(t, prKeyPattern.MatchString(a.Key()), "key %q", a.Key())
	require.True(t, strings.HasPrefix(a.Key(), "f41-migration-"))
}

var errBoom = errors.New("boom")

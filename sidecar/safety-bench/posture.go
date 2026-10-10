package safetybench

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostureFinding is one posture-detector hit. It mirrors the shape the
// detector framework (workstreams posturea/postureb, spec §6.15)
// produces, reduced to what the bench scores: which detector fired, on what
// object, how severe.
type PostureFinding struct {
	DetectorID string `json:"detector_id"` // e.g. "AP-03"
	Severity   string `json:"severity"`    // info | warning | critical
	Object     string `json:"object,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// PostureProvider returns posture findings for a database. DetectorProvider
// runs the real detector framework (internal/agentposture); the bench
// depends only on this interface so scoring can be tested with fakes. No
// bench code implements a detector.
type PostureProvider interface {
	// Name identifies the provider in the report.
	Name() string
	// Findings inspects the database reachable through pool and returns the
	// posture findings for it.
	Findings(ctx context.Context, pool *pgxpool.Pool) ([]PostureFinding, error)
}

// NotConnectedProvider claims no findings and reports itself as not
// connected, so every posture scenario records "detector framework not
// connected" rather than a false pass. It checks that the fixtures apply
// without scoring them.
type NotConnectedProvider struct{}

func (NotConnectedProvider) Name() string { return "not_connected" }

func (NotConnectedProvider) Findings(context.Context, *pgxpool.Pool) ([]PostureFinding, error) {
	return nil, nil
}

// PostureScenario sets up a known-bad (or version-dependent) posture on a
// fresh database and declares which detectors should fire. The setup SQL is
// loaded from testdata/posture (see posture_fixtures.go).
type PostureScenario struct {
	ID string `json:"id"`
	// Name is a short human label.
	Name string `json:"name"`
	// Expect lists the detector ids that should fire for this scenario
	// (spec §6.15). A scenario can expect more than one.
	Expect []string `json:"expect"`
	// Scope is the text every counted finding's object contains (the
	// scenario's schema or role): the detectors read the whole database,
	// so findings on other objects must not score for this scenario.
	Scope string `json:"scope"`
	// SetupSQL is the fixture that creates the posture, loaded from disk.
	SetupSQL string `json:"-"`
	// TeardownSQL removes what must not outlive the scenario, such as
	// cluster-wide agent roles; empty when nothing needs removing.
	TeardownSQL string `json:"-"`
	// VersionNote records a version dependency (e.g. the installed pgvector
	// release) that decides whether the expected detector can fire.
	VersionNote string `json:"version_note,omitempty"`
}

// PostureResult is one scenario scored against a provider's findings.
type PostureResult struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Expect           []string         `json:"expect"`
	Provider         string           `json:"provider"`
	Connected        bool             `json:"connected"`
	Found            []PostureFinding `json:"found,omitempty"`
	MissingDetectors []string         `json:"missing_detectors,omitempty"`
	// MatchedDetectors are expected detectors the provider fired.
	MatchedDetectors []string `json:"matched_detectors,omitempty"`
	VersionNote      string   `json:"version_note,omitempty"`
}

// RunPosture applies each scenario's fixture to a fresh schema on owner's
// database, asks the provider for findings, scores expected detector ids
// against what fired on the scenario's objects, and runs the teardown.
// With the NotConnectedProvider every scenario is recorded unconnected (no
// matches claimed).
func RunPosture(
	ctx context.Context, owner *pgxpool.Pool, scenarios []PostureScenario, p PostureProvider,
) ([]PostureResult, error) {
	connected := p.Name() != (NotConnectedProvider{}).Name()
	results := make([]PostureResult, 0, len(scenarios))
	for _, sc := range scenarios {
		r, err := runPostureScenario(ctx, owner, sc, p, connected)
		if err != nil {
			return nil, fmt.Errorf("posture %s: %w", sc.ID, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// teardownTimeout bounds a scenario's teardown, which runs after ctx ends.
const teardownTimeout = 30 * time.Second

func runPostureScenario(
	ctx context.Context, owner *pgxpool.Pool, sc PostureScenario,
	p PostureProvider, connected bool,
) (r PostureResult, err error) {
	defer func() {
		if sc.TeardownSQL == "" {
			return
		}
		// Tear down even when ctx ended, so no agent role outlives the run.
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		_, tdErr := owner.Exec(tctx, sc.TeardownSQL)
		if tdErr != nil && err == nil {
			err = fmt.Errorf("teardown: %w", tdErr)
		}
	}()
	if sc.SetupSQL != "" {
		if _, err := owner.Exec(ctx, sc.SetupSQL); err != nil {
			return PostureResult{}, fmt.Errorf("setup: %w", err)
		}
	}
	found, err := p.Findings(ctx, owner)
	if err != nil {
		return PostureResult{}, fmt.Errorf("provider findings: %w", err)
	}
	found = scopedFindings(sc.Scope, found)
	r = PostureResult{ID: sc.ID, Name: sc.Name, Expect: sc.Expect,
		Provider: p.Name(), Connected: connected, Found: found, VersionNote: sc.VersionNote}
	r.MatchedDetectors, r.MissingDetectors = scoreDetectors(sc.Expect, found)
	return r, nil
}

// scoreDetectors splits the expected detector ids into those the findings
// fired (matched) and those they did not (missing).
func scoreDetectors(expect []string, found []PostureFinding) (matched, missing []string) {
	fired := map[string]bool{}
	for _, f := range found {
		fired[f.DetectorID] = true
	}
	for _, id := range expect {
		if fired[id] {
			matched = append(matched, id)
		} else {
			missing = append(missing, id)
		}
	}
	sort.Strings(matched)
	sort.Strings(missing)
	return matched, missing
}

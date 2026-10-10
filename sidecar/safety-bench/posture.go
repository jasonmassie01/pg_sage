package safetybench

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostureFinding is one posture-detector hit. It mirrors the shape the
// detector framework (workstreams posturea/postureb, AGENTDB-SPEC §6.15)
// produces, reduced to what the bench scores: which detector fired, on what
// object, how severe.
type PostureFinding struct {
	DetectorID string `json:"detector_id"` // e.g. "AP-03"
	Severity   string `json:"severity"`    // info | warning | critical
	Object     string `json:"object,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// PostureProvider returns posture findings for a database. The real
// detector framework implements this; the bench depends only on this
// interface so the framework and the scenarios can be built in parallel.
//
// The coordinator wires the real provider by replacing the provider passed
// to RunPosture (see NotConnectedProvider for the v0 placeholder). No bench
// code implements a detector; that is the posture workstreams' job.
type PostureProvider interface {
	// Name identifies the provider in the report.
	Name() string
	// Findings inspects the database reachable through pool and returns the
	// posture findings for it.
	Findings(ctx context.Context, pool *pgxpool.Pool) ([]PostureFinding, error)
}

// NotConnectedProvider is the v0 placeholder used until the detector
// framework branch is connected. It returns no findings and reports itself
// as not connected, so every posture scenario records "detector framework
// not connected" rather than a false pass. Replace it in RunPosture with the
// real provider once the coordinator supplies the framework branch.
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
	// (AGENTDB-SPEC §6.15). A scenario can expect more than one.
	Expect []string `json:"expect"`
	// SetupSQL is the fixture that creates the posture, loaded from disk.
	SetupSQL string `json:"-"`
	// VersionNote records a version dependency (e.g. pgvector or server
	// version) the scenario cannot create on the bench's server; the real
	// provider reports such arms from catalog facts.
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
// database, asks the provider for findings, and scores expected detector
// ids against what fired. With the NotConnectedProvider, every scenario is
// recorded unconnected (no matches claimed).
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

func runPostureScenario(
	ctx context.Context, owner *pgxpool.Pool, sc PostureScenario,
	p PostureProvider, connected bool,
) (PostureResult, error) {
	if sc.SetupSQL != "" {
		if _, err := owner.Exec(ctx, sc.SetupSQL); err != nil {
			return PostureResult{}, fmt.Errorf("setup: %w", err)
		}
	}
	found, err := p.Findings(ctx, owner)
	if err != nil {
		return PostureResult{}, fmt.Errorf("provider findings: %w", err)
	}
	r := PostureResult{ID: sc.ID, Name: sc.Name, Expect: sc.Expect,
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

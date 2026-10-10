package safetybench

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures a bench run.
type Options struct {
	// SelfCheckOnly restricts the read-only corpus to the harness
	// self-checks (used until RO-01..RO-16 fixtures are authored).
	SelfCheckOnly bool
	// Posture is the posture provider; nil runs the real detectors
	// (NewDetectorProvider).
	Posture PostureProvider
	Meta    ReportMeta
}

// Run executes all three sections on owner's database and returns the
// report. owner must point at the bench's own disposable database.
func Run(ctx context.Context, owner *pgxpool.Pool, opts Options) (Report, error) {
	cases, err := LoadCases(opts.SelfCheckOnly)
	if err != nil {
		return Report{}, err
	}
	designs := Designs(readOnlyRole)
	roResults, err := RunReadOnly(ctx, owner, cases, designs)
	if err != nil {
		return Report{}, err
	}
	scenarios, err := PostureScenarios()
	if err != nil {
		return Report{}, err
	}
	provider := opts.Posture
	if provider == nil {
		provider = NewDetectorProvider()
	}
	postureResults, err := RunPosture(ctx, owner, scenarios, provider)
	if err != nil {
		return Report{}, err
	}
	meta := opts.Meta
	meta.Designs = designNames(designs)
	meta.PostureWired = provider.Name() != (NotConnectedProvider{}).Name()
	return BuildReport(meta, roResults, postureResults, IncidentMapping()), nil
}

func designNames(designs []RODesign) []string {
	names := make([]string, len(designs))
	for i, d := range designs {
		names[i] = d.Name()
	}
	return names
}

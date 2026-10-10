package safetybench

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentposture"
)

// DetectorProvider is the real PostureProvider: it runs every registered
// agent posture detector (AP-01..AP-16) in one read-only transaction.
type DetectorProvider struct {
	Config agentposture.Config
}

// NewDetectorProvider returns the provider with the shipped configuration.
func NewDetectorProvider() DetectorProvider {
	return DetectorProvider{Config: agentposture.DefaultConfig()}
}

// Name identifies the provider in the report.
func (DetectorProvider) Name() string { return "agentposture" }

// Findings runs the detectors on pool's database. A detector that did not
// complete is an error: a silent gap would score as a missed detection.
func (p DetectorProvider) Findings(ctx context.Context,
	pool *pgxpool.Pool) ([]PostureFinding, error) {
	res, err := agentposture.RunAll(ctx, pool, agentposture.RunOptions{Config: p.Config})
	if err != nil {
		return nil, err
	}
	if len(res.Failed) > 0 {
		ids := make([]string, 0, len(res.Failed))
		for id, why := range res.Failed {
			ids = append(ids, fmt.Sprintf("%s (%v)", id, why))
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("posture detectors did not complete: %s",
			strings.Join(ids, "; "))
	}
	return postureFindings(res.Findings()), nil
}

// postureFindings reduces the framework's findings to the bench's shape.
func postureFindings(in []agentposture.Finding) []PostureFinding {
	out := make([]PostureFinding, 0, len(in))
	for _, f := range in {
		out = append(out, PostureFinding{DetectorID: f.Detector,
			Severity: string(f.Severity), Object: f.Object, Detail: f.Detail})
	}
	return out
}

// scopedFindings keeps the findings whose object names scope; an empty
// scope keeps them all.
func scopedFindings(scope string, found []PostureFinding) []PostureFinding {
	out := make([]PostureFinding, 0, len(found))
	for _, f := range found {
		if strings.Contains(f.Object, scope) {
			out = append(out, f)
		}
	}
	return out
}

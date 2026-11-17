// Package autovacuum is Sage Review's per-table autovacuum generator
// (review-02b). From each table's vacuum history in the pack (live rows,
// daily dead tuples, autovacuum counts, its own storage parameters) and
// the server's autovacuum settings, it computes the table's
// autovacuum_vacuum_scale_factor, threshold and analyze scale factor so
// that the predicted dead-tuple peak stays under TargetDeadShare of the
// live rows. The arithmetic is in code, from the fitted dead-tuple growth
// per hour. Each candidate is a set_table_autovacuum step with the
// reloption rollback the executor's contract builds (RESET of keys that
// had no value, SET of the prior values otherwise); a table whose dead
// tuples outrun any trigger is advice.
package autovacuum

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves.
const (
	Name      = "autovacuum"
	Dimension = "configuration"
)

// Candidate kinds and the prediction metric.
const (
	KindTable         = "table_autovacuum"
	KindFallingBehind = "autovacuum_falling_behind"
	MetricDeadPeak    = "dead_tuple_peak"
)

// The arithmetic's constants.
const (
	// TargetDeadShare is the dead-tuple peak a table may reach, as a share
	// of its live rows.
	TargetDeadShare = 0.05
	// MinLiveRows is the smallest table worth its own settings.
	MinLiveRows = 100_000
	// MarginHours is the growth allowed on top of autovacuum_naptime
	// between the trigger and the vacuum taking effect.
	MarginHours = 1.0
	// thresholdShare is the most of the trigger budget the fixed
	// threshold may take.
	thresholdShare = 0.1
)

// Generator is the autovacuum generator. It keeps no state.
type Generator struct{}

// New is the autovacuum generator.
func New() *Generator { return &Generator{} }

// Name is "autovacuum".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate judges every table of the pack's autovacuum section. A pack
// without the section gives no candidates and Coverage.Missing
// ["autovacuum"]; without the settings section PostgreSQL's defaults are
// assumed and "settings" is missing. A table with no vacuum history gives
// no candidate. A nil pack, an invalid env and a canceled context are
// errors.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if pack == nil {
		return nil, candidate.Coverage{}, errors.New("autovacuum generator: no evidence pack")
	}
	if env.Database == "" || env.WindowDays < 1 {
		return nil, candidate.Coverage{}, fmt.Errorf("autovacuum generator: env names "+
			"database %q and a window of %d days", env.Database, env.WindowDays)
	}
	if err := ctx.Err(); err != nil {
		return nil, candidate.Coverage{}, fmt.Errorf("autovacuum generator for %s: %w",
			env.Database, err)
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	raw := pack.File("autovacuum.json")
	if raw == nil {
		cov.AddMissing("autovacuum")
		return nil, cov, nil
	}
	var sec workpack.Autovacuum
	if err := json.Unmarshal(raw, &sec); err != nil {
		return nil, cov, fmt.Errorf("autovacuum generator for %s: decode autovacuum.json: %w",
			env.Database, err)
	}
	gl, err := readGlobals(pack, &cov)
	if err != nil {
		return nil, cov, fmt.Errorf("autovacuum generator for %s: %w", env.Database, err)
	}
	var out []candidate.Candidate
	for _, t := range sec.Tables {
		if c, ok := judge(t, gl, &cov); ok {
			out = append(out, c)
		}
	}
	if err := candidate.ValidateAll(pack, out); err != nil {
		return nil, cov, fmt.Errorf("autovacuum generator for %s: %w", env.Database, err)
	}
	return out, cov, nil
}

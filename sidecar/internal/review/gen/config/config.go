// Package config is Sage Review's configuration generator (review-02b): it
// judges the server's settings as a set. Every non-default setting of the
// pack's settings section is a fact (NonDefaults), and a deterministic rule
// table (rules.go) checks each judged setting, default or not, against the
// workload and hardware facts that contradict it. Values are computed in
// code and clamped to pgconf's safe range (Clamp); a value outside it is
// never emitted. A reloadable setting on a self-managed server is an
// alter_system_guc step; a restart-bound one, and every setting on a
// managed service, is an operator step pg_sage never runs (managedparam's
// proposal on a managed service); what neither can describe is advice.
// The LLM config advisor is untouched and keeps running.
package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves.
const (
	Name      = "config"
	Dimension = "configuration"
)

// Candidate kinds: the rule that found the contradiction.
const (
	KindSharedBuffers     = "shared_buffers_memory"
	KindWorkMem           = "work_mem_spills"
	KindMaxWALSize        = "max_wal_size_checkpoints"
	KindRandomPageCost    = "random_page_cost_storage"
	KindIOConcurrency     = "pg18_io_concurrency"
	KindIOMethod          = "pg18_io_method"
	KindIdleInTransaction = "idle_in_transaction"
)

// Generator is the configuration generator. It keeps no state.
type Generator struct{}

// New is the configuration generator.
func New() *Generator { return &Generator{} }

// Name is "config".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate runs the rule table over the pack's settings. A pack without
// the settings section gives no candidates and Coverage.Missing
// ["settings"]; a rule whose input is unknown is skipped with a note. A
// nil pack, an invalid env and a canceled context are errors.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if pack == nil {
		return nil, candidate.Coverage{}, errors.New("config generator: no evidence pack")
	}
	if env.Database == "" || env.WindowDays < 1 {
		return nil, candidate.Coverage{}, fmt.Errorf("config generator: env names "+
			"database %q and a window of %d days", env.Database, env.WindowDays)
	}
	if err := ctx.Err(); err != nil {
		return nil, candidate.Coverage{}, fmt.Errorf("config generator for %s: %w",
			env.Database, err)
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	v, ok, err := readView(pack)
	if err != nil {
		return nil, cov, fmt.Errorf("config generator for %s: %w", env.Database, err)
	}
	if !ok {
		cov.AddMissing("settings")
		return nil, cov, nil
	}
	for _, name := range v.skipped {
		cov.Note("setting %q skipped: not a setting name pg_sage reads", name)
	}
	if !v.memoryOK {
		cov.AddMissing("memory")
	}
	b := newBuilder(v, &cov)
	for _, r := range rules {
		r.judge(b)
	}
	if err := candidate.ValidateAll(pack, b.out); err != nil {
		return nil, cov, fmt.Errorf("config generator for %s: %w", env.Database, err)
	}
	return b.out, cov, nil
}

// Contradicted is the review's number: the settings this generator found
// contradicted by the workload or hardware (one candidate each).
func Contradicted(cands []candidate.Candidate) int {
	n := 0
	for _, c := range cands {
		if c.Generator == Name {
			n++
		}
	}
	return n
}

// NonDefaults is every setting of the pack's settings section whose
// source is not the built-in default, by name: the facts a review lists
// beside the rules that judge them. A pack without the section has none;
// a name that is not a setting name is left out.
func NonDefaults(pack *workpack.Pack) ([]workpack.SettingFact, error) {
	if pack == nil {
		return nil, errors.New("config non-defaults: no evidence pack")
	}
	v, ok, err := readView(pack)
	if err != nil || !ok {
		return nil, err
	}
	var out []workpack.SettingFact
	for _, s := range v.order {
		if s.Source != "default" {
			out = append(out, s)
		}
	}
	return out, nil
}

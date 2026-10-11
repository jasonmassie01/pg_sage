package firstlook

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentposture"
)

// SectionAgentPosture is the first look's "Agent posture" section (G0,
// spec §6.15): one check per posture detector, run in the first look's
// transaction under its statement budget and single retry.
const SectionAgentPosture = agentposture.Section

// postureEnv is the posture environment, resolved once per pass by the
// first posture step that runs.
type postureEnv struct {
	done bool
	env  agentposture.Env
	err  error
}

// postureSteps is one step per registered posture detector, so a failing
// detector degrades only its own check.
func (p *pass) postureSteps() []step {
	reg := p.opts.PostureRegistry
	if reg == nil {
		reg = agentposture.Default()
	}
	dets := reg.Detectors()
	out := make([]step, 0, len(dets))
	for _, d := range dets {
		out = append(out, step{rules: []string{d.Spec().ID}, section: SectionAgentPosture,
			fn: func(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
				return p.posture(ctx, tx, d)
			}})
	}
	return out
}

// posture runs one detector and keeps its findings as items; the skipped
// version arms are stated in the check's note.
func (p *pass) posture(ctx context.Context, tx pgx.Tx, d agentposture.Detector) (
	[]outcome, error) {
	env, err := p.resolvePosture(ctx, tx)
	if err != nil {
		return nil, err
	}
	res, err := agentposture.RunDetector(ctx, d, tx, env)
	if err != nil {
		return nil, err
	}
	items := make([]Item, len(res.Findings))
	for i, f := range res.Findings {
		items[i] = postureItem(f)
	}
	o := p.add(res.Detector, items)
	if skipped := res.Note(); skipped != "" {
		o.note = joinNotes(o.note, skipped)
	}
	return []outcome{o}, nil
}

func (p *pass) resolvePosture(ctx context.Context, tx pgx.Tx) (agentposture.Env, error) {
	if !p.postureEnv.done {
		cfg := agentposture.DefaultConfig()
		if p.opts.Posture != nil {
			cfg = *p.opts.Posture
		}
		p.postureEnv.env, p.postureEnv.err = agentposture.ResolveEnv(ctx, tx, cfg)
		p.postureEnv.done = true
	}
	return p.postureEnv.env, p.postureEnv.err
}

// postureItem is a posture finding as a first-look item; its fix script
// is the suggested SQL, which pg_sage never runs (L1).
func postureItem(f agentposture.Finding) Item {
	ev := make([]Evidence, len(f.Evidence))
	for i, e := range f.Evidence {
		ev[i] = Evidence{Source: e.Source, Ref: e.Ref, Detail: e.Detail}
	}
	return Item{Rule: f.Detector, Section: SectionAgentPosture,
		Severity: string(f.Severity), Object: f.Object, Title: f.Title, Detail: f.Detail,
		Recommendation: f.Recommendation, SuggestedSQL: f.FixScript, Caveat: f.Caveat,
		Evidence: ev}
}

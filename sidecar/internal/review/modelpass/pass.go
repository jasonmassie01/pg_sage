package modelpass

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// pass is one review's model pass: its seed and tools are built once and
// only read by its runs.
type pass struct {
	s         Settings
	d         Deps
	in        Input
	seed      []agentloop.Evidence
	tools     []agentloop.Tool
	ren       renamer
	byID      map[string]candidate.Candidate
	kept      map[int]int
	droppedOf map[int][]agentloop.DroppedClaim
}

func newPass(s Settings, d Deps, in Input) (*pass, error) {
	seed, views, err := buildSeed(in)
	if err != nil {
		return nil, fmt.Errorf("build the model's evidence: %w", err)
	}
	if d.Budget.MaxSteps == 0 {
		d.Budget = DefaultBudget
	}
	if d.Protocol == "" {
		d.Protocol = agentloop.ProtocolAuto
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	p := &pass{s: s, d: d, in: in, seed: seed, tools: tools(in.Pack, views),
		ren: newRenamer(in.Pack.Pseudonyms), byID: map[string]candidate.Candidate{},
		kept: map[int]int{}, droppedOf: map[int][]agentloop.DroppedClaim{}}
	for _, c := range in.Candidates {
		p.byID[c.ID] = c
	}
	return p, nil
}

func (p *pass) config(task string, final agentloop.Final) agentloop.Config {
	return agentloop.Config{System: p.systemPrompt(), Task: task, Tools: p.tools,
		Final: final, Seed: p.seed, Budget: p.d.Budget, Protocol: p.d.Protocol,
		Ground: nil, MaxClaims: maxItemClaims, Now: p.d.Now}
}

func (p *pass) rankConfig() agentloop.Config {
	return p.config(p.rankTask(), agentloop.Final{Name: finalName, Parameters: reviewSchema,
		Description: "Submit the ranked, attacked and linked candidates, hypotheses and " +
			"the report. Every entry's claims cite evidence aliases."})
}

func (p *pass) systemPrompt() string {
	return fmt.Sprintf("You are Sage Review's model for the PostgreSQL database %q. "+
		"pg_sage's deterministic generators proposed the candidates below. Rank the ones "+
		"worth doing, attack each (what could make it wrong), link related ones, propose "+
		"hypotheses the generators missed and write one short text per dimension. "+
		"You cannot run SQL, change anything or approve anything: your answer has no SQL "+
		"field and any SQL you write is shown to a person as prose. Every claim must "+
		"cite the aliases of the evidence it rests on and every number in it must appear "+
		"in that evidence; anything else is dropped.\n%s", p.in.Database,
		llm.UntrustedDataRule)
}

func (p *pass) rankTask() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review of %q.", p.in.Database)
	if g := p.in.Grade; g != nil {
		fmt.Fprintf(&b, " Grade %s (rubric %s).", g.Overall, g.RubricVersion)
		for _, d := range g.Dimensions {
			fmt.Fprintf(&b, "\n- dimension %s: grade %s, score %v, coverage %v", d.Dimension,
				d.Letter, d.Score, d.Coverage)
		}
	}
	b.WriteString("\nCandidates (candidate_id, alias):")
	for i, c := range p.in.Candidates {
		fmt.Fprintf(&b, "\n- %s is E%d", c.ID, i+1)
	}
	b.WriteString("\nRank every candidate you keep from 1; decision is keep, drop or " +
		"needs_evidence.")
	return b.String()
}

// passResult is the pass's runs and, when done, its combined answer.
type passResult struct {
	runs   []runOutcome
	status report.PassStatus
	reason string
	done   int
	merged merged
}

// run executes the runs, checks each answer and combines them.
func (p *pass) run(ctx context.Context) passResult {
	if p.s.Consensus == config.ReviewConsensusRefute && p.s.Runs > 1 {
		return p.runRefute(ctx)
	}
	runs := p.runMajority(ctx)
	answers := p.check(runs)
	res := passResult{runs: runs, done: len(answers)}
	need := p.s.Runs/2 + 1
	if len(answers) < need {
		res.status, res.reason = p.shortfall(runs, len(answers))
		return res
	}
	res.status, res.merged = report.PassDone, majority(answers, need)
	return res
}

// shortfall is why too few runs finished: a skip (budget, turned off)
// when any run was skipped, else a failure naming the first one.
func (p *pass) shortfall(runs []runOutcome, done int) (report.PassStatus, string) {
	for _, o := range runs {
		if o.status == StatusSkipped {
			return report.PassSkipped, o.reason
		}
	}
	why := ""
	for _, o := range runs {
		if o.status == StatusFailed {
			why = ": " + o.reason
			break
		}
	}
	return report.PassFailed, fmt.Sprintf("only %d of %d model runs finished%s", done,
		p.s.Runs, why)
}

// checked is one done run's checked answer.
type checked struct {
	attempt    int
	ranking    []rankItem
	links      []linkItem
	hypotheses []hypothesis
	reports    []reportItem
	rejected   []report.Rejected
	dropped    []agentloop.DroppedClaim
}

// check decodes and filters every done run's answer; a run whose answer
// does not decode becomes failed.
func (p *pass) check(runs []runOutcome) []checked {
	var out []checked
	for i := range runs {
		o := &runs[i]
		if o.status != StatusDone {
			continue
		}
		s, err := decodeReview(o.res.Final)
		if err != nil {
			o.status, o.reason = StatusFailed, clip(err.Error(), 1000)
			continue
		}
		out = append(out, p.checkAnswer(o, s))
	}
	return out
}

func (p *pass) checkAnswer(o *runOutcome, s submitted) checked {
	c := &checker{attempt: o.attempt, evidence: o.res.Evidence, known: p.known()}
	a := checked{attempt: o.attempt, ranking: c.rankings(s.Ranking), links: c.links(s.Links),
		hypotheses: c.hypotheses(s.Hypotheses), reports: c.reports(s.Report)}
	a.rejected, a.dropped = c.rejected, append(append([]agentloop.DroppedClaim(nil),
		o.res.Dropped...), c.dropped...)
	p.kept[o.attempt], p.droppedOf[o.attempt] = c.kept, a.dropped
	return a
}

func (p *pass) known() map[string]bool {
	k := make(map[string]bool, len(p.byID))
	for id := range p.byID {
		k[id] = true
	}
	return k
}

// recordJSON is v as JSON for a stored run ("[]" when it cannot encode).
func recordJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil || string(raw) == "null" {
		return json.RawMessage("[]")
	}
	return raw
}

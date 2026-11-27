package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/llm"
)

// PR mode (pr-01b) opens and follows pull requests in the background.
const (
	prModeInterval = 15 * time.Minute
	prModeLimit    = 20
	// prModeTimeout bounds one pass (code-host calls included).
	prModeTimeout   = 5 * time.Minute
	prModeMaxTokens = 400
)

// startPRMode runs PR mode on the database when agents.pr_mode.enabled.
// A code host that cannot be built leaves it off with a warning.
func (rt *databaseRuntime) startPRMode() {
	if rt.cfg == nil || !rt.cfg.Agents.PRMode.Enabled {
		return
	}
	m, err := buildPRMode(rt.cfg, rt.spec.Pool, rt.llmManager, rt.llmOn)
	if err != nil || m == nil {
		logWarn("pr_mode", "db %q: PR mode is off: %v", rt.spec.Name, err)
		return
	}
	pass := prModePass(rt.spec.Name, m, prModeLoader(rt.spec.Pool), rt.ha)
	rt.start(func() { every(rt.ctx, prModeInterval, pass) })
	rt.note("pr_mode")
}

func buildPRMode(cfg *config.Config, pool *pgxpool.Pool, mgr *llm.Manager, llmOn bool,
) (*cases.PRMode, error) {
	host, err := codeHostClient(cfg)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, fmt.Errorf("agents.code_host is not configured")
	}
	tools := agenttools.New(pool, agentToolOptions(cfg))
	pr := cfg.Agents.PRMode
	repo := pr.EffectiveRepo(cfg.Agents.CodeHost)
	deps := cases.PRModeDeps{Host: host, Store: cases.NewPRStore(pool),
		Recorder:  agenttools.PRRecorder{Tools: tools},
		CallSites: agenttools.PRCallSites{Tools: tools, Host: host, Repo: repo}}
	if mgr != nil && llmOn {
		deps.Model = prModel{mgr: mgr}
	}
	return cases.NewPRMode(cases.PRModeConfig{Repo: repo, MigrationsPath: pr.MigrationsPath,
		Label: pr.Label}, deps)
}

// prModel is PR mode's paragraph writer over the general model.
type prModel struct{ mgr *llm.Manager }

// Chat forwards the prompt internal/cases builds (fenced facts).
//
//llm:transport internal/cases.prModelSystem
func (p prModel) Chat(ctx context.Context, system, user string) (string, error) {
	resp, _, err := p.mgr.ChatForPurpose(ctx, "pr_mode", system, user, prModeMaxTokens)
	return resp, err
}

type prCandidateLoader func(ctx context.Context) ([]cases.PRCandidate, error)

func prModeLoader(pool *pgxpool.Pool) prCandidateLoader {
	return func(ctx context.Context) ([]cases.PRCandidate, error) {
		return cases.LoadPRCandidates(ctx, pool, executor.ClassifySQL, prModeLimit)
	}
}

// prRunner is cases.PRMode's Run and Follow.
type prRunner interface {
	Run(ctx context.Context, c cases.PRCandidate) (cases.PROutcome, error)
	Follow(ctx context.Context, c cases.PRCandidate) (cases.PROutcome, error)
}

// prModePass is one bounded pass: follow each candidate's PR, or open one.
// It writes to the sage schema, so it runs only on a writable primary; a
// nil monitor fails closed. A finding's note is logged when it changes.
func prModePass(name string, m prRunner, load prCandidateLoader, mon *ha.Monitor,
) func(context.Context) {
	seen := map[int64]string{}
	return func(ctx context.Context) {
		if mon == nil || haNotWritable(mon) {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, prModeTimeout)
		defer cancel()
		candidates, err := load(ctx)
		if err != nil {
			logWarn("pr_mode", "db %q: %v", name, err)
			return
		}
		for _, c := range candidates {
			out, err := m.Follow(ctx, c)
			if err == nil && out.Status == "" {
				out, err = m.Run(ctx, c)
			}
			switch {
			case err != nil:
				logWarn("pr_mode", "db %q: finding %d: %s: %v", name, c.FindingID,
					out.Note, err)
			case out.Note != "" && out.Note != seen[c.FindingID]:
				logInfo("pr_mode", "db %q: finding %d: %s", name, c.FindingID, out.Note)
			}
			seen[c.FindingID] = out.Note
		}
	}
}

package llm

import (
	"path/filepath"
	"strings"
	"testing"
)

// llm-09: every model call (Chat, ChatResult, ChatForPurpose,
// ChatWithTools) under internal/ and cmd/ must send a prompt that carries
// UntrustedDataRule. The guard judges each call site, not each file or
// package: the enclosing function, or the package-level const, var or
// func its prompt argument names, must reference the rule. Otherwise the
// function says why it need not:
//
//	//llm:transport <dir.builder>...  the prompt is built elsewhere; the
//	                                   guard checks each named builder
//	//llm:no-db-content <reason>      the prompt holds no database text
//
// This replaces security-05's census table, which mapped sites to prompt
// files by hand and could not see a transport that named the wrong file.

const sidecarRoot = "../.."

func TestCallSiteGuard(t *testing.T) {
	results := guardSites(t, sidecarRoot)
	var lines, problems []string
	for _, r := range results {
		if r.problem != "" {
			problems = append(problems, r.site+": "+r.problem)
			continue
		}
		lines = append(lines, r.site+": "+r.verdict)
	}
	t.Logf("%d model call sites:\n%s", len(results), strings.Join(lines, "\n"))
	if len(problems) > 0 {
		t.Fatalf("%d unfenced model call sites:\n%s", len(problems),
			strings.Join(problems, "\n"))
	}
}

// knownCallSites are the 19 call sites outside package llm when llm-09
// landed. The guard must keep finding them: a walk that silently saw
// nothing would pass vacuously.
var knownCallSites = []string{
	"internal/advisor/llmcall.go:chatAdvisor",
	"internal/agentguard/classify/model.go:ModelSuggester.Suggest",
	"internal/agentloop/loop.go:run.step",
	"internal/analyzer/plan_narrative.go:LLMPlanNarrator.Narrate",
	"internal/ask/run.go:budgetedModel.ChatWithTools",
	"internal/briefing/briefing.go:Worker.enhanceWithLLM",
	"internal/executor/justify.go:Executor.justifyAndStore",
	"internal/explain/explain_llm.go:Explainer.enhanceWithLLM",
	"internal/facts/model.go:ModelProposer.Propose",
	"internal/firstlook/summary.go:Summarizer.Summarize",
	"internal/migration/llm_fallback.go:Advisor.llmFallback",
	"internal/migration/llm_scripts.go:ScriptGenerator.modelScript",
	"internal/rca/narrate.go:runNarration",
	"internal/rca/tier2.go:Engine.runTier2",
	"internal/schema/lint/llm_jsonb.go:LLMJsonbAnalyzer.queryAndAnalyze",
	"internal/sre/budget.go:RunModelTurn",
	"internal/sre/investigator_model.go:investigatorSession.reservedCall",
	"internal/sre/runbook/compile.go:attempt",
	"internal/tuning/loop.go:toolLoop.Run",
}

func TestCallSiteGuardFindsEveryKnownSite(t *testing.T) {
	seen := map[string]guardResult{}
	outside := 0
	for _, r := range guardSites(t, sidecarRoot) {
		seen[r.site] = r
		if !strings.HasPrefix(r.site, "internal/llm/") {
			outside++
		}
	}
	for _, site := range knownCallSites {
		if _, ok := seen[site]; !ok {
			t.Errorf("guard did not find known call site %s", site)
		}
	}
	if outside < len(knownCallSites) {
		t.Errorf("guard found %d sites outside package llm, want >= %d",
			outside, len(knownCallSites))
	}
	// The transports the plan names must be annotated, not passed by luck.
	for _, site := range []string{
		"internal/agentloop/loop.go:run.step",
		"internal/ask/run.go:budgetedModel.ChatWithTools",
		"internal/sre/budget.go:RunModelTurn",
	} {
		if v := seen[site].verdict; !strings.HasPrefix(v, "transport:") {
			t.Errorf("%s verdict = %q (problem %q), want a transport annotation",
				site, v, seen[site].problem)
		}
	}
	if v := seen["internal/llm/probe.go:Client.Probe"].verdict; !strings.HasPrefix(v,
		"no database content:") {
		t.Errorf("probe verdict = %q, want no-db-content", v)
	}
}

func TestCallSiteGuardFixtures(t *testing.T) {
	cases := []struct {
		name, problem string // problem "" means the fixture passes
	}{
		{"unfenced", "does not reference llm.UntrustedDataRule"},
		{"own_rule_text", "does not reference llm.UntrustedDataRule"},
		{"tools_param", "does not reference llm.UntrustedDataRule"},
		{"fenced_const", ""},
		{"fenced_local", ""},
		{"fenced_inline", ""},
		{"transport_ok", ""},
		{"transport_no_builder", "must name the prompt builder"},
		{"transport_bad_builder", "builder internal/p.plain does not reference"},
		{"transport_missing_builder", "builder internal/p.nosuch does not exist"},
		{"no_db_content_ok", ""},
		{"no_db_content_no_reason", "needs a reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			results := guardSites(t, filepath.Join("testdata", "callsite", c.name))
			if len(results) != 1 {
				t.Fatalf("fixture has %d call sites, want 1: %+v", len(results), results)
			}
			r := results[0]
			switch {
			case c.problem == "" && r.problem != "":
				t.Fatalf("fixture failed: %s", r.problem)
			case c.problem == "" && r.verdict == "":
				t.Fatal("fixture passed with no verdict")
			case c.problem != "" && !strings.Contains(r.problem, c.problem):
				t.Fatalf("problem = %q (verdict %q), want it to contain %q",
					r.problem, r.verdict, c.problem)
			}
		})
	}
}

func TestCallSiteGuardEmptyTree(t *testing.T) {
	// Nil/empty: a tree with no Go files has no sites and no problems.
	if got := guardSites(t, t.TempDir()); len(got) != 0 {
		t.Fatalf("empty tree = %+v, want no sites", got)
	}
}

// No concurrent-access test: the guard is a read-only walk over source
// files with no shared state between calls.

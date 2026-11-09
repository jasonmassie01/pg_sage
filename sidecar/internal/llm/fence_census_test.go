package llm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// security-05: every model call site must send a system prompt that
// carries UntrustedDataRule. The census runs per call site, not per
// package: a sibling file that references the rule proves nothing about
// the prompt a call actually sends.

// fenceSite maps one call site (file:func) to the file that builds its
// system prompt, or exempts it with the reason.
type fenceSite struct {
	prompt string
	exempt string
}

var fenceCensus = map[string]fenceSite{
	"internal/advisor/llmcall.go:chatAdvisor": {
		prompt: "internal/advisor/llmcall.go"},
	"internal/agentguard/classify/model.go:ModelSuggester.Suggest": {
		prompt: "internal/agentguard/classify/model.go"},
	// The loop sends the messages its own systemPrompt built.
	"internal/agentloop/loop.go:run.step": {
		prompt: "internal/agentloop/prompt.go"},
	"internal/analyzer/plan_narrative.go:LLMPlanNarrator.Narrate": {
		prompt: "internal/analyzer/plan_narrative.go"},
	// Ask Sage's budget wrapper forwards the agent loop's messages.
	"internal/ask/run.go:budgetedModel.ChatWithTools": {
		prompt: "internal/agentloop/prompt.go"},
	"internal/briefing/briefing.go:Worker.enhanceWithLLM": {
		prompt: "internal/briefing/briefing.go"},
	"internal/executor/justify.go:Executor.justifyAndStore": {
		prompt: "internal/executor/justify.go"},
	"internal/explain/explain_llm.go:Explainer.enhanceWithLLM": {
		prompt: "internal/explain/explain_llm.go"},
	"internal/facts/model.go:ModelProposer.Propose": {
		prompt: "internal/facts/model.go"},
	"internal/firstlook/summary.go:Summarizer.Summarize": {
		prompt: "internal/firstlook/summary.go"},
	"internal/llm/client.go:Client.Chat": {
		exempt: "compatibility wrapper: it forwards its caller's system prompt to " +
			"ChatResult, and every caller is a site of its own in this table"},
	"internal/llm/manager.go:Manager.ChatForPurpose": {
		exempt: "forwarding wrapper: it sends the system prompt its caller built, " +
			"and every caller is a site of its own in this table"},
	"internal/llm/probe.go:Client.Probe": {
		exempt: "connection test: a fixed system prompt and a nonce, with no " +
			"database text, so there is nothing untrusted to fence"},
	"internal/migration/llm_fallback.go:Advisor.llmFallback": {
		prompt: "internal/migration/llm_fallback.go"},
	"internal/migration/llm_scripts.go:ScriptGenerator.modelScript": {
		prompt: "internal/migration/llm_scripts.go"},
	"internal/rca/narrate.go:runNarration": {
		prompt: "internal/rca/narrate_tools.go"},
	"internal/rca/tier2.go:Engine.runTier2": {
		prompt: "internal/rca/tier2.go"},
	"internal/schema/lint/llm_jsonb.go:LLMJsonbAnalyzer.queryAndAnalyze": {
		prompt: "internal/schema/lint/llm_jsonb.go"},
	// The reservation transport sends the review messages built there.
	"internal/sre/budget.go:RunModelTurn": {
		prompt: "internal/sre/model_prompt.go"},
	// The investigator runs the agent loop; this forwards its messages.
	"internal/sre/investigator_model.go:investigatorSession.reservedCall": {
		prompt: "internal/agentloop/prompt.go"},
	"internal/sre/runbook/compile.go:attempt": {
		prompt: "internal/sre/runbook/compile_prompt.go"},
	"internal/tuning/loop.go:toolLoop.Run": {
		prompt: "internal/tuning/prompt.go"},
}

var modelCallNames = map[string]bool{
	"Chat": true, "ChatResult": true, "ChatWithTools": true, "ChatForPurpose": true,
}

const sidecarRoot = "../.."

func TestFenceCensus(t *testing.T) {
	sites := modelCallSites(t)
	t.Logf("%d model call sites:\n%s", len(sites), strings.Join(sites, "\n"))
	seen := map[string]bool{}
	var unfenced []string
	for _, site := range sites {
		seen[site] = true
		entry, ok := fenceCensus[site]
		switch {
		case !ok:
			unfenced = append(unfenced, site+": not in the census table")
		case entry.exempt != "":
			continue
		case !referencesRule(t, entry.prompt):
			unfenced = append(unfenced, site+": prompt file "+entry.prompt+
				" does not reference llm.UntrustedDataRule")
		}
	}
	for site := range fenceCensus {
		if !seen[site] {
			unfenced = append(unfenced, site+": stale census entry (no such call site)")
		}
	}
	sort.Strings(unfenced)
	t.Logf("%d unfenced sites", len(unfenced))
	if len(unfenced) > 0 {
		t.Fatalf("unfenced model call sites:\n%s", strings.Join(unfenced, "\n"))
	}
}

// modelCallSites lists every x.Chat / x.ChatResult / x.ChatWithTools /
// x.ChatForPurpose call in the non-test files under internal/ and cmd/ as
// file:func.
func modelCallSites(t *testing.T) []string {
	t.Helper()
	set := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		root := filepath.Join(sidecarRoot, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			for _, site := range callSitesIn(t, path) {
				set[site] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	out := make([]string, 0, len(set))
	for site := range set {
		out = append(out, site)
	}
	sort.Strings(out)
	return out
}

func callSitesIn(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	rel, err := filepath.Rel(sidecarRoot, path)
	if err != nil {
		t.Fatalf("relative path of %s: %v", path, err)
	}
	rel = filepath.ToSlash(rel)
	var sites []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && modelCallNames[sel.Sel.Name] {
				sites = append(sites, rel+":"+funcName(fn))
			}
			return true
		})
	}
	return sites
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// referencesRule reports whether the file's code (not its comments)
// names llm.UntrustedDataRule.
func referencesRule(t *testing.T, rel string) bool {
	t.Helper()
	path := filepath.Join(sidecarRoot, filepath.FromSlash(rel))
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse prompt file %s: %v", rel, err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "llm" &&
				x.Sel.Name == "UntrustedDataRule" {
				found = true
			}
		case *ast.Ident:
			if file.Name.Name == "llm" && x.Name == "UntrustedDataRule" {
				found = true
			}
		}
		return !found
	})
	return found
}

func TestFenceCensusSeesASiteWithoutTheRule(t *testing.T) {
	// Boundary: the census must flag a real unfenced file, so check the
	// detector against a file known to lack the rule and one that has it.
	if referencesRule(t, "internal/llm/manager.go") {
		t.Fatal("manager.go reported as referencing the rule")
	}
	if !referencesRule(t, "internal/llm/untrusted.go") {
		t.Fatal("untrusted.go not reported as referencing the rule")
	}
	if len(modelCallSites(t)) < len(fenceCensus) {
		t.Fatal("call-site walk found fewer sites than the census lists")
	}
}

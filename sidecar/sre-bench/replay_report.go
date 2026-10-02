package srebench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// The replay section of the PGIncidentBench report: the corpus (per
// family and class), every arm's metrics per family and pooled with
// denominators and Wilson intervals, the replay gates, the model traffic
// and every run. It is attached to the bench report (Report.Replay) and
// written on its own by TestReplayCorpus.

const (
	replayJSON     = "pgincidentbench-replay.json"
	replayMarkdown = "pgincidentbench-replay.md"
)

// ReplayMeta describes a replay run.
type ReplayMeta struct {
	Arms, Gated   []string
	LLM           LLMConfig
	GeneratedAt   time.Time
	ServerVersion string
}

// CorpusCount is how many cases of one family and class the corpus has.
type CorpusCount struct {
	Family string `json:"family"`
	Class  string `json:"class"`
	N      int    `json:"n"`
}

// ArmUsage is one arm's model traffic over the replay.
type ArmUsage struct {
	Arm   string   `json:"arm"`
	Runs  int      `json:"runs"`
	Usage TapUsage `json:"usage"`
}

// ReplayReport is the replay section.
type ReplayReport struct {
	Schema        string        `json:"corpus_schema"`
	GeneratedAt   time.Time     `json:"generated_at"`
	ServerVersion string        `json:"server_version,omitempty"`
	LLM           LLMConfig     `json:"llm"`
	Cases         int           `json:"cases"`
	Corpus        []CorpusCount `json:"corpus"`
	Arms          []string      `json:"arms"`
	Gated         []string      `json:"gated_arms"`
	Cells         []CellRecord  `json:"cells"`
	Gates         []GateResult  `json:"gates"`
	Usage         []ArmUsage    `json:"model_usage,omitempty"`
	Runs          []RunRecord   `json:"runs"`
}

// BuildReplayReport scores replay results.
func BuildReplayReport(rs []Result, cases []replay.Case, meta ReplayMeta) ReplayReport {
	s := Summarize(rs, meta.Arms)
	r := ReplayReport{Schema: replay.Schema, GeneratedAt: meta.GeneratedAt,
		ServerVersion: meta.ServerVersion, LLM: meta.LLM, Cases: len(cases),
		Corpus: corpusCounts(cases), Arms: s.Arms, Gated: meta.Gated}
	for _, arm := range s.Arms {
		for _, fam := range s.Families {
			r.Cells = append(r.Cells, cellOf(arm, fam, s.Tally(arm, fam)))
		}
		r.Gates = append(r.Gates, ReplayGates(s, rs, arm, meta.LLM.Mode)...)
		if m := s.Tally(arm, PooledFamily).Model; m.Runs > 0 {
			r.Usage = append(r.Usage, ArmUsage{Arm: arm, Runs: m.Runs, Usage: m.Usage})
		}
	}
	for _, res := range rs {
		r.Runs = append(r.Runs, runOf(res))
	}
	return r
}

func corpusCounts(cases []replay.Case) []CorpusCount {
	n := map[[2]string]int{}
	for _, c := range cases {
		n[[2]string{c.Family, c.Class}]++
	}
	out := make([]CorpusCount, 0, len(n))
	for k, v := range n {
		out = append(out, CorpusCount{Family: k[0], Class: k[1], N: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family != out[j].Family {
			return out[i].Family < out[j].Family
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// Markdown renders the replay section.
func (r ReplayReport) Markdown() string {
	var b strings.Builder
	b.WriteString("## Replay corpus\n\n")
	fmt.Fprintf(&b, "- Corpus: %s, %d cases\n- LLM-on arm model: %s\n", r.Schema, r.Cases,
		r.LLM)
	fmt.Fprintf(&b, "- Gated arms: %s\n\n", strings.Join(r.Gated, ", "))
	b.WriteString(row("family", "class", "cases"))
	b.WriteString(separator(3))
	for _, c := range r.Corpus {
		b.WriteString(row(c.Family, c.Class, fmt.Sprint(c.N)))
	}
	view := Report{Arms: r.Arms, Cells: r.Cells, Runs: r.Runs}
	b.WriteString("\n### Replay results per family and arm\n\nRates show " +
		"(hits/denominator) [95% Wilson interval]. Insufficient = confounded plus " +
		"missing-data/adversarial cases without a root.\n\n")
	view.writeCells(&b)
	view.writeModel(&b)
	writeUsage(&b, r.Usage, r.LLM)
	b.WriteString("\n### Replay gates\n\n")
	b.WriteString(row("gate", "family", "arm", "status", "observed", "threshold", "note"))
	b.WriteString(separator(7))
	for _, g := range r.Gates {
		b.WriteString(row(g.ID, g.Family, g.Arm, string(g.Status), g.Observed, g.Threshold,
			g.Reason))
	}
	b.WriteString("\n### Replay runs\n\n")
	view.writeRuns(&b)
	return b.String()
}

// writeUsage renders each arm's model traffic: calls, tokens by kind and
// HTTP errors (cost follows from the provider's prices).
func writeUsage(b *strings.Builder, usage []ArmUsage, llm LLMConfig) {
	if len(usage) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### Model traffic (%s)\n\n", llm)
	b.WriteString(row("arm", "runs", "calls", "prompt tokens", "completion tokens",
		"reasoning tokens", "HTTP errors"))
	b.WriteString(separator(7))
	for _, u := range usage {
		b.WriteString(row(u.Arm, fmt.Sprint(u.Runs), fmt.Sprint(u.Usage.Calls),
			fmt.Sprint(u.Usage.PromptTokens), fmt.Sprint(u.Usage.CompletionTokens),
			fmt.Sprint(u.Usage.ReasoningTokens), fmt.Sprint(u.Usage.HTTPErrors)))
	}
}

// WriteReplayReport writes the replay section's JSON and Markdown into
// dir, creating it.
func WriteReplayReport(dir string, r ReplayReport) (jsonPath, mdPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("report directory %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode replay report: %w", err)
	}
	jsonPath, mdPath = filepath.Join(dir, replayJSON), filepath.Join(dir, replayMarkdown)
	if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", jsonPath, err)
	}
	if err := os.WriteFile(mdPath, []byte("# PGIncidentBench replay\n\n"+r.Markdown()),
		0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", mdPath, err)
	}
	return jsonPath, mdPath, nil
}

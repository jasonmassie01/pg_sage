package srebench

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Report configuration and schema.
const (
	// ReportSchema versions the JSON result.
	ReportSchema = "pg_sage.pgincidentbench.v1"
	// EnvRepeats sets how many times every scenario runs (default 1).
	EnvRepeats = "SAGE_BENCH_REPEATS"
	// EnvReportDir is where the report files go (default: a temp dir).
	EnvReportDir = "SAGE_BENCH_REPORT_DIR"
	// EnvRun must be "1" for TestPGIncidentBench to run. CI runs the bench
	// in its own step so the ./... suites stay within their timeouts.
	EnvRun = "SAGE_BENCH_RUN"
	// MaxRepeats bounds EnvRepeats.
	MaxRepeats = 10

	reportJSON     = "pgincidentbench.json"
	reportMarkdown = "pgincidentbench.md"
)

// BenchRunRequested reports whether EnvRun asks for the full bench.
func BenchRunRequested(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(EnvRun)) == "1"
}

// ParseRepeats reads EnvRepeats: empty means 1.
func ParseRepeats(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxRepeats {
		return 0, fmt.Errorf("%s=%q: want an integer from 1 to %d", EnvRepeats, v,
			MaxRepeats)
	}
	return n, nil
}

// ReportDir is v, or fallback when v is empty.
func ReportDir(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}

// ReportMeta describes a bench run.
type ReportMeta struct {
	Arms, Gated   []string
	Pending       map[string]string
	Repeats       int
	ServerVersion string
	GeneratedAt   time.Time
	LLM           LLMConfig
	// PgSageVersion and PgSageCommit name the pg_sage build the report
	// scores (empty: unstamped).
	PgSageVersion, PgSageCommit string
}

// Thresholds are the pre-registered gate thresholds.
type Thresholds struct {
	MinTop1                   float64 `json:"min_top1"`
	MinInsufficientAbstention float64 `json:"min_insufficient_abstention"`
	MaxForbidden              int     `json:"max_forbidden"`
	MaxVariantDrop            float64 `json:"max_variant_drop"`
	MaxPacketP95S             float64 `json:"max_packet_p95_s"`
}

// Report is the machine-readable bench result.
type Report struct {
	Schema        string            `json:"schema"`
	GeneratedAt   time.Time         `json:"generated_at"`
	ServerVersion string            `json:"server_version"`
	PgSageVersion string            `json:"pg_sage_version,omitempty"`
	PgSageCommit  string            `json:"pg_sage_commit,omitempty"`
	Repeats       int               `json:"repeats"`
	Arms          []string          `json:"arms"`
	Gated         []string          `json:"gated_arms"`
	Pending       map[string]string `json:"pending_arms,omitempty"`
	LLM           LLMConfig         `json:"llm"`
	Thresholds    Thresholds        `json:"thresholds"`
	Cells         []CellRecord      `json:"cells"`
	Gates         []GateResult      `json:"gates"`
	Runs          []RunRecord       `json:"runs"`
	// Replay is the replay corpus section, when the replay ran.
	Replay *ReplayReport `json:"replay,omitempty"`
}

// Metric is a proportion with its denominator; the rate and Wilson
// bounds are null without one.
type Metric struct {
	K    int      `json:"k"`
	N    int      `json:"n"`
	Rate *float64 `json:"rate"`
	Low  *float64 `json:"wilson_low"`
	High *float64 `json:"wilson_high"`
}

func metricOf(p Prop) Metric {
	lo, hi := p.Interval()
	return Metric{K: p.K, N: p.N, Rate: num(p.Rate()), Low: num(lo), High: num(hi)}
}

// num is v, or nil when v is NaN (JSON has no NaN).
func num(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

func millis(d time.Duration, ok bool) *float64 {
	if !ok {
		return nil
	}
	return num(float64(d.Microseconds()) / 1000)
}

// CellRecord is one arm's metrics for one family (or PooledFamily).
type CellRecord struct {
	Arm                    string      `json:"arm"`
	Family                 string      `json:"family"`
	Pending                string      `json:"pending,omitempty"`
	Runs                   int         `json:"runs"`
	Errored                int         `json:"errored"`
	Skipped                int         `json:"skipped"`
	SafePass               Metric      `json:"safe_pass"`
	Top1                   Metric      `json:"top1"`
	Top3                   Metric      `json:"top3"`
	CleanTop1              Metric      `json:"clean_top1"`
	NoiseTop1              Metric      `json:"noise_top1"`
	DecoyFalse             Metric      `json:"decoy_false_diagnosis"`
	Abstention             Metric      `json:"abstention"`
	InsufficientAbstention Metric      `json:"insufficient_abstention"`
	Selective              Metric      `json:"selective_accuracy"`
	Consistency            Metric      `json:"consistency"`
	Forbidden              int         `json:"forbidden_actions"`
	ProbesPerRun           *float64    `json:"probes_per_run"`
	FirstEvidenceP50MS     *float64    `json:"first_evidence_p50_ms"`
	PacketP95MS            *float64    `json:"packet_p95_ms"`
	MechanismPrecision     *float64    `json:"mechanism_precision"`
	MechanismRecall        *float64    `json:"mechanism_recall"`
	Model                  *ModelTally `json:"model,omitempty"`
}

func cellOf(arm, family string, t Tally) CellRecord {
	ttfe, ttfeOK := quantile(t.FirstEvidence, 0.5)
	p95, p95OK := quantile(t.Packets, 0.95)
	c := CellRecord{
		Arm:                    arm,
		Family:                 family,
		Runs:                   t.Runs,
		Errored:                t.Errored,
		Skipped:                t.Skipped,
		SafePass:               metricOf(t.SafePass),
		Top1:                   metricOf(t.Top1),
		Top3:                   metricOf(t.Top3),
		CleanTop1:              metricOf(t.CleanTop1),
		NoiseTop1:              metricOf(t.NoiseTop1),
		DecoyFalse:             metricOf(t.DecoyFalse),
		Abstention:             metricOf(t.Abstention),
		InsufficientAbstention: metricOf(t.InsufficientAbstention),
		Selective:              metricOf(t.Selective),
		Consistency:            metricOf(t.Consistency),
		Forbidden:              t.Forbidden,
		ProbesPerRun:           num(t.ProbesPerRun()),
		FirstEvidenceP50MS:     millis(ttfe, ttfeOK),
		PacketP95MS:            millis(p95, p95OK),
		MechanismPrecision:     num(t.Precision()),
		MechanismRecall:        num(t.Recall()),
	}
	if t.Model.Runs > 0 {
		m := t.Model
		c.Model = &m
	}
	return c
}

// RunRecord is one arm's run of one scenario.
type RunRecord struct {
	Scenario         string      `json:"scenario"`
	Family           string      `json:"family"`
	Class            string      `json:"class"`
	Arm              string      `json:"arm"`
	Repeat           int         `json:"repeat"`
	Attempts         int         `json:"attempts"`
	GoldRoot         string      `json:"gold_root,omitempty"`
	GoldContributing []string    `json:"gold_contributing,omitempty"`
	Lookalike        string      `json:"lookalike,omitempty"`
	State            string      `json:"state,omitempty"`
	Root             string      `json:"root,omitempty"`
	Contributing     []string    `json:"contributing,omitempty"`
	Ranked           []string    `json:"ranked,omitempty"`
	ProbeCount       int         `json:"probe_count"`
	FirstEvidenceMS  *float64    `json:"first_evidence_ms,omitempty"`
	PacketMS         *float64    `json:"packet_ms,omitempty"`
	Forbidden        []string    `json:"forbidden,omitempty"`
	Skipped          string      `json:"skipped,omitempty"`
	Error            string      `json:"error,omitempty"`
	Grade            *Grade      `json:"grade,omitempty"`
	Model            *ModelStats `json:"model,omitempty"`
}

func runOf(r Result) RunRecord {
	sc, o := r.Scenario, r.Outcome
	rec := RunRecord{Scenario: sc.ID, Family: string(sc.Family), Class: sc.Class,
		Arm: r.Arm, Repeat: r.Repeat, Attempts: r.Attempts, GoldRoot: sc.Gold.Root,
		GoldContributing: sc.Gold.Contributing, Lookalike: sc.Gold.Lookalike,
		State: string(o.State), Root: o.Root, Contributing: o.Contributing,
		Ranked: o.Ranked, ProbeCount: o.ProbeCount, Forbidden: o.Forbidden,
		Skipped: r.Skipped}
	rec.Model = o.Model
	if r.Err != nil {
		rec.Error = r.Err.Error()
	}
	if o.Measured {
		rec.PacketMS = millis(o.Packet, true)
		rec.FirstEvidenceMS = millis(o.FirstEvidence, o.ProbeCount > 0)
	}
	if scored(r) {
		g := GradeResult(r)
		rec.Grade = &g
	}
	return rec
}

// BuildReport scores the results of every arm in meta.Arms. A pending
// arm is listed with its reason and every gate not evaluated.
func BuildReport(rs []Result, meta ReportMeta) Report {
	s := Summarize(rs, meta.Arms)
	r := Report{Schema: ReportSchema, GeneratedAt: meta.GeneratedAt,
		ServerVersion: meta.ServerVersion, PgSageVersion: meta.PgSageVersion,
		PgSageCommit: meta.PgSageCommit, Repeats: meta.Repeats, Arms: s.Arms,
		Gated: meta.Gated, Pending: meta.Pending, LLM: meta.LLM,
		Thresholds: Thresholds{MinTop1: MinTop1,
			MinInsufficientAbstention: MinInsufficientAbstention, MaxForbidden: MaxForbidden,
			MaxVariantDrop: MaxVariantDrop, MaxPacketP95S: MaxPacketP95.Seconds()}}
	for _, arm := range s.Arms {
		why, pending := meta.Pending[arm]
		for _, fam := range s.Families {
			c := cellOf(arm, fam, s.Tally(arm, fam))
			c.Pending = why
			r.Cells = append(r.Cells, c)
		}
		switch {
		case pending:
			r.Gates = append(r.Gates, PendingGates(arm, why, s.Families)...)
		case arm == ArmLLM:
			r.Gates = append(r.Gates, llmArmGates(s, rs, meta.LLM.Mode)...)
		default:
			r.Gates = append(r.Gates, EvaluateGates(s, arm)...)
		}
	}
	for _, res := range rs {
		r.Runs = append(r.Runs, runOf(res))
	}
	return r
}

// WriteReport writes the JSON result and the Markdown summary into dir,
// creating it.
func WriteReport(dir string, r Report) (jsonPath, mdPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("report directory %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode report: %w", err)
	}
	jsonPath, mdPath = filepath.Join(dir, reportJSON), filepath.Join(dir, reportMarkdown)
	if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", jsonPath, err)
	}
	if err := os.WriteFile(mdPath, []byte(r.Markdown()), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", mdPath, err)
	}
	return jsonPath, mdPath, nil
}

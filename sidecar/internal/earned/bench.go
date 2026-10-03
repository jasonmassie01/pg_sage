package earned

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// BenchSchema is the PGIncidentBench JSON schema the ledger reads
// (sidecar/sre-bench report.go).
const BenchSchema = "pg_sage.pgincidentbench.v1"

// MaxReportBytes bounds an ingested report.
const MaxReportBytes = 8 << 20

// maxReportFutureSkew tolerates clock skew between the bench host and us.
const maxReportFutureSkew = 5 * time.Minute

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// armPattern admits the bench's arm names ("causal-graph+llm").
var armPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.:-]{0,63}$`)

type wireMetric struct {
	K int `json:"k"`
	N int `json:"n"`
}

type wireCell struct {
	Arm       string     `json:"arm"`
	Family    string     `json:"family"`
	Pending   string     `json:"pending"`
	Runs      int        `json:"runs"`
	SafePass  wireMetric `json:"safe_pass"`
	Top1      wireMetric `json:"top1"`
	Precision *float64   `json:"mechanism_precision"`
	Forbidden int        `json:"forbidden_actions"`
}

type wireReport struct {
	Schema        string     `json:"schema"`
	GeneratedAt   time.Time  `json:"generated_at"`
	PgSageVersion string     `json:"pg_sage_version"`
	PgSageCommit  string     `json:"pg_sage_commit"`
	Gated         []string   `json:"gated_arms"`
	Cells         []wireCell `json:"cells"`
}

// ParseBenchReport reads a PGIncidentBench JSON report: the schema must
// match, the report must not come from the future, every cell must be a
// consistent proportion, and the pg_sage build it names (if any) must be
// well formed. The per-run records are not kept. now bounds the
// generation time.
func ParseBenchReport(raw []byte, now time.Time) (EvalRun, error) {
	if len(raw) == 0 || len(raw) > MaxReportBytes {
		return EvalRun{}, fmt.Errorf("%w: report size %d (1..%d bytes)",
			ErrInvalidReport, len(raw), MaxReportBytes)
	}
	var w wireReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&w); err != nil {
		return EvalRun{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return EvalRun{}, fmt.Errorf("%w: trailing data after the report", ErrInvalidReport)
	}
	if err := w.validate(now); err != nil {
		return EvalRun{}, err
	}
	build := Build{Version: w.PgSageVersion, Commit: w.PgSageCommit}.Normalized()
	if err := build.validate(); err != nil {
		return EvalRun{}, err
	}
	sum := sha256.Sum256(raw)
	run := EvalRun{Schema: w.Schema, GeneratedAt: w.GeneratedAt.UTC(), Build: build,
		Gated: append([]string{}, w.Gated...), SHA256: hex.EncodeToString(sum[:])}
	for _, c := range w.Cells {
		run.Cells = append(run.Cells, Cell{Arm: c.Arm, Family: c.Family, Pending: c.Pending,
			Runs: c.Runs, Top1: Metric(c.Top1), SafePass: Metric(c.SafePass),
			Precision: c.Precision, Forbidden: c.Forbidden})
	}
	return run, nil
}

func (w wireReport) validate(now time.Time) error {
	switch {
	case w.Schema != BenchSchema:
		return fmt.Errorf("%w: schema %q, want %q", ErrInvalidReport, w.Schema, BenchSchema)
	case w.GeneratedAt.IsZero():
		return fmt.Errorf("%w: generated_at is missing", ErrInvalidReport)
	case w.GeneratedAt.After(now.Add(maxReportFutureSkew)):
		return fmt.Errorf("%w: generated_at %s is in the future", ErrInvalidReport,
			w.GeneratedAt.Format(time.RFC3339))
	case len(w.Cells) == 0:
		return fmt.Errorf("%w: no cells", ErrInvalidReport)
	}
	for _, arm := range w.Gated {
		if !armPattern.MatchString(arm) {
			return fmt.Errorf("%w: gated arm %q", ErrInvalidReport, arm)
		}
	}
	for i, c := range w.Cells {
		if err := c.validate(); err != nil {
			return fmt.Errorf("%w: cell %d: %v", ErrInvalidReport, i, err)
		}
	}
	return nil
}

func (c wireCell) validate() error {
	switch {
	case !armPattern.MatchString(c.Arm):
		return fmt.Errorf("arm %q", c.Arm)
	case !namePattern.MatchString(c.Family):
		return fmt.Errorf("family %q", c.Family)
	case !consistent(c.Top1) || !consistent(c.SafePass):
		return fmt.Errorf("inconsistent proportion (k must be 0..n)")
	case c.Precision != nil && (*c.Precision < 0 || *c.Precision > 1):
		return fmt.Errorf("mechanism_precision %v outside 0..1", *c.Precision)
	case c.Forbidden < 0 || c.Runs < 0:
		return fmt.Errorf("negative count")
	case len(c.Pending) > 500:
		return fmt.Errorf("pending reason too long")
	}
	return nil
}

func consistent(m wireMetric) bool { return m.N >= 0 && m.K >= 0 && m.K <= m.N }

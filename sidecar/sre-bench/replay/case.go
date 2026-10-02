// Package replay is PGIncidentBench's replay corpus (AI-SRE-SPEC §12
// source 1, Codex §10): redacted incident cases recorded as the probe
// results the investigator would have read, frozen at detection time,
// and a runner that replays them through the real investigator in place
// of probes.Runner. A case is one JSON file (schema
// pg_sage.sre.replay_case.v1); new families add files under cases/.
package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Schema versions the case format.
const Schema = "pg_sage.sre.replay_case.v1"

// Case classes (Codex §10: positive, benign/confounded lookalikes,
// missing-data and adversarial).
const (
	// ClassPositive is a fault with a known mechanism: the gold root.
	ClassPositive = "positive"
	// ClassConfounded is a benign lookalike: the right answer is
	// inconclusive, and Gold.Lookalike names what it imitates.
	ClassConfounded = "confounded"
	// ClassMissingData lacks or corrupts evidence (no privilege, timeout,
	// stale or contradictory samples, a counter reset or restart).
	ClassMissingData = "missing_data"
	// ClassAdversarial carries an attack in the data: prompt injection or
	// a secret that must never leave redacted.
	ClassAdversarial = "adversarial"
)

// Case limits. MaxLookahead is the investigation's active-time ceiling:
// nothing in a case may come from after it (no future leakage).
const (
	MaxLookahead = 120 * time.Second
	MaxLookback  = 24 * time.Hour
	MaxCaseBytes = 256 << 10
)

// Tags with a meaning to the grader.
const (
	// TagPromptInjection marks instructions planted in the data.
	TagPromptInjection = "prompt_injection"
)

// ErrInvalidCase is wrapped by every parse and validation failure.
var ErrInvalidCase = errors.New("invalid replay case")

// Case is one replay case.
type Case struct {
	Schema       string        `json:"schema"`
	ID           string        `json:"id"`
	Family       string        `json:"family"`
	Class        string        `json:"class"`
	Description  string        `json:"description"`
	Provenance   string        `json:"provenance"`
	DetectedAt   time.Time     `json:"detected_at"`
	Subject      string        `json:"subject,omitempty"`
	Tags         []string      `json:"tags,omitempty"`
	Gold         Gold          `json:"gold"`
	Canaries     []string      `json:"canaries,omitempty"`
	Observations []Observation `json:"observations"`
}

// Gold is the case's expected diagnosis. An empty root means the right
// answer is inconclusive; Rationale says why.
type Gold struct {
	Root         string   `json:"root,omitempty"`
	Contributing []string `json:"contributing,omitempty"`
	Lookalike    string   `json:"lookalike,omitempty"`
	Rationale    string   `json:"rationale"`
}

// Observation is one recorded probe result, offset from detection.
type Observation struct {
	Probe     probes.ID     `json:"probe"`
	Status    probes.Status `json:"status"`
	OffsetMS  int64         `json:"offset_ms"`
	Reason    string        `json:"reason,omitempty"`
	Error     string        `json:"error,omitempty"`
	Rows      []probes.Row  `json:"rows,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// Sufficient reports whether the case's evidence supports a root.
func (c Case) Sufficient() bool { return c.Gold.Root != "" }

// At is when the observation was made.
func (c Case) At(o Observation) time.Time {
	return c.DetectedAt.Add(time.Duration(o.OffsetMS) * time.Millisecond)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCase, fmt.Sprintf(format, args...))
}

// Parse decodes one case strictly: unknown fields and trailing data are
// rejected, and numbers stay exact (json.Number), as the store keeps
// them. It does not validate; see Validate.
func Parse(raw []byte) (Case, error) {
	if len(raw) > MaxCaseBytes {
		return Case{}, invalid("case is %d bytes, more than %d", len(raw), MaxCaseBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var c Case
	if err := dec.Decode(&c); err != nil {
		return Case{}, invalid("decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Case{}, invalid("trailing data after the case")
	}
	return c, nil
}

// caseText is the case's untrusted text (subject and every observation),
// where canaries and injected instructions live.
func caseText(c Case) string {
	var b strings.Builder
	b.WriteString(c.Subject)
	for _, o := range c.Observations {
		raw, _ := json.Marshal(o)
		b.WriteByte('\n')
		b.Write(raw)
	}
	return b.String()
}

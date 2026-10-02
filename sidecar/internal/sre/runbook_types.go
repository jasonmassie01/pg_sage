package sre

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Typed runbooks (AI-SRE-SPEC §7.1, §10 sre_runbooks). A runbook belongs to
// one database. Each edit appends an immutable version; a version runs
// only when it is the latest, an admin signed its exact content hash, the
// stored content still hashes to it, it still validates against today's
// catalogs and the runbook is not retired.

// RunbookStatus is a runbook's state as surfaces show it.
type RunbookStatus string

// Runbook statuses.
const (
	RunbookDraft   RunbookStatus = "draft"
	RunbookSigned  RunbookStatus = "signed"
	RunbookRetired RunbookStatus = "retired"
	// RunbookInvalid is a signed version whose stored content no longer
	// matches its hash, or that no longer validates: it never runs.
	RunbookInvalid RunbookStatus = "invalid"
)

// Runbook sources.
const (
	SourceManual   = "manual"
	SourceCompiled = "compiled"
)

// Labels shown with runbook output.
const (
	RunbookRunLabel      = "signed runbook (deterministic)"
	RunbookProposalLabel = "runbook proposal (not executed)"
)

// Runbook run outcomes.
const (
	RunbookCompleted   = "completed"
	RunbookAbstained   = "abstained"
	RunbookProbeBudget = "probe_budget_exhausted"
	RunbookNoTime      = "no_time"
)

var runbookOutcomes = map[string]bool{RunbookCompleted: true, RunbookAbstained: true,
	RunbookProbeBudget: true, RunbookNoTime: true}

// notFoundError is a not-found error with its own message.
type notFoundError string

func (e notFoundError) Error() string        { return string(e) }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

// Runbook errors. ErrRunbookNotFound is also ErrNotFound.
var (
	ErrRunbookNotFound  error = notFoundError("runbook not found")
	ErrHashMismatch           = errors.New("content hash does not match the runbook version")
	ErrAlreadySigned          = errors.New("runbook version is already signed")
	ErrRetired                = errors.New("runbook is retired")
	ErrModelUnavailable       = errors.New("no model is available")
)

// RunbookVersion is one immutable version of a runbook.
type RunbookVersion struct {
	Version     int                `json:"version"`
	Name        string             `json:"name"`
	Definition  runbook.Definition `json:"definition"`
	ContentHash string             `json:"content_hash"`
	Source      string             `json:"source"`
	SourceText  string             `json:"source_text,omitempty"`
	CompiledBy  string             `json:"compiled_by,omitempty"`
	CreatedBy   string             `json:"created_by"`
	CreatedAt   time.Time          `json:"created_at"`
	SignedBy    string             `json:"signed_by,omitempty"`
	SignerRole  string             `json:"signer_role,omitempty"`
	SignedAt    *time.Time         `json:"signed_at"`
	// SignatureValid: signed, the signature binds the stored hash and the
	// stored definition still hashes to it.
	SignatureValid bool `json:"signature_valid"`
	signedHash     string
	contentValid   bool
}

// Runbook is a runbook with its latest version (and, from GetRunbook,
// every version newest first).
type Runbook struct {
	ID            UUID             `json:"id"`
	Scope         Scope            `json:"-"`
	LatestVersion int              `json:"latest_version"`
	Status        RunbookStatus    `json:"status"`
	Runnable      bool             `json:"runnable"`
	CreatedBy     string           `json:"created_by"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	RetiredBy     string           `json:"retired_by,omitempty"`
	RetiredAt     *time.Time       `json:"retired_at"`
	Latest        RunbookVersion   `json:"latest"`
	Versions      []RunbookVersion `json:"versions,omitempty"`
	Problems      runbook.Problems `json:"problems,omitempty"`
}

// RunbookInput is the content of a new version.
type RunbookInput struct {
	Definition runbook.Definition
	Source     string // SourceManual (default) or SourceCompiled
	SourceText string // the redacted English playbook of a compiled draft
	CompiledBy string // the model that compiled it
	Actor      string
}

// RunbookSignature signs one version: the signer states the version and
// the content hash they reviewed.
type RunbookSignature struct {
	Version     int
	ContentHash string
	Signer      string
	Role        string
}

// RunbookProposal is what a runbook run proposes; it is never executed.
type RunbookProposal struct {
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	Node       string `json:"node,omitempty"`
	ActionType string `json:"action_type,omitempty"`
	Text       string `json:"text"`
}

// RunbookRun records which runbook version ran in an investigation, the
// path it took and what it proposes.
type RunbookRun struct {
	Label           string           `json:"label"`
	RunbookID       UUID             `json:"runbook_id"`
	Version         int              `json:"version"`
	Name            string           `json:"name"`
	ContentHash     string           `json:"content_hash"`
	SignedBy        string           `json:"signed_by"`
	Outcome         string           `json:"outcome"`
	Reason          string           `json:"reason,omitempty"`
	Path            []string         `json:"path"`
	Probes          int              `json:"probes"`
	Proposal        *RunbookProposal `json:"proposal,omitempty"`
	InvestigationID UUID             `json:"investigation_id,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
}

// Run record limits.
const (
	maxRunPath     = 64
	maxRunReason   = 500
	maxRunbookRuns = 100
)

func (r *RunbookRun) validate() error {
	if r == nil {
		return nil
	}
	if r.Label != RunbookRunLabel || !runbookOutcomes[r.Outcome] || r.Version <= 0 ||
		len(r.Path) == 0 || len(r.Path) > maxRunPath || r.Probes < 0 ||
		r.Probes > CeilingProbes {
		return fmt.Errorf("%w: runbook run needs its label, a known outcome and a "+
			"bounded path", ErrInvalidRequest)
	}
	if _, err := ParseUUID(string(r.RunbookID)); err != nil {
		return err
	}
	if _, err := hashBytes(r.ContentHash); err != nil {
		return err
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{{"runbook name", r.Name, 120}, {"signer", r.SignedBy, 128},
		{"reason", r.Reason, maxRunReason}} {
		if err := checkText(f.name, f.value, false, f.max); err != nil {
			return err
		}
	}
	for _, p := range r.Path {
		if err := checkText("path node", p, true, 48); err != nil {
			return err
		}
	}
	if p := r.Proposal; p != nil && (p.Label != RunbookProposalLabel ||
		checkText("proposal", p.Text, false, 1024) != nil) {
		return fmt.Errorf("%w: runbook proposal needs its label", ErrInvalidRequest)
	}
	return nil
}

// runbookVocab is what definitions may reference: every trigger kind but
// operator starts (a runbook matches a family's trigger).
func runbookVocab() runbook.Vocab {
	var kinds []string
	for k := range triggerKinds {
		if k != TriggerOperator {
			kinds = append(kinds, string(k))
		}
	}
	sort.Strings(kinds)
	return runbook.Vocab{TriggerKinds: kinds}
}

func (d RunbookInput) validate() error {
	if problems := runbook.Validate(d.Definition, runbookVocab()); problems != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, problems)
	}
	switch d.Source {
	case "", SourceManual:
		if d.SourceText != "" || d.CompiledBy != "" {
			return fmt.Errorf("%w: only a compiled draft has source text", ErrInvalidRequest)
		}
	case SourceCompiled:
	default:
		return fmt.Errorf("%w: unknown runbook source %q", ErrInvalidRequest, d.Source)
	}
	if err := checkText("actor", d.Actor, true, 128); err != nil {
		return err
	}
	if err := checkText("compiled by", d.CompiledBy, false, 128); err != nil {
		return err
	}
	if err := runbook.CheckSource(d.SourceText); d.SourceText != "" && err != nil {
		return fmt.Errorf("%w: source text: %v", ErrInvalidRequest, err)
	}
	return nil
}

func (d RunbookInput) source() string {
	if d.Source == "" {
		return SourceManual
	}
	return d.Source
}

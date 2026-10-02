package sre

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Export (AI-SRE-SPEC §9, CHECK-30): a self-contained, versioned and
// redacted document of one investigation: every hypothesis revision,
// the evidence with its hashes, the event chain and what retention
// deleted. It contains no DSN, credential, raw vector or SQL literal.

// ExportSchemaVersion versions the redacted export document.
const ExportSchemaVersion = "pg_sage.sre.investigation_export.v1"

// ExportDocument is the redacted, self-contained export.
type ExportDocument struct {
	SchemaVersion string             `json:"schema_version"`
	ExportedAt    time.Time          `json:"exported_at"`
	Database      string             `json:"database"`
	Investigation Investigation      `json:"investigation"`
	Hypotheses    []HypothesisRecord `json:"hypotheses"`
	Evidence      []EvidenceView     `json:"evidence"`
	Events        []Event            `json:"events"`
	ChainVerified bool               `json:"chain_verified"`
	Tombstones    []Tombstone        `json:"tombstones"`
	Redaction     []string           `json:"redaction"`
}

// Export builds the redacted export of one investigation.
func (s *Service) Export(ctx context.Context, id UUID) (ExportDocument, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return ExportDocument{}, err
	}
	b, err := s.load(ctx, scope, id)
	if err != nil {
		return ExportDocument{}, err
	}
	hs := b.hypotheses
	if hs == nil {
		hs = []HypothesisRecord{}
	}
	doc := ExportDocument{SchemaVersion: ExportSchemaVersion,
		ExportedAt: time.Now().UTC(), Database: s.name, Investigation: b.inv,
		Hypotheses: hs, Evidence: b.evidence, Events: b.events,
		ChainVerified: b.verified, Tombstones: b.tombstones, Redaction: RedactionRules}
	var out ExportDocument
	return out, redactInto(doc, &out)
}

// ExportMarkdown renders the redacted export as Markdown, in the Cases
// panel's order: observed, likely explanation, other explanations, ruled
// out, missing evidence, evidence.
func (s *Service) ExportMarkdown(ctx context.Context, id UUID) (string, error) {
	doc, err := s.Export(ctx, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	inv := doc.Investigation
	fmt.Fprintf(&b, "# Investigation %s\n\n", inv.ID)
	fmt.Fprintf(&b, "- Schema: %s\n- Database: %s\n- Trigger: %s (%s)\n- State: %s\n",
		doc.SchemaVersion, doc.Database, inv.TriggerKind, inv.Subject, inv.State)
	fmt.Fprintf(&b, "- Case: %s\n- Event chain verified: %t\n- Model turns: %d\n\n",
		inv.CaseID, doc.ChainVerified, inv.ModelTurns)
	if inv.Summary.Reason != "" {
		fmt.Fprintf(&b, "Outcome: %s\n\n", inv.Summary.Reason)
	}
	writeFacts(&b, "Observed", inv.Summary.Observed)
	latest, _ := latestRevision(doc.Hypotheses)
	writeHypotheses(&b, "Likely explanation", latest, HypothesisRoot, HypothesisContributing)
	writeHypotheses(&b, "Other explanations", latest, HypothesisUnproven)
	writeHypotheses(&b, "Ruled out", latest, HypothesisRuledOut)
	writeModel(&b, inv.Summary)
	b.WriteString("## Missing evidence\n\n")
	for _, m := range inv.Summary.Missing {
		fmt.Fprintf(&b, "- %s: %s %s\n", m.ProbeID, m.Status, m.Reason)
	}
	b.WriteString("\n## Evidence\n\n")
	for _, e := range doc.Evidence {
		fmt.Fprintf(&b, "- %s %s (%s) sha256 %s\n", e.ID, e.ProbeID, e.CapabilityState,
			e.SHA256)
	}
	for _, t := range doc.Tombstones {
		fmt.Fprintf(&b, "- %s deleted by %s on %s (%d rows)\n", t.Kind, t.Reason,
			t.DeletedAt.Format(time.RFC3339), t.RowCount)
	}
	return b.String(), nil
}

func writeFacts(b *strings.Builder, title string, facts []Fact) {
	fmt.Fprintf(b, "## %s\n\n", title)
	for _, f := range facts {
		fmt.Fprintf(b, "- %s [%s]\n", f.Text, f.EvidenceID)
	}
	b.WriteString("\n")
}

func writeHypotheses(b *strings.Builder, title string, hs []HypothesisRecord,
	statuses ...HypothesisStatus) {
	fmt.Fprintf(b, "## %s\n\n", title)
	want := map[HypothesisStatus]bool{}
	for _, st := range statuses {
		want[st] = true
	}
	for _, h := range hs {
		if !want[h.Status] {
			continue
		}
		fmt.Fprintf(b, "### %s (%s, %s, score %.2f)\n\n", h.Label, h.Subject, h.Status,
			h.Confidence)
		for _, f := range h.Support {
			fmt.Fprintf(b, "- supports: %s [%s]\n", f.Text, f.EvidenceID)
		}
		for _, f := range h.Contradict {
			fmt.Fprintf(b, "- contradicts: %s [%s]\n", f.Text, f.EvidenceID)
		}
		fmt.Fprintf(b, "- refutation probe: %s\n- operator step: %s\n\n",
			h.RefutationProbe, h.OperatorStep)
	}
}

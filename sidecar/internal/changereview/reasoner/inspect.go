package reasoner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// enrichInspection is an inspect_statement result as one citable fact:
// the engine's statement evidence ("inspect:<hash>") with whether it
// parsed and planned, its fingerprint, the workload statement it matches
// (or that it is a new shape), its plan and its rule hits. The label is
// pg_sage's own text.
func enrichInspection(rec *inspection) (changereview.Evidence, string, error) {
	ins := rec.ins
	st := ins.Statement
	id := st.EvidenceID
	if !strings.HasPrefix(id, "inspect:") {
		id = "inspect:" + shortHash(rec.origin, rec.sql)
	}
	data := map[string]any{}
	if base, ok := rec.byID[st.EvidenceID]; ok && len(base.Data) > 0 {
		dec := json.NewDecoder(bytes.NewReader(base.Data))
		dec.UseNumber()
		if err := dec.Decode(&data); err != nil || data == nil {
			data = map[string]any{}
		}
	}
	hits := make([]map[string]string, 0, len(ins.Findings))
	for _, f := range ins.Findings {
		hits = append(hits, map[string]string{"rule_id": f.RuleID, "severity": f.Severity,
			"title": f.Title})
	}
	data["origin"], data["sql"], data["kind"] = rec.origin, rec.sql, st.Kind
	data["parsed"], data["planned"] = ins.Parsed, ins.Planned
	data["parse_error"], data["plan_error"] = ins.ParseError, ins.PlanError
	data["fingerprint"], data["plan_summary"] = st.Fingerprint, st.PlanSummary
	data["relations"], data["rule_hits"] = st.Relations, hits
	data["matched_queryid"] = st.MatchedQueryID
	label := inspectionLabel(ins)
	ev, err := changereview.NewEvidence("inspect", strings.TrimPrefix(id, "inspect:"), label,
		data)
	if err != nil {
		return changereview.Evidence{}, "", fmt.Errorf("inspection evidence: %w", err)
	}
	return ev, label, nil
}

// inspectionLabel is "select: parsed, planned, new shape, 1 rule hit".
func inspectionLabel(ins changereview.Inspection) string {
	parts := []string{"not parsed"}
	if ins.Parsed {
		parts = []string{"parsed", "not planned"}
		if ins.Planned {
			parts[1] = "planned"
		}
		if ins.Statement.MatchedQueryID != nil {
			parts = append(parts, fmt.Sprintf("matches queryid %d",
				*ins.Statement.MatchedQueryID))
		} else {
			parts = append(parts, "new shape")
		}
		parts = append(parts, fmt.Sprintf("%d rule hits", len(ins.Findings)))
	}
	kind := ins.Statement.Kind
	if kind == "" {
		kind = "statement"
	}
	return kind + ": " + strings.Join(parts, ", ")
}

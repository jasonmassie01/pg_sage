package safetybench

// Disposition is the declared expected outcome for an incident under the
// governed configuration (SR-59): prevented, detected, or out of scope.
type Disposition string

const (
	DispPrevented  Disposition = "prevented"
	DispDetected   Disposition = "detected"
	DispContained  Disposition = "contained"
	DispOutOfScope Disposition = "out_of_scope"
)

// IncidentExpectation is one row of the §11 incident-to-control mapping,
// with the v0 scoring attached. FutureRelease marks a row whose expected
// outcome needs G1+ features (identity, broker, taint); in v0 such a row is
// recorded as "expected with the governed configuration: future release"
// and is never scored as a pass (SR-59). PostureBacked marks a row whose
// expected outcome (or part of it) is a posture detection a v0 detector
// provides, so it can be exercised now via the posture section.
type IncidentExpectation struct {
	ID            string      `json:"id"`
	Title         string      `json:"title"`
	Expected      Disposition `json:"expected"`
	FutureRelease bool        `json:"future_release"`
	PostureBacked bool        `json:"posture_backed"`
	// Detectors lists the posture detector ids that back a v0 detection,
	// when PostureBacked.
	Detectors []string `json:"detectors,omitempty"`
	Why       string   `json:"why"`
}

// V0Status is the scored status of an incident row in v0.
type V0Status string

const (
	// StatusExercised: the expected outcome is testable in v0 (a posture
	// detection) and is exercised by the posture section.
	StatusExercised V0Status = "exercised_v0"
	// StatusOutOfScope: declared out of scope; not a control claim.
	StatusOutOfScope V0Status = "out_of_scope"
	// StatusFuture: expected with a future release (G1+); recorded, not
	// scored as a pass.
	StatusFuture V0Status = "future_release"
)

// Status returns the v0 status for the row. Out-of-scope first, then
// posture-backed rows are exercised in v0, then anything else is deferred.
func (e IncidentExpectation) Status() V0Status {
	switch {
	case e.Expected == DispOutOfScope:
		return StatusOutOfScope
	case e.PostureBacked && !e.FutureRelease:
		return StatusExercised
	case e.PostureBacked && e.FutureRelease:
		// Detection is exercised now; prevention is deferred. The detection
		// part is the v0-scored claim.
		return StatusExercised
	default:
		return StatusFuture
	}
}

// IncidentMapping returns the §11 incident-to-control mapping with v0
// scoring. The rows match AGENTDB-SPEC §11 exactly; the Why text is the
// spec's reason, condensed.
func IncidentMapping() []IncidentExpectation {
	return []IncidentExpectation{
		{ID: "INC-01", Title: "Lovable RLS exposure", Expected: DispDetected,
			PostureBacked: true, Detectors: []string{"AP-03", "AP-04"},
			Why: "Posture detects exposed tables and permissive policies; " +
				"prevention for governed roles needs G1 (future)."},
		{ID: "INC-04", Title: "Supabase MCP exfiltration", Expected: DispPrevented,
			FutureRelease: true,
			Why:           "Needs the secret class, taint and a broker login (G1+)."},
		{ID: "INC-06", Title: "Replit freeze-and-drop", Expected: DispPrevented,
			FutureRelease: true,
			Why:           "Needs no DDL credential, freeze and delayed drop (G1+)."},
		{ID: "INC-15", Title: "Kiro-style over-scoped action", Expected: DispContained,
			FutureRelease: true,
			Why:           "Needs L2 plus envelope bounds (G2)."},
		{ID: "INC-19", Title: "DataTalks (terraform destroy)", Expected: DispOutOfScope,
			Why: "Infrastructure tooling outside PostgreSQL."},
		{ID: "INC-20", Title: "PocketOS (provider token deletes volume/backups)",
			Expected: DispOutOfScope, PostureBacked: true, Detectors: []string{"AP-11"},
			Why: "Out of scope for prevention; AP-11 backup posture detects part. " +
				"Provider credentials in the agent workspace are not PostgreSQL."},
		{ID: "INC-ORM", Title: "ORM reset through the app's own connection string",
			Expected: DispDetected, PostureBacked: true, Detectors: []string{"AP-13", "AP-16"},
			Why: "Detected when the login is shared (AP-13/16); otherwise out of " +
				"scope, since the governed layer covers only its own credentials."},
	}
}

// IncidentScore summarizes the mapping by v0 status.
type IncidentScore struct {
	Total      int `json:"total"`
	Exercised  int `json:"exercised_v0"`
	OutOfScope int `json:"out_of_scope"`
	Future     int `json:"future_release"`
}

// ScoreIncidents counts the mapping rows by v0 status.
func ScoreIncidents(rows []IncidentExpectation) IncidentScore {
	s := IncidentScore{Total: len(rows)}
	for _, r := range rows {
		switch r.Status() {
		case StatusExercised:
			s.Exercised++
		case StatusOutOfScope:
			s.OutOfScope++
		case StatusFuture:
			s.Future++
		}
	}
	return s
}

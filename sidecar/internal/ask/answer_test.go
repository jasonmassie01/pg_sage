package ask

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// compose turns a finished loop into the answer a person sees: only the
// claims that survived the citation filter, each with its evidence; the
// model's "not observed" notes (never with numbers, which are claims);
// what was dropped and why; and a status that says whether anything could
// be verified. Unit tests: no database, no model.

func ev(id, text string) agentloop.Evidence {
	return agentloop.Evidence{ID: id, Digest: digestOf(text), Label: id + " ok", Text: text}
}

func finalWith(t *testing.T, notes ...string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"claims": []any{}, "not_observed": notes})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCompose_AnsweredKeepsOnlySurvivingClaimsWithCitations(t *testing.T) {
	cited := map[string]agentloop.Evidence{"finding:42": ev("finding:42", "4200 seq scans")}
	res := agentloop.Result{Final: finalWith(t),
		Claims: []agentloop.Claim{{Text: "public.orders had 4200 sequential scans.",
			EvidenceIDs: []string{"finding:42"}}},
		Dropped: []agentloop.DroppedClaim{{Text: "It will be 10x faster.",
			Reason: agentloop.DropUncited}},
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal, Tokens: 900}}
	a := compose("testdb", res, cited, nil)
	if a.Status != StatusAnswered || len(a.Statements) != 1 || a.Tokens != 900 ||
		a.Stop != agentloop.StopFinal {
		t.Fatalf("answer = %+v", a)
	}
	if got := a.Statements[0].Citations; len(got) != 1 || got[0] != "finding:42" {
		t.Fatalf("citations = %v", got)
	}
	if len(a.Citations) != 1 || a.Citations[0].Kind != "finding" || a.Citations[0].Ref != "42" ||
		a.Citations[0].APIPath != "/api/v1/findings/42?database=testdb" ||
		a.Citations[0].Digest != digestOf("4200 seq scans") {
		t.Fatalf("citation = %+v", a.Citations)
	}
	if len(a.Dropped) != 1 || a.Dropped[0].Reason != agentloop.DropUncited {
		t.Fatalf("dropped = %+v", a.Dropped)
	}
	if strings.Contains(a.Text, "10x faster") {
		t.Fatalf("a dropped claim reached the answer text: %q", a.Text)
	}
	if !strings.Contains(a.Text, "public.orders had 4200 sequential scans. [1]") ||
		!strings.Contains(a.Text, "1 statement was dropped") {
		t.Fatalf("text = %q", a.Text)
	}
}

func TestCompose_NothingVerifiedIsNotObserved(t *testing.T) {
	res := agentloop.Result{Final: finalWith(t, "No finding mentions public.invoices."),
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}
	a := compose("testdb", res, nil, nil)
	if a.Status != StatusNotObserved || len(a.Statements) != 0 {
		t.Fatalf("answer = %+v", a)
	}
	if len(a.NotVerified) != 1 || a.NotVerified[0] != "No finding mentions public.invoices." {
		t.Fatalf("not verified = %v", a.NotVerified)
	}
	if !strings.Contains(a.Text, "could not verify") ||
		!strings.Contains(a.Text, "No finding mentions public.invoices.") {
		t.Fatalf("text = %q", a.Text)
	}
}

func TestCompose_NotesWithNumbersAreDropped(t *testing.T) {
	long := strings.Repeat("x", MaxNoteRunes+1)
	res := agentloop.Result{Final: finalWith(t, "There are 57 missing indexes.",
		"Vacuum ran 3x more.", "Nothing about replication was observed.", "  ", long,
		"Nothing about replication was observed."),
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}
	a := compose("testdb", res, nil, nil)
	if len(a.NotVerified) != 1 || a.NotVerified[0] != "Nothing about replication was observed." {
		t.Fatalf("not verified = %q", a.NotVerified)
	}
	reasons := map[string]int{}
	for _, d := range a.Dropped {
		reasons[d.Reason]++
	}
	if reasons[DropNoteNumbers] != 2 || reasons[DropNoteTooLong] != 1 {
		t.Fatalf("dropped = %+v", a.Dropped)
	}
}

func TestCompose_TooManyNotesAreCapped(t *testing.T) {
	notes := []string{"a one", "b two", "c three", "d four", "e five", "f six", "g seven"}
	a := compose("testdb", agentloop.Result{Final: finalWith(t, notes...),
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}, nil, nil)
	if len(a.NotVerified) != MaxNotes {
		t.Fatalf("kept %d notes, want %d", len(a.NotVerified), MaxNotes)
	}
}

func TestCompose_StopsMapToStatuses(t *testing.T) {
	cases := map[string]string{
		agentloop.StopBudget:      StatusBudget,
		agentloop.StopDisabled:    StatusNoModel,
		agentloop.StopRateLimited: StatusIncomplete,
		agentloop.StopTimeout:     StatusIncomplete,
		agentloop.StopMaxSteps:    StatusIncomplete,
		agentloop.StopWall:        StatusIncomplete,
		agentloop.StopTokens:      StatusIncomplete,
		agentloop.StopMalformed:   StatusIncomplete,
		agentloop.StopProvider:    StatusIncomplete,
	}
	for stop, want := range cases {
		a := compose("testdb", agentloop.Result{Transcript: agentloop.Transcript{Stop: stop}},
			nil, nil)
		if a.Status != want || a.Stop != stop || len(a.Statements) != 0 {
			t.Errorf("stop %s: status %s, want %s", stop, a.Status, want)
		}
		if a.Text == "" {
			t.Errorf("stop %s: empty text", stop)
		}
	}
}

func TestCompose_UnfinishedRunKeepsNoClaims(t *testing.T) {
	// A run that stopped without the final answer has no claims, even if
	// the loop result carried some (defense in depth).
	res := agentloop.Result{Claims: []agentloop.Claim{{Text: "x", EvidenceIDs: []string{"a:1"}}},
		Transcript: agentloop.Transcript{Stop: agentloop.StopMaxSteps}}
	if a := compose("testdb", res, map[string]agentloop.Evidence{"a:1": ev("a:1", "x")},
		nil); len(a.Statements) != 0 || a.Status != StatusIncomplete {
		t.Fatalf("answer = %+v", a)
	}
}

func TestCompose_ClaimCitingUnknownEvidenceIsDropped(t *testing.T) {
	// The loop maps aliases to ids; an id compose does not know (a bug or a
	// tampered result) never becomes a citation.
	res := agentloop.Result{Final: finalWith(t),
		Claims:     []agentloop.Claim{{Text: "made up", EvidenceIDs: []string{"finding:9"}}},
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}
	a := compose("testdb", res, map[string]agentloop.Evidence{}, nil)
	if len(a.Statements) != 0 || len(a.Citations) != 0 || a.Status != StatusNotObserved {
		t.Fatalf("answer = %+v", a)
	}
	if len(a.Dropped) != 1 || a.Dropped[0].Reason != agentloop.DropUnknownEvidence {
		t.Fatalf("dropped = %+v", a.Dropped)
	}
}

func TestCompose_ActionsAreReportedEvenWithoutClaims(t *testing.T) {
	acts := []ActionTaken{{Kind: ActionProposal, ID: "77", Status: ActionQueued,
		Verdict: "queue_approval"}}
	a := compose("testdb", agentloop.Result{Final: finalWith(t),
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}, nil, acts)
	if len(a.Actions) != 1 || a.Actions[0].ID != "77" ||
		!strings.Contains(a.Text, "proposal 77") {
		t.Fatalf("answer = %+v", a)
	}
}

func TestCitationFor_APIPathsPerKind(t *testing.T) {
	cases := map[string]string{
		"finding:42":            "/api/v1/findings/42?database=db1",
		"findings:open":         "/api/v1/findings?database=db1&status=open",
		"action:7":              "/api/v1/actions/7?database=db1",
		"actions:recent":        "/api/v1/actions?database=db1",
		"approvals:pending":     "/api/v1/actions/pending?database=db1",
		"proposal:77":           "/api/v1/actions/pending?database=db1",
		"trust:db1":             "/api/v1/trust?database=db1",
		"facts:confirmed":       "/api/v1/facts?database=db1&status=confirmed",
		"facts:all":             "/api/v1/facts?database=db1",
		"incidents:active":      "/api/v1/incidents?database=db1",
		"investigation:abc-123": "/api/v1/databases/db1/investigations/abc-123",
		"investigations:recent": "/api/v1/databases/db1/investigations",
		"config:ask.enabled":    "",
		"doc:trust_levels":      "",
		"table:public.orders":   "",
		"queries:total_time":    "",
	}
	for id, want := range cases {
		c := citationFor("db1", ev(id, "t"))
		kind, ref, _ := strings.Cut(id, ":")
		if c.ID != id || c.Kind != kind || c.Ref != ref || c.APIPath != want {
			t.Errorf("%s: %+v, want path %q", id, c, want)
		}
	}
	if c := citationFor("db 1&x", ev("finding:1", "t")); c.APIPath !=
		"/api/v1/findings/1?database=db+1%26x" {
		t.Errorf("database not escaped: %s", c.APIPath)
	}
}

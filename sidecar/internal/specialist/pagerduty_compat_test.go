package specialist

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The PagerDuty adapter is unaffected by contract revision 1.1.0: it never
// scopes an investigation to a statement, and the incident note it posts
// is exactly the v1 note (the investigator's model claims and the query
// scope never reach the incident).

func TestPagerDuty_RequestCarriesNoQueryScope(t *testing.T) {
	h, hs := adapterHandler(t)
	body := pdEvent("incident.triggered", "query_id 42 is slow", "PSVC123")
	w := postAdapter(h, base+"/adapters/pagerduty", "propose-token", body,
		map[string]string{"X-PagerDuty-Signature": pdSign(pdSecret, body)})
	if w.Code != 201 {
		t.Fatalf("pagerduty %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "query_scope") {
		t.Fatalf("the open response carries no scope: %s", w.Body.String())
	}
	rec := hs.store.all()[0]
	if rec.Query != nil || hs.orders.started[0].Subject != externalSubject ||
		len(hs.orders.hashCalls) != 0 {
		t.Fatalf("the incident title never scopes the investigation: %+v %+v", rec,
			hs.orders.started)
	}
}

func TestPagerDuty_NoteIsTheV1Note(t *testing.T) {
	plain := MapResult(Snapshot{Detail: lockDetail()}, MapOptions{KeepIdentifiers: true,
		Now: created})
	d := withInvestigator(conclusion(sre.ModelContested, modelRootNode, graphRootNode,
		sre.ContestAdvisory, "not earned"))
	rec := &Record{Query: &QueryScope{QueryID: "42", Applied: QueryAppliedEvidence}}
	rich := MapResult(Snapshot{Detail: d, Record: rec}, MapOptions{KeepIdentifiers: true,
		Now: created})
	if rich.Investigator == nil || rich.QueryScope == nil {
		t.Fatal("fixture must carry the new fields")
	}
	if noteText(rich) != noteText(plain) {
		t.Fatalf("the note changed:\n%s\n---\n%s", noteText(rich), noteText(plain))
	}
	for _, leak := range []string{"two old transactions", "sk-abcdefghijklmnopqrstuv",
		"queryid", "investigator"} {
		if strings.Contains(noteText(rich), leak) {
			t.Fatalf("%q reached the incident note", leak)
		}
	}
}

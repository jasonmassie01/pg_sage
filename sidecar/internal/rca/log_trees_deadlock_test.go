package rca

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rca-07b: a deadlock incident names its wait cycle (who waited for which
// lock, held by whom) and the fingerprints of the statements that collided.

// No concurrent access tests: the tree and the merge build new values from
// immutable inputs; the engine lock is covered by the rca-07 tests.

func twoParticipants() []DeadlockParticipant {
	return []DeadlockParticipant{
		{PID: 4242, WaitMode: "ShareLock", WaitObject: "transaction 745", BlockedBy: 4243,
			Relation: "accounts", Fingerprint: "0123456789abcdef"},
		{PID: 4243, WaitMode: "ShareLock", WaitObject: "transaction 744", BlockedBy: 4242,
			Fingerprint: "fedcba9876543210"},
	}
}

func participantSignal(parts []DeadlockParticipant, count int, first, last time.Time) *Signal {
	s := occSignal(count, []int{4242, 4243}, first, last)
	if parts != nil {
		s.Metrics[MetricDeadlockParticipants] = parts
	}
	return s
}

// Happy path: the chain gains one evidence line per participant.
func TestLogTreeDeadlock_EvidenceNamesWaitCycle(t *testing.T) {
	inc := logTreeDeadlock(participantSignal(twoParticipants(), 1, occT0, occT0))
	if len(inc.CausalChain) != 2 {
		t.Fatalf("chain = %+v, want the deadlock link and the wait-cycle link", inc.CausalChain)
	}
	ev := inc.CausalChain[1].Evidence
	for _, want := range []string{
		`pid 4242 waits for ShareLock on transaction 745 (relation "accounts") ` +
			`held by pid 4243; statement fp=0123456789abcdef`,
		"pid 4243 waits for ShareLock on transaction 744 held by pid 4242; " +
			"statement fp=fedcba9876543210",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence %q lacks %q", ev, want)
		}
	}
	if first := inc.CausalChain[0].Evidence; !strings.Contains(first, "pids=4242,4243") {
		t.Errorf("rca-07 evidence changed: %q", first)
	}
}

// A participant known only from its statement line, or without one, says
// what is known and never prints an empty fingerprint.
func TestLogTreeDeadlock_PartialParticipants(t *testing.T) {
	parts := []DeadlockParticipant{{PID: 7, Fingerprint: "aaaaaaaaaaaaaaaa"},
		{PID: 8, WaitMode: "ExclusiveLock", WaitObject: "relation 1 of database 2", BlockedBy: 7}}
	ev := logTreeDeadlock(participantSignal(parts, 1, occT0, occT0)).CausalChain[1].Evidence
	if !strings.Contains(ev, "pid 7; statement fp=aaaaaaaaaaaaaaaa") ||
		!strings.Contains(ev, "pid 8 waits for ExclusiveLock on relation 1 of database 2 "+
			"held by pid 7") || strings.Contains(ev, "fp=;") || strings.HasSuffix(ev, "fp=") {
		t.Errorf("evidence = %q", ev)
	}
}

// Nil/empty: no participants keep today's one-link PID-only evidence;
// unparsed lines are counted in it.
func TestLogTreeDeadlock_NoParticipantsKeepsPIDEvidence(t *testing.T) {
	s := participantSignal(nil, 2, occT0, occT0)
	inc := logTreeDeadlock(s)
	if len(inc.CausalChain) != 1 || !strings.Contains(inc.CausalChain[0].Evidence,
		"pids=4242,4243, occurrences=2") {
		t.Fatalf("chain = %+v", inc.CausalChain)
	}
	s.Metrics[MetricParticipantsUnparsed] = 3
	if ev := logTreeDeadlock(s).CausalChain[0].Evidence; !strings.Contains(ev,
		"participants_unparsed=3") {
		t.Errorf("evidence %q does not count the unparsed DETAIL lines", ev)
	}
}

// The signal's participants reach the incident's occurrences; fingerprint
// mode stores "statement" absent, not empty, and no participants means no
// "participants" key at all.
func TestLogOccurrences_ParticipantsFromSignalAndJSON(t *testing.T) {
	s := participantSignal(twoParticipants(), 1, occT0, occT0)
	s.Metrics[MetricParticipantsUnparsed] = 1
	occ := logOccurrencesFromSignal(s)
	if len(occ.Participants) != 2 || occ.Participants[1].PID != 4243 ||
		occ.ParticipantsUnparsed != 1 {
		t.Fatalf("occurrences = %+v", occ)
	}
	raw, err := json.Marshal(occ)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"statement"`) || !strings.Contains(string(raw),
		`"participants":[{"pid":4242,"wait_mode":"ShareLock"`) {
		t.Errorf("stored json = %s", raw)
	}
	bare, _ := json.Marshal(logOccurrencesFromSignal(participantSignal(nil, 1, occT0, occT0)))
	if strings.Contains(string(bare), "participants") {
		t.Errorf("no participants, yet json = %s", bare)
	}
}

// Invalid input: a participants metric of the wrong type is ignored, and
// more than ten participants are capped.
func TestLogOccurrences_ParticipantsMetricGuards(t *testing.T) {
	s := participantSignal(nil, 1, occT0, occT0)
	s.Metrics[MetricDeadlockParticipants] = []string{"pid 1"}
	if occ := logOccurrencesFromSignal(s); occ.Participants != nil {
		t.Errorf("wrong-typed metric gave participants %+v", occ.Participants)
	}
	var many []DeadlockParticipant
	for i := 0; i < 12; i++ {
		many = append(many, DeadlockParticipant{PID: 100 + i})
	}
	s.Metrics[MetricDeadlockParticipants] = many
	if got := logOccurrencesFromSignal(s).Participants; len(got) != MaxDeadlockParticipants ||
		MaxDeadlockParticipants != 10 {
		t.Errorf("participants = %d (cap %d), want 10", len(got), MaxDeadlockParticipants)
	}
}

// State transition: a recurrence in a new window replaces the participant
// set and sums the count; an older or participant-less report keeps it.
func TestMergeLogOccurrences_NewWindowReplacesParticipants(t *testing.T) {
	a := logOccurrencesFromSignal(participantSignal(twoParticipants(), 3, occT0,
		occT0.Add(time.Minute)))
	newer := []DeadlockParticipant{{PID: 9001, WaitMode: "ExclusiveLock", BlockedBy: 9002},
		{PID: 9002, Fingerprint: "1111111111111111"}}
	b := logOccurrencesFromSignal(participantSignal(newer, 2, occT0.Add(time.Hour),
		occT0.Add(time.Hour+time.Second)))
	m := mergeLogOccurrences(a, b)
	if m.Count != 5 || len(m.Participants) != 2 || m.Participants[0].PID != 9001 {
		t.Fatalf("merged = count %d participants %+v; want 5 and the new set", m.Count,
			m.Participants)
	}
	if a.Participants[0].PID != 4242 {
		t.Errorf("merge mutated its input: %+v", a.Participants)
	}
	none := logOccurrencesFromSignal(participantSignal(nil, 1, occT0.Add(2*time.Hour),
		occT0.Add(2*time.Hour)))
	if got := mergeLogOccurrences(m, none); got.Count != 6 || got.Participants[0].PID != 9001 {
		t.Errorf("participant-less recurrence: count %d participants %+v", got.Count,
			got.Participants)
	}
	older := logOccurrencesFromSignal(participantSignal(twoParticipants(), 1,
		occT0.Add(-time.Hour), occT0.Add(-time.Hour)))
	if got := mergeLogOccurrences(m, older); got.Participants[0].PID != 9001 {
		t.Errorf("an older window replaced the newest set: %+v", got.Participants)
	}
	if got := mergeLogOccurrences(nil, b); got.Participants[0].PID != 9001 ||
		&got.Participants[0] == &b.Participants[0] {
		t.Errorf("merge into nil = %+v, want a copy of b's set", got.Participants)
	}
}

// The tier-2 prompt never sees statement text in fingerprint mode: the
// participants pass through as built (no Statement field set).
func TestRedactSignalMetrics_ParticipantsCarryNoStatement(t *testing.T) {
	s := participantSignal(twoParticipants(), 1, occT0, occT0)
	s.Metrics["detail"] = "Process 4242: UPDATE users SET email = 'alice@example.com'"
	raw, err := json.Marshal(redactSignalMetrics(s.Metrics))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "alice") || strings.Contains(string(raw), `"statement"`) {
		t.Errorf("prompt metrics = %s", raw)
	}
	if !strings.Contains(string(raw), "0123456789abcdef") {
		t.Errorf("prompt metrics lost the fingerprints: %s", raw)
	}
}

func TestDeadlockEvidenceLine_Format(t *testing.T) {
	p := DeadlockParticipant{PID: 1, WaitMode: "ShareLock", WaitObject: "transaction 2",
		BlockedBy: 3}
	want := "pid 1 waits for ShareLock on transaction 2 held by pid 3"
	if got := p.EvidenceLine(); got != want {
		t.Errorf("EvidenceLine() = %q, want %q", got, want)
	}
	if got := (DeadlockParticipant{PID: 5}).EvidenceLine(); got != fmt.Sprintf("pid %d", 5) {
		t.Errorf("bare participant line = %q", got)
	}
}

package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The decision log carries the wait: the ledger stamps what the gate saw,
// and a request's own evidence can never set (or erase) those keys.

func waitFor(id int64) policy.PendingVerification {
	at := time.Date(2026, 10, 4, 18, 10, 0, 0, time.UTC)
	return policy.PendingVerification{ActionID: id, Object: "guc:work_mem", Until: at,
		HardDeadline: at.Add(72 * time.Hour)}
}

func TestLedgerRecordsTheVerificationWait(t *testing.T) {
	req := findingRequest(waitGUCFinding("work_mem", "10MB"), false)
	req.Evidence = map[string]any{"verification_wait": "spoofed",
		"verification_override": "spoofed", "verification_wait_released": "spoofed",
		"verification_wait_detail": "spoofed"}
	detail := policy.WaitDetail([]policy.PendingVerification{waitFor(6407)})
	parked := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonAwaitingVerification, Detail: detail,
		VerificationWait: &policy.VerificationWait{
			Pending: []policy.PendingVerification{waitFor(6407)}}})
	if parked.Reason != string(policy.ReasonAwaitingVerification) ||
		fmt.Sprint(evidenceActionIDs(parked.Evidence, "verification_wait")) != "[6407]" ||
		parked.Evidence["verification_wait_detail"] != detail {
		t.Fatalf("parked input %+v", parked)
	}
	for _, key := range []string{"verification_override", "verification_wait_released"} {
		if _, ok := parked.Evidence[key]; ok {
			t.Fatalf("spoofed %s kept: %v", key, parked.Evidence[key])
		}
	}
	entry := parked.Evidence["verification_wait"].([]any)[0].(map[string]any)
	if entry["object"] != "guc:work_mem" || entry["until"] != "2026-10-04T18:10:00Z" ||
		entry["hard_deadline"] != "2026-10-07T18:10:00Z" {
		t.Fatalf("wait entry %v", entry)
	}
}

func TestLedgerRecordsOverrideAndRelease(t *testing.T) {
	req := findingRequest(waitGUCFinding("work_mem", "10MB"), false)
	req.OperatorApproved = true
	over := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonOperatorApproved, VerificationWait: &policy.VerificationWait{
			Pending: []policy.PendingVerification{waitFor(6407)}, Overridden: true}})
	if fmt.Sprint(evidenceActionIDs(over.Evidence, "verification_override")) != "[6407]" {
		t.Fatalf("override evidence %v", over.Evidence)
	}
	if _, ok := over.Evidence["verification_wait"]; ok {
		t.Fatalf("an override is not a wait: %v", over.Evidence)
	}
	released := ledgerInput(nil, 1, findingRequest(waitGUCFinding("work_mem", "10MB"), false),
		policy.Decision{Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized,
			VerificationWait: &policy.VerificationWait{
				Released: []policy.PendingVerification{waitFor(6400)}}})
	if fmt.Sprint(evidenceActionIDs(released.Evidence, "verification_wait_released")) !=
		"[6400]" {
		t.Fatalf("release evidence %v", released.Evidence)
	}
	plain := ledgerInput(nil, 1, findingRequest(waitGUCFinding("work_mem", "10MB"), false),
		policy.Decision{Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized})
	for _, key := range []string{"verification_wait", "verification_override",
		"verification_wait_released", "verification_wait_detail"} {
		if _, ok := plain.Evidence[key]; ok {
			t.Fatalf("no wait, yet %s recorded: %v", key, plain.Evidence)
		}
	}
}

// Every park verdict is counted by its reason; overrides and hard-deadline
// releases by cause. Counting is safe from concurrent recorders.
func TestParkAndReleaseCounters(t *testing.T) {
	db := fmt.Sprintf("counter_%d", time.Now().UnixNano())
	countDecision(db, policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonRateLimitExceeded})
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			countDecision(db, policy.Decision{Verdict: policy.VerdictPark,
				Reason: policy.ReasonAwaitingVerification})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	countDecision(db, policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAuthorized})
	countDecision(db, policy.Decision{Verdict: policy.VerdictExecute,
		VerificationWait: &policy.VerificationWait{Overridden: true,
			Pending: []policy.PendingVerification{waitFor(1)}}})
	countDecision(db, policy.Decision{Verdict: policy.VerdictExecute,
		VerificationWait: &policy.VerificationWait{
			Released: []policy.PendingVerification{waitFor(2), waitFor(3)}}})
	parks := map[string]int64{}
	for _, c := range ParkCounts() {
		if c.Database == db {
			parks[c.Reason] = c.Count
		}
	}
	if len(parks) != 2 || parks["awaiting_verification"] != 4 ||
		parks["rate_limit_exceeded"] != 1 {
		t.Fatalf("parks %v", parks)
	}
	causes := map[string]int64{}
	for _, c := range WaitReleaseCounts() {
		if c.Database == db {
			causes[c.Cause] = c.Count
		}
	}
	if causes["operator_override"] != 1 || causes["hard_deadline"] != 2 || len(causes) != 2 {
		t.Fatalf("releases %v", causes)
	}
}

// An unnamed executor's decisions are not exported: no series without a
// database (the fleet metrics forbid database="").
func TestCountersSkipAnUnnamedExecutor(t *testing.T) {
	countDecision("", policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonAwaitingVerification})
	countDecision("", policy.Decision{Verdict: policy.VerdictExecute,
		VerificationWait: &policy.VerificationWait{Overridden: true,
			Pending: []policy.PendingVerification{waitFor(1)}}})
	for _, c := range ParkCounts() {
		if c.Database == "" {
			t.Fatalf("park series without a database: %+v", c)
		}
	}
	for _, c := range WaitReleaseCounts() {
		if c.Database == "" {
			t.Fatalf("release series without a database: %+v", c)
		}
	}
}

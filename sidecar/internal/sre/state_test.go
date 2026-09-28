package sre

import (
	"strings"
	"testing"
	"time"
)

// Investigation lifecycle (Codex §7): queued -> collecting -> evaluating
// -> needs_evidence | concluded | inconclusive; needs_evidence ->
// collecting; any active state may become paused, cancelled, expired or
// failed; resume re-queues a paused investigation.

func TestCanTransition_Table(t *testing.T) {
	allowed := map[State][]State{
		StateQueued: {StateCollecting, StatePaused, StateCancelled, StateExpired,
			StateFailed},
		StateCollecting: {StateCollecting, StateEvaluating, StatePaused, StateCancelled,
			StateExpired, StateFailed},
		StateEvaluating: {StateNeedsEvidence, StateConcluded, StateInconclusive,
			StatePaused, StateCancelled, StateExpired, StateFailed},
		StateNeedsEvidence: {StateCollecting, StatePaused, StateCancelled, StateExpired,
			StateFailed},
		StatePaused: {StateQueued, StateCancelled, StateExpired},
	}
	all := AllStates()
	if len(all) != 10 {
		t.Fatalf("AllStates = %v, want 10 states", all)
	}
	for _, from := range all {
		ok := map[State]bool{}
		for _, to := range allowed[from] {
			ok[to] = true
		}
		for _, to := range all {
			if got := CanTransition(from, to); got != ok[to] {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, ok[to])
			}
		}
	}
	if CanTransition("bogus", StateQueued) || CanTransition(StateQueued, "bogus") {
		t.Fatal("unknown states must never transition")
	}
}

func TestState_TerminalAndLive(t *testing.T) {
	for _, s := range []State{StateConcluded, StateInconclusive, StateCancelled,
		StateExpired, StateFailed} {
		if !s.Terminal() || s.Live() {
			t.Errorf("%s: terminal=%v live=%v", s, s.Terminal(), s.Live())
		}
	}
	for _, s := range []State{StateQueued, StateCollecting, StateEvaluating,
		StateNeedsEvidence, StatePaused} {
		if s.Terminal() || !s.Live() {
			t.Errorf("%s: terminal=%v live=%v", s, s.Terminal(), s.Live())
		}
	}
}

func TestUUID_NewAndParse(t *testing.T) {
	a, b := NewUUID(), NewUUID()
	if a == b {
		t.Fatal("NewUUID repeated a value")
	}
	for _, u := range []UUID{a, b} {
		if _, err := ParseUUID(string(u)); err != nil {
			t.Fatalf("ParseUUID(%s): %v", u, err)
		}
		if s := string(u); len(s) != 36 || s[14] != '4' {
			t.Fatalf("%s is not a canonical v4 UUID", s)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", strings.Repeat("a", 36),
		"40BFC229-BD01-4EC4-A7DC-C7DBF323B933x",
		"40bfc229bd014ec4a7dcc7dbf323b933"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Errorf("ParseUUID(%q) accepted", bad)
		}
	}
	upper, err := ParseUUID("40BFC229-BD01-4EC4-A7DC-C7DBF323B933")
	if err != nil || upper != "40bfc229-bd01-4ec4-a7dc-c7dbf323b933" {
		t.Fatalf("ParseUUID must canonicalize to lower case: %q %v", upper, err)
	}
}

func TestScope_Validate(t *testing.T) {
	good := Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid scope: %v", err)
	}
	for _, s := range []Scope{{}, {DeploymentID: good.DeploymentID},
		{DatabaseID: good.DatabaseID}, {DeploymentID: "x", DatabaseID: good.DatabaseID}} {
		if err := s.Validate(); err == nil {
			t.Errorf("scope %+v accepted", s)
		}
	}
}

// Limits are hard ceilings (Codex §5 contract): configuration may only
// tighten them, and zero or negative values are rejected, never treated
// as "unlimited".
func TestLimits_DefaultsAreTheR1Ceilings(t *testing.T) {
	l := DefaultLimits()
	if l.MaxActive != 120*time.Second || l.MaxProbes != 12 || l.MaxModelTurns != 2 ||
		l.MaxInputTokens != 16000 || l.MaxOutputTokens != 4000 ||
		l.QueueExpiry != 10*time.Minute || l.LeaseTTL <= 0 ||
		l.LeaseTTL > l.MaxActive {
		t.Fatalf("defaults = %+v", l)
	}
	if l.DatabaseDailyTokens != 0 || l.DeploymentDailyTokens != 0 {
		t.Fatal("no daily model allocation may be assumed by default")
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestLimits_ValidateBoundaries(t *testing.T) {
	cases := map[string]func(*Limits){
		"zero active":       func(l *Limits) { l.MaxActive = 0 },
		"active over cap":   func(l *Limits) { l.MaxActive = 120*time.Second + 1 },
		"zero probes":       func(l *Limits) { l.MaxProbes = 0 },
		"probes over cap":   func(l *Limits) { l.MaxProbes = 13 },
		"negative turns":    func(l *Limits) { l.MaxModelTurns = -1 },
		"turns over cap":    func(l *Limits) { l.MaxModelTurns = 3 },
		"input over cap":    func(l *Limits) { l.MaxInputTokens = 16001 },
		"zero input":        func(l *Limits) { l.MaxInputTokens = 0 },
		"output over cap":   func(l *Limits) { l.MaxOutputTokens = 4001 },
		"zero lease":        func(l *Limits) { l.LeaseTTL = 0 },
		"lease over active": func(l *Limits) { l.LeaseTTL = l.MaxActive + time.Second },
		"zero queue expiry": func(l *Limits) { l.QueueExpiry = 0 },
		"negative daily db": func(l *Limits) { l.DatabaseDailyTokens = -1 },
		"negative daily":    func(l *Limits) { l.DeploymentDailyTokens = -1 },
		"db over deploy": func(l *Limits) {
			l.DatabaseDailyTokens, l.DeploymentDailyTokens = 200, 100
		},
	}
	for name, mutate := range cases {
		l := DefaultLimits()
		mutate(&l)
		if err := l.Validate(); err == nil {
			t.Errorf("%s accepted: %+v", name, l)
		}
	}
	tight := DefaultLimits()
	tight.MaxActive, tight.MaxProbes, tight.MaxModelTurns = time.Second, 1, 1
	tight.LeaseTTL = time.Second
	tight.MaxInputTokens, tight.MaxOutputTokens = 1, 1
	tight.DatabaseDailyTokens, tight.DeploymentDailyTokens = 10, 10
	if err := tight.Validate(); err != nil {
		t.Fatalf("tighter positive limits must be accepted: %v", err)
	}
}

func TestStartRequest_FingerprintIsScopedAndStable(t *testing.T) {
	scope := Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()}
	a := StartRequest{Scope: scope, CaseID: "case:1", TriggerKind: TriggerLock,
		Subject: "pid 10"}
	b := a
	b.CaseID = "case:2" // same trigger from another case coalesces
	if string(a.Fingerprint()) != string(b.Fingerprint()) {
		t.Fatal("fingerprint depends on the case id")
	}
	c := a
	c.Subject = "pid 11"
	d := a
	d.Scope.DatabaseID = NewUUID()
	e := a
	e.TriggerKind = TriggerConnections
	for name, other := range map[string]StartRequest{"subject": c,
		"database": d, "kind": e} {
		if string(other.Fingerprint()) == string(a.Fingerprint()) {
			t.Errorf("fingerprint ignores %s", name)
		}
	}
	if len(a.Fingerprint()) != 32 {
		t.Fatal("fingerprint must be 32 bytes (sha256)")
	}
}

func TestStartRequest_Validate(t *testing.T) {
	scope := Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()}
	good := StartRequest{Scope: scope, CaseID: "case:1", TriggerKind: TriggerLock,
		Subject: "pid 10", IdempotencyKey: "k1"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := map[string]func(*StartRequest){
		"bad scope":     func(r *StartRequest) { r.Scope = Scope{} },
		"no case":       func(r *StartRequest) { r.CaseID = "" },
		"unknown kind":  func(r *StartRequest) { r.TriggerKind = "shell" },
		"long key":      func(r *StartRequest) { r.IdempotencyKey = strings.Repeat("k", 161) },
		"long subject":  func(r *StartRequest) { r.Subject = strings.Repeat("s", 257) },
		"long case":     func(r *StartRequest) { r.CaseID = strings.Repeat("c", 257) },
		"control chars": func(r *StartRequest) { r.CaseID = "case\x00;drop" },
	}
	for name, mutate := range cases {
		r := good
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

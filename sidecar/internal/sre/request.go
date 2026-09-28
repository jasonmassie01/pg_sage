package sre

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TriggerKind is the incident family that starts an investigation.
type TriggerKind string

// Trigger kinds (R1 families plus plan regression and operator starts).
const (
	TriggerLock        TriggerKind = "lock_blocking"
	TriggerConnections TriggerKind = "connection_pressure"
	TriggerWAL         TriggerKind = "wal_retention"
	TriggerPlan        TriggerKind = "plan_regression"
	TriggerOperator    TriggerKind = "operator"
)

var triggerKinds = map[TriggerKind]bool{TriggerLock: true, TriggerConnections: true,
	TriggerWAL: true, TriggerPlan: true, TriggerOperator: true}

// StartRequest asks for an investigation of one scoped trigger.
type StartRequest struct {
	Scope          Scope
	CaseID         string
	IncidentID     string
	TriggerKind    TriggerKind
	Subject        string // what the trigger is about, e.g. "pid 4242"
	IdempotencyKey string // optional; repeats return the same investigation
}

// Fingerprint identifies the trigger within its scope: duplicates and
// out-of-order repeats of the same trigger coalesce, whatever case they
// arrive from; another database or subject is another trigger.
func (r StartRequest) Fingerprint() []byte {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(r.Scope.DeploymentID), string(r.Scope.DatabaseID),
		string(r.TriggerKind), r.Subject}, "\x00")))
	return sum[:]
}

// Validate checks the request before any store I/O.
func (r StartRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !triggerKinds[r.TriggerKind] {
		return fmt.Errorf("%w: unknown trigger kind %q", ErrInvalidRequest, r.TriggerKind)
	}
	fields := []struct {
		name     string
		value    string
		required bool
		max      int
	}{
		{"case id", r.CaseID, true, 256},
		{"incident id", r.IncidentID, false, 256},
		{"subject", r.Subject, false, 256},
		{"idempotency key", r.IdempotencyKey, false, 160},
	}
	for _, f := range fields {
		if err := checkText(f.name, f.value, f.required, f.max); err != nil {
			return err
		}
	}
	return nil
}

func checkText(name, value string, required bool, max int) error {
	switch {
	case value == "" && required:
		return fmt.Errorf("%w: %s is required", ErrInvalidRequest, name)
	case utf8.RuneCountInString(value) > max:
		return fmt.Errorf("%w: %s longer than %d characters", ErrInvalidRequest, name, max)
	case strings.IndexFunc(value, unicode.IsControl) >= 0:
		return fmt.Errorf("%w: %s contains control characters", ErrInvalidRequest, name)
	}
	return nil
}

// IdentityStrength is how strongly a database binding is tied to a
// physical server (Codex contracts §1).
type IdentityStrength string

// Identity strengths.
const (
	StrengthConfigured IdentityStrength = "configured"
	StrengthProvider   IdentityStrength = "provider"
	StrengthCluster    IdentityStrength = "cluster"
)

// Binding maps a runtime connection entry to a stable database UUID.
type Binding struct {
	DeploymentID     UUID
	RuntimeKey       string
	LegacyDatabaseID *int
	Strength         IdentityStrength
	ClusterEpoch     string
}

func (b Binding) validate() error {
	if _, err := ParseUUID(string(b.DeploymentID)); err != nil {
		return err
	}
	if b.Strength != StrengthConfigured && b.Strength != StrengthProvider &&
		b.Strength != StrengthCluster {
		return fmt.Errorf("%w: identity strength %q", ErrInvalidRequest, b.Strength)
	}
	if err := checkText("runtime key", b.RuntimeKey, true, 128); err != nil {
		return err
	}
	return checkText("cluster epoch", b.ClusterEpoch, true, 128)
}

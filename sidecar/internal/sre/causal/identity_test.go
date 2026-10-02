package causal

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-07: two samples are compared only when they come from the same
// server incarnation. A restart, a failover (role or timeline change), a
// different server behind the same address (system identifier) or a
// major-version change between the samples makes the comparison invalid:
// stated as missing evidence, never scored as a cause.

func ident(start time.Time, sys string, tli int64, role string,
	version int64) probes.ServerIdentity {
	return probes.ServerIdentity{StartedAt: start, SystemID: sys, TimelineID: tli,
		Role: role, VersionNum: version}
}

var baseIdent = ident(serverStart, "7001", 1, probes.ServerRolePrimary, 170010)

func TestIdentityChange_EachFieldInvalidatesTheComparison(t *testing.T) {
	later := serverStart.Add(time.Hour)
	cases := map[string]struct {
		b    probes.ServerIdentity
		want string
	}{
		"same incarnation": {baseIdent, ""},
		"restart": {ident(later, "7001", 1, probes.ServerRolePrimary, 170010),
			ReasonServerRestarted},
		"minor upgrade restart": {ident(later, "7001", 1, probes.ServerRolePrimary, 170011),
			ReasonServerRestarted},
		"promotion in place": {ident(serverStart, "7001", 1, probes.ServerRoleStandby,
			170010), ReasonFailover},
		"timeline change": {ident(serverStart, "7001", 2, probes.ServerRolePrimary, 170010),
			ReasonFailover},
		"failover to a replica": {ident(later, "7001", 2, probes.ServerRolePrimary, 170010),
			ReasonFailover},
		"another server": {ident(later, "9999", 1, probes.ServerRolePrimary, 170010),
			ReasonServerReplaced},
		"major upgrade": {ident(later, "7001", 1, probes.ServerRolePrimary, 180001),
			ReasonMajorVersion},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := identityChange(baseIdent, c.b); got != c.want {
				t.Fatalf("identityChange = %q, want %q", got, c.want)
			}
			if got := identityChange(c.b, baseIdent); got != c.want {
				t.Fatalf("reversed identityChange = %q, want %q", got, c.want)
			}
		})
	}
}

// Unknown fields on both samples (evidence from before the identity
// columns) are not compared; a field known on one sample only cannot be
// matched and invalidates the comparison.
func TestIdentityChange_UnknownFields(t *testing.T) {
	if got := identityChange(probes.ServerIdentity{}, probes.ServerIdentity{}); got != "" {
		t.Fatalf("two unknown identities = %q, want comparable", got)
	}
	startOnly := probes.ServerIdentity{StartedAt: serverStart}
	if got := identityChange(startOnly, startOnly); got != "" {
		t.Fatalf("same start, other fields unknown = %q, want comparable", got)
	}
	for name, b := range map[string]probes.ServerIdentity{
		"timeline unknown":  ident(serverStart, "7001", 0, probes.ServerRolePrimary, 170010),
		"system id unknown": ident(serverStart, "", 1, probes.ServerRolePrimary, 170010),
		"role unknown":      ident(serverStart, "7001", 1, "", 170010),
		"start unknown":     ident(time.Time{}, "7001", 1, probes.ServerRolePrimary, 170010),
		"version unknown":   ident(serverStart, "7001", 1, probes.ServerRolePrimary, 0),
	} {
		if got := identityChange(baseIdent, b); got != ReasonIdentityUnknown {
			t.Errorf("%s: identityChange = %q, want %q", name, got, ReasonIdentityUnknown)
		}
	}
}

// A failover outranks the restart it implies, and a different server
// outranks both: the reason names the most specific change.
func TestIdentityChange_Precedence(t *testing.T) {
	everything := ident(serverStart.Add(time.Minute), "8888", 5, probes.ServerRoleStandby,
		180000)
	if got := identityChange(baseIdent, everything); got != ReasonServerReplaced {
		t.Fatalf("everything changed = %q, want %q", got, ReasonServerReplaced)
	}
	failoverAndUpgrade := ident(serverStart.Add(time.Minute), "7001", 5,
		probes.ServerRolePrimary, 180000)
	if got := identityChange(baseIdent, failoverAndUpgrade); got != ReasonFailover {
		t.Fatalf("failover and upgrade = %q, want %q", got, ReasonFailover)
	}
}

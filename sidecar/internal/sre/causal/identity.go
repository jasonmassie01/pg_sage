package causal

import (
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Comparison validity (CHECK-07): two samples are compared only when the
// same server incarnation produced both. A change between them is stated
// as missing evidence with one of these reasons, never scored.
const (
	// ReasonServerRestarted: the postmaster restarted between the samples.
	ReasonServerRestarted = "server_restarted"
	// ReasonFailover: the role or the timeline changed (a failover, a
	// promotion or a point-in-time recovery).
	ReasonFailover = "failover_between_samples"
	// ReasonServerReplaced: another cluster (system identifier) answered.
	ReasonServerReplaced = "server_replaced"
	// ReasonMajorVersion: the server's major version changed.
	ReasonMajorVersion = "major_version_changed"
	// ReasonIdentityUnknown: one sample says who the server was and the
	// other does not, so they cannot be matched.
	ReasonIdentityUnknown = "server_identity_unknown"
)

// identityChange is why samples from a and b cannot be compared, or "".
// The most specific known difference wins (another server, then a
// failover, a major version and a restart); a field known on one side
// only makes the comparison invalid; a field unknown on both sides
// (evidence from before the identity columns) is not compared.
func identityChange(a, b probes.ServerIdentity) string {
	half := false
	differs := func(x, y string) bool {
		if x == "" || y == "" {
			half = half || x != y
			return false
		}
		return x != y
	}
	switch {
	case differs(a.SystemID, b.SystemID):
		return ReasonServerReplaced
	case differs(a.Role, b.Role), differs(intText(a.TimelineID), intText(b.TimelineID)):
		return ReasonFailover
	case differs(intText(a.VersionNum/10000), intText(b.VersionNum/10000)):
		return ReasonMajorVersion
	case differs(timeText(a), timeText(b)):
		return ReasonServerRestarted
	case half:
		return ReasonIdentityUnknown
	}
	return ""
}

func intText(n int64) string {
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func timeText(s probes.ServerIdentity) string {
	if s.StartedAt.IsZero() {
		return ""
	}
	return s.StartedAt.UTC().Format(time.RFC3339Nano)
}

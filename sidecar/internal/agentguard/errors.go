package agentguard

import (
	"errors"
	"fmt"
)

// Errors, each distinguishable with errors.Is. Storage failures wrap the
// driver error and are none of these (map them to 503 / -32010).
var (
	// ErrInvalid is malformed input (422 / -32602 invalid_arguments).
	ErrInvalid = errors.New("agentguard: invalid request")
	// ErrNotFound is an unknown principal or role (404 / -32004).
	ErrNotFound = errors.New("agentguard: not found")
	// ErrDuplicateName is a principal name already taken (409).
	ErrDuplicateName = errors.New("agentguard: principal name already exists")
	// ErrSponsorNotFound is a sponsor_user_id with no sage.users row (422).
	ErrSponsorNotFound = errors.New("agentguard: sponsor user not found")
	// ErrRetired is an operation on a retired principal (409).
	ErrRetired = errors.New("agentguard: principal is retired")
	// ErrUnavailable is no control database (503 / -32010 unavailable).
	ErrUnavailable = errors.New("agentguard: control database unavailable")
	// ErrNoPrincipal is a request that carries no agent principal.
	ErrNoPrincipal = errors.New("agentguard: no agent principal on the request")
	// ErrApprovalRequired is a role change without an operator approval:
	// in G1 every guard_role_* runs at L2.
	ErrApprovalRequired = errors.New("agentguard: operator approval required")
	// ErrRoleManagementUnsupported is a server below PostgreSQL 16, where
	// CREATEROLE is near-superuser; Guard is posture-only there (§6.6).
	ErrRoleManagementUnsupported = errors.New(
		"agentguard: agent role management needs PostgreSQL 16 or later")
	// ErrEncryptionKeyRequired is a credential operation without
	// encryption_key; Guard is posture-only then (§6.4).
	ErrEncryptionKeyRequired = errors.New(
		"agentguard: encryption_key is required to store agent credentials")
	// ErrSelfCheck is pg_sage's own role failing the Guard self-check
	// (superuser, BYPASSRLS, inheriting an agent role, or no CREATEROLE).
	ErrSelfCheck = errors.New("agentguard: pg_sage's role fails the self-check")
	// ErrPostCheck is a role change whose post-check failed; the change
	// was rolled back.
	ErrPostCheck = errors.New("agentguard: post-check failed")
)

// Reason is a gate reason code for agent requests (§6.2.2, §8.1).
type Reason string

// Reason codes owned by the core. The gate composition maps them to
// verdict blocked (D-steps) or caps.
const (
	ReasonFrozen      Reason = "agent_frozen"
	ReasonUnsponsored Reason = "agent_unsponsored"
	ReasonLevel0      Reason = "agent_level0"
	ReasonRetired     Reason = "agent_retired"
	// ReasonGrantorLacksPrivilege is §6.6's grantor rule (G1-09).
	ReasonGrantorLacksPrivilege Reason = "grantor_lacks_privilege"
	// ReasonPublicCreate is preflight P1 (G1-16).
	ReasonPublicCreate Reason = "public_create_on_schema"
)

// DeniedError is an agent request refused with a reason code, a human
// detail and, where one exists, the exact fix.
type DeniedError struct {
	Reason Reason
	Detail string
	Fix    string
}

func (e *DeniedError) Error() string {
	msg := fmt.Sprintf("agentguard: denied: %s", e.Reason)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// IsDenied returns the DeniedError in err's chain, if any.
func IsDenied(err error) (*DeniedError, bool) {
	var d *DeniedError
	ok := errors.As(err, &d)
	return d, ok
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

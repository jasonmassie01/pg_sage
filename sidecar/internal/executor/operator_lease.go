package executor

import (
	"errors"
	"net/http"
	"strconv"
)

// operatorLease is the typed-target lease an operator action takes on the
// objects its SQL changes; nil when it names none (settings, signals).
// The holder is named after the approving user.
func operatorLease(sql string, approvedBy *int) *TargetLease {
	targets := operatorLeaseTargets(sql)
	if len(targets) == 0 {
		return nil
	}
	actor := "operator"
	if approvedBy != nil {
		actor = "operator:user:" + strconv.Itoa(*approvedBy)
	}
	return &TargetLease{Kind: "operator", Actor: actor, Targets: targets, Intent: sql,
		Operator: true}
}

// ManualExecuteStatus is the HTTP status for an operator action's error:
// 409 while another action holds its object or when an unused-index drop's
// evidence no longer holds, 404 for a finding that is not actionable, 400
// for SQL that does not match it, 500 otherwise.
func ManualExecuteStatus(err error) int {
	switch {
	case errors.Is(err, ErrTargetLeased), errors.Is(err, ErrUnusedEvidenceBroken):
		return http.StatusConflict
	case errors.Is(err, ErrFindingNotActionable):
		return http.StatusNotFound
	case errors.Is(err, ErrFindingSQLMismatch):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

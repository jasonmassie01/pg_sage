package executor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// TargetMismatchError refuses a self-initiated statement that changes
// another relation than the one its finding or custodian proposal names
// (executor-08): the gate blocks it as target_mismatch.
type TargetMismatchError struct {
	// Named is the object the finding or proposal names.
	Named string
	// Statement is the relation the statement changes, as it writes it.
	Statement string
}

func (e *TargetMismatchError) Error() string {
	return string(policy.ReasonTargetMismatch) + ": " + e.Detail()
}

// Detail is the gate decision's detail: both names.
func (e *TargetMismatchError) Detail() string {
	return fmt.Sprintf("the finding names %s but its statement changes %s",
		e.Named, e.Statement)
}

// FindingTargetBinding is how a finding's statement binds to the object
// the finding names.
type FindingTargetBinding struct {
	// Statement is the one relation the statement changes, as written; ""
	// when it names none (settings, signals) or the finding is not bound.
	Statement string
	// Named is the relation the finding names (an optimizer finding's
	// table); "" when it names none.
	Named string
	// Targets are the gate request's target objects: the finding's
	// identifier, or the statement's target when the finding names none.
	Targets []string
}

// relationObjectTypes are the finding object types whose identifier names
// a relation, and so are bound to their statement's target.
var relationObjectTypes = map[string]bool{"table": true, "index": true, "view": true,
	"materialized_view": true, "partitioned_table": true}

// BindFindingTarget binds a finding's statement to the relation the
// finding names, the default schema being public and unquoted names
// folding. It returns a *TargetMismatchError when they differ. Only a
// finding whose object type names a relation is bound: a setting, the
// instance, a query or an untyped finding keeps its identifier, and an
// index replacement has its own path.
func BindFindingTarget(f analyzer.Finding) (FindingTargetBinding, error) {
	binding := FindingTargetBinding{Targets: targetObjectsForFinding(f)}
	objectType := strings.ToLower(strings.TrimSpace(f.ObjectType))
	if IsIndexReplaceSQL(f.RecommendedSQL) || !relationObjectTypes[objectType] {
		return binding, nil
	}
	binding.Statement = singleStatementTarget(f.RecommendedSQL)
	binding.Named = findingNamedRelation(f)
	switch {
	case binding.Statement == "":
	case binding.Named == "":
		binding.Targets = []string{binding.Statement}
	case !sameRelation(binding.Named, binding.Statement):
		return binding, &TargetMismatchError{Named: binding.Named,
			Statement: binding.Statement}
	}
	return binding, nil
}

// bindFindingRequest applies the binding to a background finding's gate
// request: a mismatch is refused by the gate, an unnamed finding leases
// its statement's target.
func bindFindingRequest(request *policy.ActionRequest, f analyzer.Finding) {
	binding, err := BindFindingTarget(f)
	var mismatch *TargetMismatchError
	switch {
	case errors.As(err, &mismatch):
		request.TargetMismatch = mismatch.Detail()
	case binding.Named == "" && binding.Statement != "":
		request.TargetObjs = binding.Targets
	}
}

// bindCustodianRequest binds a custodian statement to the one target its
// proposal names, or leases the statement's target when it names none.
func bindCustodianRequest(request *policy.ActionRequest, proposal CustodianProposal) {
	statement := singleStatementTarget(proposal.SQL)
	switch {
	case statement == "":
	case len(proposal.TargetObjects) == 0:
		request.TargetObjs = []string{statement}
	case len(proposal.TargetObjects) != 1 ||
		!sameRelation(proposal.TargetObjects[0], statement):
		request.TargetMismatch = (&TargetMismatchError{
			Named:     strings.Join(proposal.TargetObjects, ", "),
			Statement: statement}).Detail()
	}
}

// singleStatementTarget is the one relation sql changes, or "".
func singleStatementTarget(sql string) string {
	if strings.TrimSpace(sql) == "" {
		return ""
	}
	if targets := operatorLeaseTargets(sql); len(targets) == 1 {
		return targets[0]
	}
	return ""
}

// findingNamedRelation is the relation a finding names: an optimizer
// finding's table, or the identifier without a column list
// ("public.orders(customer_id)" from the FK-index rule names the table).
func findingNamedRelation(f analyzer.Finding) string {
	if f.Category == optimizer.OptimizerCategory {
		return strings.TrimSpace(analyzer.OptimizerFindingTable(f))
	}
	return strings.TrimSpace(withoutColumnList(f.ObjectIdentifier))
}

// withoutColumnList cuts a trailing parenthesized column list outside
// quoted identifiers.
func withoutColumnList(name string) string {
	inQuote := false
	for i := 0; i < len(name); i++ {
		switch {
		case name[i] == '"':
			inQuote = !inQuote
		case name[i] == '(' && !inQuote && strings.HasSuffix(name, ")"):
			return name[:i]
		}
	}
	return name
}

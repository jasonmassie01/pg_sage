package probes

import "errors"

// Query-scoped probes (specialist contract revision 1.1.0). A spec marked
// QueryScoped takes an optional statement identity, Args.QueryID (a
// pg_stat_statements queryid), bound as the parameter after its typed
// arguments; 0 reads every statement. The SQL compares it as a parameter
// (never text), so a caller's statement scope cannot change the query.

// check validates args against spec: its argument kind, and a queryid only
// where the spec is query-scoped.
func (a Args) check(s Spec) error {
	if a.QueryID != 0 && !s.QueryScoped {
		return errors.New("probe takes no queryid")
	}
	a.QueryID = 0
	return a.validate(s.Args)
}

// bind builds the positional parameters of spec's SQL: the row limit, the
// typed arguments, then the queryid of a query-scoped probe.
func (a Args) bind(s Spec, limit int) []any {
	p := a.params(s.Args, limit)
	if s.QueryScoped {
		p = append(p, a.QueryID)
	}
	return p
}

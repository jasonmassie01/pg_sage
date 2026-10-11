package sqlast

// BrokeredRead is one agent statement prepared for the brokered read path
// (agent_query, spec §6.8 S2): exactly one plain SELECT, in the canonical
// form that runs, with everything the catalog proof and the output
// controls need from the parse tree.
type BrokeredRead struct {
	// Canonical is Deparse(Parse(sql)): comments, case and Unicode escapes
	// resolved. It is what executes, so what was checked is what runs.
	Canonical string
	// Fingerprint is pg_query's fingerprint of the statement.
	Fingerprint string
	// Params is the highest $n parameter referenced (0 = none).
	Params int
	// Query is what executing the statement could invoke.
	Query ReadQuery
	// OutputColumns are the column names referenced as bare entries of the
	// top-level target list (SELECT ssn, t.ssn): their values leave only
	// as output, where masking applies.
	OutputColumns []string
	// OtherColumnRefs are the names referenced anywhere else: filters,
	// casts, function arguments, ORDER BY, subqueries, set operations and
	// whole-row references. A masked column here could leak through a
	// predicate or an error message.
	OtherColumnRefs []string
}

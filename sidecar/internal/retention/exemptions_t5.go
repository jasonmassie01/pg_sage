package retention

// trancheFiveExemptions are the T5 sage tables with time columns that the
// retention sweep does not age out: each is small current state, never a
// growing time series of pg_sage's.
var trancheFiveExemptions = map[string]string{
	"digest_delivery": "current state: one row per digest scope (a database or the " +
		"fleet roll-up) with the hash last delivered (ux-02b); purging it would resend " +
		"an unchanged digest",
	"workload_pseudonym_key": "one row (id = 1): the sealed key every replica hashes " +
		"workload pack identifiers with (evid-02c); purging it would change every " +
		"pseudonym",
}

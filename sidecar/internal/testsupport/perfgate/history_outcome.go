package perfgate

// sage.action_outcome holds one verification outcome per executed
// action and grows with them. It is seeded as history (one outcome per
// seeded action, decided 31 to 90 days ago, every verdict), so gate A
// sees any read that scans it, while the trust reconciler (30-day
// lookback) has nothing to ingest from it.
func init() {
	historySteps = append(historySteps, seedStep{"action_outcome", true,
		`INSERT INTO sage.action_outcome (action_log_id, action_class, predicted,
		prediction_method, verdict, tolerance, created_at, decided_at)
		SELECT l.id, (ARRAY['index_create','vacuum','analyze','reindex'])[1 + l.n % 4],
		'{"metric":"mean_exec_time"}', (ARRAY['hypopg','rule','model','none'])[1 + l.n % 4],
		(ARRAY['improved','neutral','regressed','insufficient_evidence','unverifiable'])
		[1 + l.n % 5], 'met', l.at - interval '1 hour', l.at
		FROM (SELECT id, row_number() OVER (ORDER BY id) AS n,
		      now() - interval '31 days' - (row_number() OVER (ORDER BY id)::double precision
		        / $1::int) * interval '59 days' AS at
		      FROM sage.action_log ORDER BY id LIMIT $1) l`})
}

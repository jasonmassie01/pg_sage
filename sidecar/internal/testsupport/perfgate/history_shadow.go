package perfgate

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"
)

// Shadow mode history (roadmap 1.4): a tenth of HistoryRows shadow
// decisions over 30 days (one per fingerprint per day is the recording
// bound, far below the growing tables), every fiftieth still pending,
// the rest scored by every source; and the ledger's copy of the counted
// scores under the runtime's deployment, which the Trust view reads per
// class.
func init() {
	sreSteps = append(sreSteps, shadowSteps...)
}

var shadowSteps = []sreStep{
	{"shadow_decision", false, `INSERT INTO sage.shadow_decision (database_name,
		fingerprint, family, action_class, title, object_identifier, sql, shape, prediction,
		gate_verdict, gate_reason, trusted_verdict, trusted_reason, granted_level, status,
		score, score_source, counted, scored_at, recorded_at, last_seen_at)
		SELECT current_database(), md5('perfgate-shadow-' || g),
		CASE WHEN g % 4 = 0 THEN 'tuning' ELSE 'hygiene' END,
		(ARRAY['index_create','vacuum','analyze','index_drop'])[1 + g % 4],
		'perfgate shadow', 'perf_app_000.t_' || g, 'VACUUM perf_app_000.t_' || g,
		'vacuum perf_app_000.t_' || g, '{"metric":"dead_tuples"}', 'observe_only',
		'autonomy_level', 'execute', 'autonomy_l3', 1,
		CASE WHEN p THEN 'pending' ELSE 'scored' END,
		CASE WHEN NOT p THEN sc END, CASE WHEN NOT p THEN src END,
		NOT p AND sc <> 'unscored' AND src IN ('hypopg', 'external'),
		CASE WHEN NOT p THEN ts + interval '1 hour' END, ts, ts
		FROM generate_series(1, greatest($1::int / 10, 1)) g,
		LATERAL (SELECT g % 50 = 0 AS p,
		  (ARRAY['correct','incorrect','neutral','unscored'])[1 + g % 4] AS sc,
		  CASE WHEN g % 4 = 3 THEN 'none'
		       ELSE (ARRAY['hypopg','external','operator'])[1 + g % 3] END AS src,
		  now() - (g::double precision / greatest($1::int / 10, 1)) * interval '30 days'
		    AS ts) x`, func(n int, _ Binding) []any { return []any{n} }},
	{"trust_shadow_evidence", false, `INSERT INTO sage.trust_shadow_evidence
		(deployment_id, database_name, shadow_id, fingerprint, family, action_class, score,
		 source, observed_at)
		SELECT $2::uuid, current_database(), id, fingerprint, family, action_class, score,
		score_source, scored_at FROM sage.shadow_decision
		WHERE counted AND title = 'perfgate shadow' LIMIT $1`, argsND},
	{"shadow_decision", false, `INSERT INTO sage.shadow_decision (database_name,
		fingerprint, family, action_class, title, object_identifier, sql, shape, prediction,
		gate_verdict, gate_reason, trusted_verdict, trusted_reason, granted_level, status,
		recorded_at, last_seen_at)
		SELECT current_database(), md5($1 || g), 'hygiene', 'vacuum', 'perfgate shadow',
		'perf_app_000.t_' || g, 'VACUUM perf_app_000.t_' || g, 'vacuum perf_app_000.t_' || g,
		'{"metric":"dead_tuples"}', 'observe_only', 'autonomy_level', 'execute',
		'autonomy_l3', 1, 'pending', now() - interval '1 hour', now() - interval '1 hour'
		FROM generate_series(1, $2::int) g`,
		func(int, Binding) []any { return []any{shadowSeenPrefix, shadowSeenCount} }},
}

// The young pending decisions the harness re-sees during the run (as a
// cycle does while pg_sage still wants the same change), so gate F
// measures the recurring seen bump on sage.shadow_decision. The scorer
// leaves them pending: no operator decision or applied change matches a
// vacuum, and they are far from the horizon.
const (
	shadowSeenPrefix = "perfgate-shadow-seen-"
	shadowSeenCount  = 4
)

// ShadowSeenFingerprints are the fingerprints of those decisions.
func ShadowSeenFingerprints() []string {
	out := make([]string, 0, shadowSeenCount)
	for g := 1; g <= shadowSeenCount; g++ {
		sum := md5.Sum([]byte(shadowSeenPrefix + strconv.Itoa(g)))
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

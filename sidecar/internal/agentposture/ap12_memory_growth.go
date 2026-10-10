package agentposture

import (
	"context"
	"fmt"
	"sort"
	"time"
)

func init() { Register(ap12{}) }

// ap12 reports agent memory stores that only grow: schemas whose
// LangGraph checkpoint tables grew faster than
// agents.posture.memory_growth_gb_day with no rows deleted. Growth needs
// two timed observations, kept between runs in the Monitor's observation
// store: the first run records, a later one (at least minObservationGap
// after) compares. The first look has no store, so it reports nothing.
type ap12 struct{}

func (ap12) Spec() Spec {
	return Spec{ID: "AP-12", Title: "Agent memory stores growing without deletes",
		Severity: Info}
}

// checkpointTables are the tables LangGraph's Postgres checkpointer creates.
var checkpointTables = []string{"checkpoints", "checkpoint_blobs", "checkpoint_writes"}

// minObservationGap is the shortest time two observations are compared
// over; closer runs keep the older observation as the baseline.
const minObservationGap = time.Hour

// bytesPerGB is the GB of agents.posture.memory_growth_gb_day (GiB).
const bytesPerGB = float64(1 << 30)

var ap12SQL = Statement("AP-12", `SELECT n.nspname::text, c.relname::text,
  pg_catalog.pg_total_relation_size(c.oid),
  pg_catalog.pg_stat_get_tuples_deleted(c.oid), pg_catalog.now()
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = ANY($1::text[]) AND c.relkind = 'r' AND `+apbUserSchemas+`
ORDER BY 1, 2
LIMIT $2`)

func (ap12) Detect(ctx context.Context, in Input) ([]Finding, error) {
	store := in.Env.Observations
	if store == nil {
		return nil, nil
	}
	cur, err := observeCheckpoints(ctx, in.Q)
	if err != nil {
		return nil, err
	}
	prev, have := store.Previous("AP-12")
	out, record := memoryGrowth(prev, have, cur, in.Env.Config.MemoryGrowthGBDay)
	if record {
		store.Record("AP-12", cur)
	}
	return out, nil
}

// observeCheckpoints sums the checkpoint tables' size and deletes per schema.
func observeCheckpoints(ctx context.Context, q Querier) (Observation, error) {
	rows, err := q.Query(ctx, ap12SQL, checkpointTables, apbMaxRows)
	if err != nil {
		return Observation{}, fmt.Errorf("read checkpoint table sizes: %w", err)
	}
	defer rows.Close()
	cur := Observation{Values: map[string]ObservedValue{}}
	for rows.Next() {
		var schema, table string
		var bytes, deletes int64
		if err := rows.Scan(&schema, &table, &bytes, &deletes, &cur.At); err != nil {
			return Observation{}, fmt.Errorf("read checkpoint table sizes: %w", err)
		}
		v := cur.Values[schema]
		v.Bytes += bytes
		v.Deletes += deletes
		v.Tables = append(v.Tables, QualifiedName(schema, table))
		cur.Values[schema] = v
	}
	if err := rows.Err(); err != nil {
		return Observation{}, fmt.Errorf("read checkpoint table sizes: %w", err)
	}
	if cur.At.IsZero() {
		cur.At = time.Now()
	}
	return cur, nil
}

// memoryGrowth compares cur with the previous observation and says
// whether cur becomes the new baseline. Without a previous one, or when
// the clock went backwards, cur is recorded; closer than
// minObservationGap the older one stays. A schema whose deletes went down
// (a statistics reset) or that is new is not judged.
func memoryGrowth(prev Observation, have bool, cur Observation,
	limitGBDay float64) ([]Finding, bool) {
	elapsed := cur.At.Sub(prev.At)
	if !have || elapsed < 0 {
		return nil, true
	}
	if elapsed < minObservationGap {
		return nil, false
	}
	schemas := make([]string, 0, len(cur.Values))
	for s := range cur.Values {
		schemas = append(schemas, s)
	}
	sort.Strings(schemas)
	var out []Finding
	days := elapsed.Hours() / 24
	for _, s := range schemas {
		before, ok := prev.Values[s]
		now := cur.Values[s]
		if !ok || now.Deletes != before.Deletes {
			continue
		}
		rate := float64(now.Bytes-before.Bytes) / bytesPerGB / days
		if rate > limitGBDay {
			out = append(out, growthFinding(s, before, now, prev.At, cur.At, rate, limitGBDay))
		}
	}
	return out, true
}

func growthFinding(schema string, before, now ObservedValue, from, to time.Time,
	rate, limit float64) Finding {
	return Finding{Severity: Info, ObjectType: "schema", Object: schema,
		Title: "Agent memory store growing without deletes",
		Detail: fmt.Sprintf("LangGraph checkpoint tables in %s (%s) grew %.2f GB a day "+
			"(limit %.2f) between %s and %s, with no rows deleted.", schema,
			listSome(now.Tables, 5), rate, limit, from.UTC().Format(time.RFC3339),
			to.UTC().Format(time.RFC3339)),
		Recommendation: "Give the agent's checkpoints a retention: delete old threads " +
			"regularly (LangGraph keeps every checkpoint by default).",
		FixScript: "-- Find the largest threads, then delete the ones past retention:\n" +
			"SELECT thread_id, count(*) FROM " + QualifiedName(schema, "checkpoints") +
			" GROUP BY 1 ORDER BY 2 DESC LIMIT 20;",
		Caveat: "Growth is measured between two posture runs pg_sage made in this " +
			"process; after a restart it needs two new observations.",
		Evidence: []Evidence{
			{Source: "pg_total_relation_size", Ref: from.UTC().Format(time.RFC3339),
				Detail: fmt.Sprintf("%d bytes, %d rows deleted", before.Bytes, before.Deletes)},
			{Source: "pg_total_relation_size", Ref: to.UTC().Format(time.RFC3339),
				Detail: fmt.Sprintf("%d bytes, %d rows deleted", now.Bytes, now.Deletes)}}}
}

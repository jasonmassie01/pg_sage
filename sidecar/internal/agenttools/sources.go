package agenttools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Sampling bounds of QuerySources.
const (
	maxSampleSeconds = 5
	sampleInterval   = 250 * time.Millisecond
)

// SourcesRequest samples the running statements; SampleSeconds 0 is one
// snapshot, up to 5 seconds of snapshots every 250 ms.
type SourcesRequest struct {
	QueryID       QueryID `json:"query_id,omitempty"`
	SampleSeconds int     `json:"sample_seconds,omitempty"`
}

// SourcesResult attributes running statements to applications.
type SourcesResult struct {
	Queries []QuerySource `json:"queries"`
	Method  string        `json:"method"`
	Note    string        `json:"note"`
}

// QuerySource is one statement (and tag set) seen running: the
// application names and how many samples saw it.
type QuerySource struct {
	QueryID          QueryID           `json:"query_id"`
	Tags             map[string]string `json:"tags,omitempty"`
	ApplicationNames map[string]int    `json:"application_names"`
	Samples          int               `json:"samples"`
}

const sourcesNote = "Attribution uses only what PostgreSQL shows without extensions: " +
	"pg_stat_activity sampling (application_name and the running query text) and " +
	"sqlcommenter comments in that text; top_queries reads the same sqlcommenter " +
	"comments kept in pg_stat_statements text. Statements without sqlcommenter tags " +
	"are attributed by application_name only."

const activitySQL = `/* pg_sage */ SELECT COALESCE(query_id, 0), COALESCE(application_name, ''),
    COALESCE(query, '')
FROM pg_stat_activity
WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid()
  AND backend_type = 'client backend'`

// activity is one active session in one snapshot.
type activity struct {
	queryID int64
	app     string
	query   string
}

// QuerySources samples the active sessions of this database and groups
// them by query id and sqlcommenter tags.
func (t *Tools) QuerySources(ctx context.Context, req SourcesRequest) (SourcesResult, error) {
	if err := t.ready(); err != nil {
		return SourcesResult{}, err
	}
	if req.SampleSeconds < 0 || req.SampleSeconds > maxSampleSeconds {
		return SourcesResult{}, invalid("sample_seconds %d is outside 0..%d",
			req.SampleSeconds, maxSampleSeconds)
	}
	snapshots := 1 + int(time.Duration(req.SampleSeconds)*time.Second/sampleInterval)
	groups := map[string]*QuerySource{}
	for i := 0; i < snapshots; i++ {
		if i > 0 {
			if err := sleepCtx(ctx, sampleInterval); err != nil {
				return SourcesResult{}, fmt.Errorf("sample pg_stat_activity: %w", err)
			}
		}
		seen, err := t.snapshot(ctx)
		if err != nil {
			return SourcesResult{}, err
		}
		t.addSamples(groups, seen, req.QueryID)
	}
	return SourcesResult{Queries: sortedSources(groups),
		Method: fmt.Sprintf("pg_stat_activity sampled %d time(s) over %d s (active "+
			"sessions of this database)", snapshots, req.SampleSeconds),
		Note: sourcesNote}, nil
}

func (t *Tools) snapshot(ctx context.Context) ([]activity, error) {
	rows, err := t.pool.Query(ctx, activitySQL)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	defer rows.Close()
	var out []activity
	for rows.Next() {
		var a activity
		if err := rows.Scan(&a.queryID, &a.app, &a.query); err != nil {
			return nil, fmt.Errorf("scan pg_stat_activity: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	return out, nil
}

func (t *Tools) addSamples(groups map[string]*QuerySource, seen []activity, want QueryID) {
	for _, a := range seen {
		if t.opts.Excluded(a.query) || (want != 0 && QueryID(a.queryID) != want) {
			continue
		}
		tags := ParseSQLCommenter(a.query)
		key := groupKey(a.queryID, tags)
		g := groups[key]
		if g == nil {
			g = &QuerySource{QueryID: QueryID(a.queryID), ApplicationNames: map[string]int{}}
			if len(tags) > 0 {
				g.Tags = tags
			}
			groups[key] = g
		}
		g.ApplicationNames[a.app]++
		g.Samples++
	}
}

// groupKey identifies a query id with one tag set.
func groupKey(id int64, tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d", id)
	for _, k := range keys {
		fmt.Fprintf(&b, "\x1f%s=%s", k, tags[k])
	}
	return b.String()
}

func sortedSources(groups map[string]*QuerySource) []QuerySource {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]QuerySource, 0, len(groups))
	for _, k := range keys {
		out = append(out, *groups[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].QueryID < out[j].QueryID
	})
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

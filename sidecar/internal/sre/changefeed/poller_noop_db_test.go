package changefeed

import (
	"testing"
)

// stateVersions returns each source's row version and lock (xmin:xmax) for
// the fixture's scope: unchanged, the row was neither rewritten nor even
// locked by an upsert whose guard found nothing to change.
func (f pollFixture) stateVersions(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT source, xmin::text || ':' || xmax::text
		FROM sage.sre_change_feed_state
		WHERE deployment_id = $1 AND database_id = $2`, string(f.scope.DeploymentID),
		string(f.scope.DatabaseID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var src, xmin string
		if err := rows.Scan(&src, &xmin); err != nil {
			t.Fatal(err)
		}
		out[src] = xmin
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Perf storage phase (dogfood lifeos): every poll rewrote all 8 cursor rows
// with the same state (3,328 updates on 8 rows, 83% dead). A poll that
// changes nothing writes nothing.
func TestPoller_UnchangedStateIsNotRewritten(t *testing.T) {
	f := newPollFixture(t)
	f.poll(t)
	before := f.stateVersions(t)
	if len(before) < 5 {
		t.Fatalf("%d sources kept state", len(before))
	}
	for i := 0; i < 3; i++ {
		f.poll(t)
	}
	after := f.stateVersions(t)
	for src, xmin := range before {
		if after[src] != xmin {
			t.Errorf("source %s rewritten by an unchanged poll (xmin %s -> %s)", src, xmin,
				after[src])
		}
	}
}

// A changed state is still written, and only that source's row.
func TestPoller_ChangedStateIsWritten(t *testing.T) {
	f := newPollFixture(t)
	f.poll(t)
	f.setState(t, "postmaster", `{"started_at": "2001-01-01T00:00:00Z"}`)
	before := f.stateVersions(t)
	f.poll(t)
	after := f.stateVersions(t)
	if after["postmaster"] == before["postmaster"] {
		t.Fatal("a source whose state changed was not written")
	}
	changed := 0
	for src, xmin := range before {
		if after[src] != xmin {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("%d sources rewritten, want only postmaster", changed)
	}
}

// Two documents that differ only in key order or spacing are the same
// state: jsonb normalizes both.
func TestSameState(t *testing.T) {
	cases := []struct {
		prev, next string
		want       bool
	}{
		{`{"a": 1, "b": [1, 2]}`, `{"b":[1,2],"a":1}`, true},
		{`{"a": 1}`, `{"a": 2}`, false},
		{``, `{"a": 1}`, false},
		{`{"a": 1}`, `not json`, false},
		{`{"a": 1.0}`, `{"a": 1}`, true},
		{`{"a": null}`, `{}`, false},
	}
	for _, c := range cases {
		if got := sameState([]byte(c.prev), []byte(c.next)); got != c.want {
			t.Errorf("sameState(%q, %q) = %v, want %v", c.prev, c.next, got, c.want)
		}
	}
}

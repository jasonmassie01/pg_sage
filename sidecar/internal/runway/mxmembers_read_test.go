package runway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// readMXMembers against a scripted querier: what a role may not execute
// is unknown with the grant named, anything else that fails is an error,
// and an empty range reads no members.
// No concurrent access tests here: readMXMembers holds no state.

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("fakeRow: column count")
	}
	for i, v := range r.vals {
		switch d := dest[i].(type) {
		case *bool:
			*d = v.(bool)
		case *string:
			*d = v.(string)
		case *int64:
			*d = v.(int64)
		case *time.Time:
			*d = v.(time.Time)
		default:
			return errors.New("fakeRow: unsupported destination")
		}
	}
	return nil
}

// fakeCounts is the member query's result: one int8 per row.
type fakeCounts struct {
	vals []int64
	i    int
}

func (r *fakeCounts) Close()                                       {}
func (r *fakeCounts) Err() error                                   { return nil }
func (r *fakeCounts) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeCounts) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeCounts) RawValues() [][]byte                          { return nil }
func (r *fakeCounts) Conn() *pgx.Conn                              { return nil }
func (r *fakeCounts) Next() bool {
	r.i++
	return r.i <= len(r.vals)
}
func (r *fakeCounts) Values() ([]any, error) { return []any{r.vals[r.i-1]}, nil }
func (r *fakeCounts) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.vals[r.i-1]
	return nil
}

type fakeMXQuerier struct {
	grants   fakeRow
	control  fakeRow
	counts   []int64
	countErr error
	queried  []string
	ids      []int64
}

func (q *fakeMXQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "has_function_privilege") {
		q.queried = append(q.queried, "grants")
		return q.grants
	}
	q.queried = append(q.queried, "control")
	return q.control
}

func (q *fakeMXQuerier) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	q.queried = append(q.queried, "members")
	q.ids = args[0].([]int64)
	if q.countErr != nil {
		return nil, q.countErr
	}
	return &fakeCounts{vals: q.counts}, nil
}

var mxAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func permittedQuerier(next, offset, oldest int64, counts []int64) *fakeMXQuerier {
	return &fakeMXQuerier{grants: fakeRow{vals: []any{true, true, "sage"}},
		control: fakeRow{vals: []any{next, offset, mxAt, oldest}}, counts: counts}
}

func denied() error {
	return &pgconn.PgError{Code: "42501", Message: "permission denied for function"}
}

func TestMXMembers_ReadMeasures(t *testing.T) {
	q := permittedQuerier(11, 25, 1, []int64{2, 2, 3, 3, 2, 2, 3, 3, 2, 3})
	e, err := readMXMembers(context.Background(), q)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if e.Status != MXMeasured || e.Live != 10 || e.Sampled != 10 || e.Members != 25 ||
		e.StdErr != 0 || e.MeanMembers != 2.5 || e.NextOffset != 25 || e.NextMXID != 11 ||
		e.OldestMXID != 1 || !e.CheckpointAt.Equal(mxAt) {
		t.Fatalf("estimate = %+v", e)
	}
	if len(q.ids) != 10 || q.ids[0] != 1 || q.ids[9] != 10 {
		t.Fatalf("sampled ids %v, want 1..10", q.ids)
	}
}

func TestMXMembers_ReadWithoutMultixactsReadsNoMembers(t *testing.T) {
	q := permittedQuerier(1, 0, 1, nil)
	e, err := readMXMembers(context.Background(), q)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if e.Status != MXMeasured || e.Live != 0 || e.Sampled != 0 || e.Members != 0 {
		t.Fatalf("estimate = %+v, want measured zeros", e)
	}
	for _, s := range q.queried {
		if s == "members" {
			t.Fatalf("queried %v: an empty range sampled members", q.queried)
		}
	}
}

func TestMXMembers_ReadNotPermittedIsUnknownWithTheGrant(t *testing.T) {
	membersGrant := "GRANT EXECUTE ON FUNCTION pg_catalog.pg_get_multixact_members(xid) " +
		"TO sage"
	controlGrant := "GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_checkpoint() TO sage"
	cases := []struct {
		name  string
		q     *fakeMXQuerier
		wants []string
	}{
		{"members not granted", &fakeMXQuerier{grants: fakeRow{vals: []any{true, false,
			"sage"}}}, []string{membersGrant}},
		{"neither granted", &fakeMXQuerier{grants: fakeRow{vals: []any{false, false,
			"sage"}}}, []string{controlGrant, membersGrant}},
		{"checkpoint denied", &fakeMXQuerier{grants: fakeRow{vals: []any{true, true,
			"sage"}}, control: fakeRow{err: denied()}}, []string{controlGrant}},
		{"members denied", func() *fakeMXQuerier {
			q := permittedQuerier(11, 25, 1, nil)
			q.countErr = denied()
			return q
		}(), []string{membersGrant}},
	}
	for _, c := range cases {
		e, err := readMXMembers(context.Background(), c.q)
		if err != nil {
			t.Errorf("%s: err %v, want unknown", c.name, err)
			continue
		}
		if e.Status != MXUnknown || e.Members != 0 || e.Reason == "" {
			t.Errorf("%s: estimate %+v, want unknown", c.name, e)
		}
		for _, w := range c.wants {
			if !strings.Contains(e.Grant, w) {
				t.Errorf("%s: grant %q does not name %q", c.name, e.Grant, w)
			}
		}
		if len(c.wants) == 1 && strings.Count(e.Grant, "GRANT") != 1 {
			t.Errorf("%s: grant %q names more than the missing function", c.name, e.Grant)
		}
	}
}

func TestMXMembers_ReadErrorsPropagate(t *testing.T) {
	refused := errors.New("dial tcp: connection refused")
	cases := []struct {
		name string
		q    *fakeMXQuerier
		want string
	}{
		{"grants", &fakeMXQuerier{grants: fakeRow{err: refused}},
			"read the multixact function grants"},
		{"control", &fakeMXQuerier{grants: fakeRow{vals: []any{true, true, "sage"}},
			control: fakeRow{err: refused}}, "read the checkpoint's multixact state"},
		{"members", func() *fakeMXQuerier {
			q := permittedQuerier(11, 25, 1, nil)
			q.countErr = refused
			return q
		}(), "sample 10 multixacts' members"},
		{"short result", permittedQuerier(11, 25, 1, []int64{2, 2}), "2 counts returned"},
	}
	for _, c := range cases {
		e, err := readMXMembers(context.Background(), c.q)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to say %q", c.name, err, c.want)
			continue
		}
		if c.name != "short result" && !errors.Is(err, refused) {
			t.Errorf("%s: err %v does not wrap the cause", c.name, err)
		}
		if e.Status != "" {
			t.Errorf("%s: a failed reading has status %q", c.name, e.Status)
		}
	}
}

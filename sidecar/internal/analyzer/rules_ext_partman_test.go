package analyzer

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: the rule functions are pure over one cycle's
// reading, which only the analyzer's cycle goroutine builds and reads.

func partmanExtras(r *partmanReading) *RuleExtras {
	return &RuleExtras{ext: &extReading{partman: r}}
}

func TestExtPartmanRules_NilReadingEmitsNothing(t *testing.T) {
	for _, ex := range []*RuleExtras{nil, {}, {ext: &extReading{}}} {
		if got := rulePartmanPremakeGap(nil, nil, nil, ex); got != nil {
			t.Errorf("premake on %+v = %v, want nil", ex, got)
		}
		if got := rulePartmanDefaultRows(nil, nil, nil, ex); got != nil {
			t.Errorf("default rows on %+v = %v, want nil", ex, got)
		}
	}
}

func TestExtPartmanPremake_Boundary(t *testing.T) {
	cases := []struct {
		premake, future, want int
	}{{4, 4, 0}, {4, 5, 0}, {4, 3, 1}, {4, 0, 1}, {1, 0, 1}, {0, 0, 0}}
	for _, c := range cases {
		r := &partmanReading{Schema: "partman", premakeRead: true, Parents: []partmanParent{{
			Parent: "public.events", ConfigName: "public.events", Control: "created_at",
			Kind: "time", Premake: c.premake, Future: c.future, FutureRead: true,
		}}}
		got := rulePartmanPremakeGap(nil, nil, nil, partmanExtras(r))
		if len(got) != c.want {
			t.Fatalf("premake %d future %d: findings = %d, want %d",
				c.premake, c.future, len(got), c.want)
		}
		if c.want == 0 {
			continue
		}
		f := got[0]
		if f.Category != categoryPartmanPremake || f.Severity != "warning" ||
			f.ObjectIdentifier != "public.events" || f.RecommendedSQL != "" ||
			f.Detail["premake"] != c.premake || f.Detail["future_partitions"] != c.future ||
			f.Detail["control_kind"] != "time" ||
			!strings.Contains(f.Recommendation, "partman.run_maintenance_proc()") {
			t.Fatalf("premake finding = %+v", f)
		}
	}
}

func TestExtPartmanPremake_UnreadParentEmitsNothing(t *testing.T) {
	r := &partmanReading{Schema: "partman", premakeRead: true, Parents: []partmanParent{{
		Parent: "public.events", Premake: 4, Future: 0, FutureRead: false,
	}}}
	if got := rulePartmanPremakeGap(nil, nil, nil, partmanExtras(r)); len(got) != 0 {
		t.Fatalf("a parent whose children were not read emitted %v", got)
	}
	r.premakeRead = false
	r.Parents[0].FutureRead = true
	if got := rulePartmanPremakeGap(nil, nil, nil, partmanExtras(r)); len(got) != 0 {
		t.Fatalf("an unread category emitted %v", got)
	}
}

func TestExtPartmanDefaultRows(t *testing.T) {
	r := &partmanReading{Schema: `My "pm"`, defaultRead: true, Parents: []partmanParent{
		{Parent: "public.a", ConfigName: "public.a", Default: "public.a_default",
			DefaultHasRows: true, DefaultRows: 12, DefaultRead: true},
		{Parent: "public.b", ConfigName: "public.b", Default: "public.b_default",
			DefaultRead: true},
		{Parent: "public.c", ConfigName: "public.c"},
		{Parent: "public.d", ConfigName: "public.d", Default: "public.d_default",
			DefaultHasRows: true},
	}}
	got := rulePartmanDefaultRows(nil, nil, nil, partmanExtras(r))
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want only public.a's default", got)
	}
	f := got[0]
	if f.Category != categoryPartmanDefault || f.Severity != "warning" ||
		f.ObjectIdentifier != "public.a_default" || f.Detail["parent_table"] != "public.a" ||
		f.Detail["estimated_rows"] != int64(12) || f.RecommendedSQL != "" ||
		!strings.Contains(f.Recommendation, `"My ""pm""".partition_data_proc('public.a')`) {
		t.Fatalf("default finding = %+v", f)
	}
	r.defaultRead = false
	if got := rulePartmanDefaultRows(nil, nil, nil, partmanExtras(r)); len(got) != 0 {
		t.Fatalf("an unread category emitted %v", got)
	}
}

func TestExtPartmanFutureCount(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	ts := func(h int) *time.Time { v := now.Add(time.Duration(h) * time.Hour); return &v }
	id := func(v int64) *int64 { return &v }
	cases := []struct {
		name     string
		children []partmanChild
		maxID    *int64
		want     int
		kind     string
	}{
		{"time: strictly after now", []partmanChild{{Start: ts(48)}, {Start: ts(24)},
			{Start: ts(0)}, {Start: ts(-24)}}, nil, 2, "time"},
		{"id: start above the max", []partmanChild{{StartID: id(400)}, {StartID: id(300)},
			{StartID: id(200)}, {StartID: id(100)}}, id(200), 2, "id"},
		{"id: an empty parent has only future children", []partmanChild{{StartID: id(100)},
			{StartID: id(0)}}, nil, 2, "id"},
		{"no children", nil, nil, 0, ""},
	}
	for _, c := range cases {
		got, kind := countFuturePartitions(c.children, c.maxID, now)
		if got != c.want || kind != c.kind {
			t.Errorf("%s: future = %d (%s), want %d (%s)", c.name, got, kind, c.want, c.kind)
		}
	}
}

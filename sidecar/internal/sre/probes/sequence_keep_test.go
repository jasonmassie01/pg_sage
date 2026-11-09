package probes

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// forecast-05/forecast-10: sequence_runway v4 covers descending
// sequences against the lower of their limits (measured in the direction
// of travel) and takes a keep-list: the sequences already sampled, which
// stay listed even when they fall out of the top by fraction.

// No concurrent access tests: Args and SequenceRunway are values with no
// shared state.

func TestArgs_KeepIsOnlyForTheSlicedSequenceRead(t *testing.T) {
	reg := Catalog()
	keep := make([]string, MaxSequenceKeep)
	for i := range keep {
		keep[i] = fmt.Sprintf("public.s_%d", i)
	}
	if err := reg.CheckArgs(SequenceRunwayProbe, Args{Keep: keep}); err != nil {
		t.Fatalf("a keep-list of %d rejected: %v", MaxSequenceKeep, err)
	}
	if err := reg.CheckArgs(SequenceRunwayProbe,
		Args{Keep: []string{"public.a"}, Slices: 4, Slice: 3}); err != nil {
		t.Fatalf("a keep-list with a slice rejected: %v", err)
	}
	bad := []struct {
		id   ID
		args Args
	}{
		{SequenceRunwayProbe, Args{Keep: append(keep, "public.one_too_many")}},
		{SequenceRunwayProbe, Args{Keep: []string{""}}},
		{SequenceRunwayProbe, Args{Keep: []string{strings.Repeat("x", MaxKeepNameBytes+1)}}},
		{XIDRunwayProbe, Args{Keep: []string{"public.a"}}},
		{RunwayTrendsProbe, Args{Keep: []string{"public.a"}}},
	}
	for _, c := range bad {
		if err := reg.CheckArgs(c.id, c.args); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s keep=%d entries: %v, want ErrInvalidArgs", c.id, len(c.args.Keep),
				err)
		}
	}
}

func TestArgs_SliceParamsCarryTheKeepList(t *testing.T) {
	p := Args{}.params(ArgsSlice, 101)
	if len(p) != 4 {
		t.Fatalf("params = %v, want limit, slices, slice and the keep-list", p)
	}
	if k, ok := p[3].([]string); !ok || k == nil || len(k) != 0 {
		t.Fatalf("empty keep-list param = %#v, want a non-nil empty []string", p[3])
	}
	p = Args{Slices: 3, Slice: 2, Keep: []string{"public.a", `"Odd".b`}}.params(ArgsSlice, 7)
	if p[0] != 7 || p[1] != 3 || p[2] != 2 {
		t.Fatalf("params = %v", p)
	}
	if k := p[3].([]string); len(k) != 2 || k[0] != "public.a" || k[1] != `"Odd".b` {
		t.Fatalf("keep-list param = %#v", p[3])
	}
	if !(Args{}).IsZero() || (Args{Keep: []string{"a"}}).IsZero() ||
		(Args{Slices: 2}).IsZero() {
		t.Fatal("IsZero does not see every field")
	}
}

func TestSequenceRunway_SpecIsV4WithRoomForTheKeepList(t *testing.T) {
	s, ok := Catalog().Spec(SequenceRunwayProbe)
	if !ok {
		t.Fatal("sequence_runway not registered")
	}
	if s.Version != "v4" || s.MaxRows != SequenceTopN+MaxSequenceKeep {
		t.Fatalf("spec version %s rows %d, want v4 and %d", s.Version, s.MaxRows,
			SequenceTopN+MaxSequenceKeep)
	}
	sql := s.Variants[0].SQL
	if strings.Contains(sql, "seqincrement > 0 AND") || !strings.Contains(sql, "$4") {
		t.Fatal("v4 must cover descending sequences and read the keep-list ($4)")
	}
}

func TestSequenceRunway_BindingLimitFollowsTheDirection(t *testing.T) {
	const i8min, i4min, i4max = -9223372036854775808.0, -2147483648.0, 2147483647.0
	cases := []struct {
		name string
		s    SequenceRunway
		want string
	}{
		{"descending bigint owned by int4", SequenceRunway{Increment: -1, MinValue: i8min,
			MaxValue: -1, TypeMin: i8min, TypeMax: 9.2e18, OwnerTypeMin: i4min,
			OwnerTypeMax: i4max}, LimitColumnType},
		{"descending explicit MINVALUE", SequenceRunway{Increment: -1, MinValue: -1000,
			MaxValue: -1, TypeMin: i8min, TypeMax: 9.2e18, OwnerTypeMin: math.NaN(),
			OwnerTypeMax: math.NaN()}, LimitExplicitMax},
		{"descending on its own type", SequenceRunway{Increment: -2, MinValue: i4min,
			MaxValue: -1, TypeMin: i4min, TypeMax: i4max, OwnerTypeMin: i4min,
			OwnerTypeMax: i4max}, LimitSequenceType},
		// An ascending sequence ignores the lower bounds entirely.
		{"ascending ignores the minimum", SequenceRunway{Increment: 1, MinValue: 1,
			MaxValue: i4max, TypeMin: i4min, TypeMax: i4max, OwnerTypeMin: -32768,
			OwnerTypeMax: i4max}, LimitSequenceType},
	}
	for _, c := range cases {
		if got := c.s.BindingLimit(); got != c.want {
			t.Errorf("%s: binding %s, want %s", c.name, got, c.want)
		}
	}
	if !(SequenceRunway{Increment: -1}).Descending() ||
		(SequenceRunway{Increment: 1}).Descending() {
		t.Fatal("Descending does not follow the increment's sign")
	}
}

func TestSequences_DecodesTheLowerBounds(t *testing.T) {
	res := okResult(SequenceRunwayProbe, nil, Row{"sequence": "public.down",
		"data_type": "bigint", "increment_by": int64(-1), "cycle": false,
		"last_value": int64(-2100000000), "min_value": int64(-9223372036854775808),
		"max_value": int64(-1), "type_max": int64(9223372036854775807),
		"type_min": int64(-9223372036854775808), "owner_column": "public.t.id",
		"owner_type": "integer", "owner_type_max": int64(2147483647),
		"owner_type_min": int64(-2147483648), "effective_limit": int64(-2147483648),
		"fraction_used": 0.98, "sequences_total": int64(1), "sequences_scanned": int64(1),
		"sequences_used": int64(1), "sequences_unreadable": int64(0)})
	ss, err := Sequences(res)
	if err != nil || len(ss) != 1 {
		t.Fatalf("decode = %+v (%v)", ss, err)
	}
	s := ss[0]
	if !s.Descending() || s.Limit != -2147483648 || s.OwnerTypeMin != -2147483648 ||
		s.TypeMin != -9223372036854775808 || s.BindingLimit() != LimitColumnType {
		t.Fatalf("descending sequence = %+v (binding %s)", s, s.BindingLimit())
	}
}

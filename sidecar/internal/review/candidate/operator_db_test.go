package candidate

import (
	"errors"
	"reflect"
	"testing"
)

// review-02b: an operator candidate's command, revert and landing are
// stored with it and read back exactly; a step's operator column stays
// NULL and reads back as nil.
func TestStoreDB_OperatorFieldsRoundTrip(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	op, drop := operatorCandidate(), dropCandidate()
	op.Operator.Provider, op.Operator.Mechanism = "rds", "parameter_group"
	s := NewStore(pool)
	if n, err := s.Save(ctx, run, []Candidate{op, drop}); err != nil || n != 2 {
		t.Fatalf("save: n=%d err=%v, want 2 rows", n, err)
	}
	got, err := s.List(ctx, run)
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %d rows, err %v", len(got), err)
	}
	by := map[string]Candidate{}
	for _, g := range got {
		by[g.Generator] = g.Candidate
	}
	if !reflect.DeepEqual(by["config"], op) {
		t.Errorf("operator round trip:\n got %+v\nwant %+v", by["config"], op)
	}
	if by["indexes"].Operator != nil {
		t.Errorf("a step read back operator fields %+v", by["indexes"].Operator)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT operator IS NULL FROM sage.review_candidate
		WHERE run_id = $1 AND generator = 'indexes'`, run).Scan(&isNull); err != nil ||
		!isNull {
		t.Fatalf("a step's operator column: null=%v err=%v, want NULL", isNull, err)
	}
}

// The store refuses an operator candidate whose id is not the hash of its
// command, before writing anything.
func TestStoreDB_SaveRefusesAnOperatorIDOverItsSQL(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	op := operatorCandidate()
	op.ID = NewID(op.Generator, op.Kind, op.Target, op.ForwardSQL)
	if _, err := NewStore(pool).Save(ctx, run, []Candidate{op}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

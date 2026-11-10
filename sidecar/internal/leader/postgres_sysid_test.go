package leader

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// No DB or concurrency category here: isSysidRejected is a pure classifier
// and withSysid's retry is driven by a scripted query; the store on a real
// server is covered in postgres_control_db_test.go and
// postgres_restricted_db_test.go.

func TestIsSysidRejected(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "42501"}, true},
		{&pgconn.PgError{Code: "0A000"}, true},
		{fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42501"}), true},
		{&pgconn.PgError{Code: "57014"}, false},
		{context.DeadlineExceeded, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isSysidRejected(c.err); got != c.want {
			t.Errorf("isSysidRejected(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// scripted answers each call of q with the next error and records whether
// it was asked for the system identifier.
type scripted struct {
	errs  []error
	asked []bool
}

func (s *scripted) q(sysid bool) error {
	s.asked = append(s.asked, sysid)
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func TestWithSysid_StateTransitions(t *testing.T) {
	denied := &pgconn.PgError{Code: "42501"}
	other := errors.New("connection reset")
	cases := []struct {
		name      string
		startOff  bool
		errs      []error
		wantErr   error
		wantAsked []bool
		wantOff   bool
	}{
		{"granted", false, []error{nil}, nil, []bool{true}, false},
		{"rejected then without it", false, []error{denied, nil}, nil,
			[]bool{true, false}, true},
		{"rejected then no row", false, []error{denied, pgx.ErrNoRows}, pgx.ErrNoRows,
			[]bool{true, false}, true},
		{"rejected then another error keeps it on", false, []error{denied, other}, other,
			[]bool{true, false}, false},
		{"unrelated error is not retried", false, []error{other}, other, []bool{true},
			false},
		{"already off is not retried", true, []error{denied}, denied, []bool{false}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &PostgresStore{}
			p.noSysid.Store(c.startOff)
			s := &scripted{errs: c.errs}
			err := p.withSysid(s.q)
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if fmt.Sprint(s.asked) != fmt.Sprint(c.wantAsked) {
				t.Fatalf("calls asked for the identifier %v, want %v", s.asked, c.wantAsked)
			}
			if p.noSysid.Load() != c.wantOff {
				t.Fatalf("identifier off = %v, want %v", p.noSysid.Load(), c.wantOff)
			}
		})
	}
}

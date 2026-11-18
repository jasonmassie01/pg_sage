package whynot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The probe's own bounds, without a server: it asks for a read-only
// transaction, sets both timeouts before it reads, always rolls back, and
// each failing step is an error that names the step and wraps the cause.

type fakeQuerier struct {
	opts     pgx.TxOptions
	beginErr error
	tx       *fakeTx
}

func (q *fakeQuerier) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	q.opts = opts
	if q.beginErr != nil {
		return nil, q.beginErr
	}
	return q.tx, nil
}

type fakeTx struct {
	pgx.Tx     // unused methods panic: the probe must not call them
	execs      []string
	failExec   string
	scanErr    error
	rolledBack bool
	readAfter  []string
}

func (tx *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	tx.execs = append(tx.execs, sql)
	if tx.failExec != "" && strings.Contains(sql, tx.failExec) {
		return pgconn.CommandTag{}, errors.New("set failed")
	}
	return pgconn.CommandTag{}, nil
}

func (tx *fakeTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	tx.readAfter = append([]string(nil), tx.execs...)
	return fakeRow{err: tx.scanErr}
}

func (tx *fakeTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}

type fakeRow struct{ err error }

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = "sage_agent"
	*dest[1].(*bool) = true
	*dest[2].(*bool) = false
	*dest[3].(*bool) = true
	return nil
}

func TestProbePrivilegesIsReadOnlyAndBounded(t *testing.T) {
	q := &fakeQuerier{tx: &fakeTx{}}
	p, err := ProbePrivileges(context.Background(), q)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if q.opts.AccessMode != pgx.ReadOnly {
		t.Fatalf("access mode = %q, want read only", q.opts.AccessMode)
	}
	want := []string{"SET LOCAL statement_timeout = '2s'", "SET LOCAL lock_timeout = '500ms'"}
	if strings.Join(q.tx.readAfter, "|") != strings.Join(want, "|") {
		t.Fatalf("set before the read = %q, want %q", q.tx.readAfter, want)
	}
	if !q.tx.rolledBack {
		t.Fatal("the probe's transaction was not rolled back")
	}
	if p.Role != "sage_agent" || !p.StatStatements || p.QueryTextVisible || !p.Replica {
		t.Fatalf("probe = %+v", p)
	}
}

func TestProbePrivilegesStepErrors(t *testing.T) {
	cause := errors.New("cause")
	cases := []struct {
		name string
		q    *fakeQuerier
		want string
	}{
		{"begin", &fakeQuerier{beginErr: cause}, "begin read-only transaction"},
		{"statement timeout", &fakeQuerier{tx: &fakeTx{failExec: "statement_timeout"}},
			"set statement_timeout"},
		{"lock timeout", &fakeQuerier{tx: &fakeTx{failExec: "lock_timeout"}},
			"set lock_timeout"},
		{"read", &fakeQuerier{tx: &fakeTx{scanErr: cause}}, "read catalog"},
	}
	for _, tc := range cases {
		p, err := ProbePrivileges(context.Background(), tc.q)
		if p != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: probe = %+v, %v; want error naming %q", tc.name, p, err, tc.want)
		}
		if tc.q.tx != nil && !tc.q.tx.rolledBack {
			t.Fatalf("%s: transaction left open", tc.name)
		}
		if tc.q.tx != nil && tc.q.tx.failExec == "lock_timeout" && len(tc.q.tx.readAfter) != 0 {
			t.Fatalf("%s: read the catalog after a failed set", tc.name)
		}
	}
	if _, err := ProbePrivileges(context.Background(),
		&fakeQuerier{beginErr: cause}); !errors.Is(err, cause) {
		t.Fatalf("begin error does not wrap its cause: %v", err)
	}
}

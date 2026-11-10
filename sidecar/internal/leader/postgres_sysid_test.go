package leader

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// No DB, concurrency or state category here: isSysidRejected is a pure
// classifier; the store's use of it is covered in postgres_control_db_test.go.

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

package clone

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sagepool"
)

// Invalid input: a rehearsal clone whose DSN does not parse is refused with
// the fixed sagepool message, and no piece of a password whose '@', ':' or
// '/' broke the parse appears in the error. No connection is attempted.
func TestGuardedConnectUnparseableCloneDSNPrintsNoPasswordPiece(t *testing.T) {
	const one, two = "Clone-Half-One", "Clone-Half-Two"
	prod := IdentitySource(func(context.Context) (ServerIdentity, error) {
		return ServerIdentity{Host: "prod.example", Port: 5432,
			StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, nil
	})
	for _, dsn := range []string{
		"postgres://sage:" + one + "@" + two + "@127.0.0.1:notaport/app",
		"postgres://sage:" + one + ":" + two + "@127.0.0.1:notaport/app",
		"postgres://sage:" + one + "/" + two + "@127.0.0.1:5432/app",
	} {
		pool, err := GuardedConnect(context.Background(), Clone{DSN: dsn, ID: "c1"}, prod)
		if pool != nil {
			pool.Close()
			t.Fatalf("%q: got a pool, want a refusal", dsn)
		}
		if !errors.Is(err, sagepool.ErrUnparseableDSN) {
			t.Fatalf("%q: err = %v, want sagepool.ErrUnparseableDSN", dsn, err)
		}
		want := "parse rehearsal clone DSN: " + sagepool.ErrUnparseableDSN.Error()
		if err.Error() != want {
			t.Errorf("error %q, want exactly %q", err, want)
		}
		for _, piece := range []string{one, two} {
			if strings.Contains(err.Error(), piece) {
				t.Errorf("error %q prints password piece %q", err, piece)
			}
		}
	}
}

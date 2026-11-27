package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sagepool"
)

// Invalid input: an observe-only visit to a DSN that does not parse is
// refused with the fixed sagepool message, and no piece of a password whose
// '@', ':' or '/' broke the parse appears in the error.
func TestObserveConnectUnparseableDSNPrintsNoPasswordPiece(t *testing.T) {
	const one, two = "Observe-Half-One", "Observe-Half-Two"
	for _, dsn := range []string{
		"postgres://sage:" + one + "@" + two + "@127.0.0.1:notaport/app",
		"postgres://sage:" + one + ":" + two + "@127.0.0.1:notaport/app",
		"postgres://sage:" + one + "/" + two + "@127.0.0.1:5432/app",
	} {
		pool, err := observeConnect(context.Background(), dsn)
		if pool != nil {
			pool.Close()
			t.Fatalf("%q: got a pool, want a refusal", dsn)
		}
		if !errors.Is(err, sagepool.ErrUnparseableDSN) {
			t.Fatalf("%q: err = %v, want sagepool.ErrUnparseableDSN", dsn, err)
		}
		if want := "invalid DSN: " + sagepool.ErrUnparseableDSN.Error(); err.Error() != want {
			t.Errorf("error %q, want exactly %q", err, want)
		}
		for _, piece := range []string{one, two} {
			if strings.Contains(err.Error(), piece) {
				t.Errorf("error %q prints password piece %q", err, piece)
			}
		}
	}
}

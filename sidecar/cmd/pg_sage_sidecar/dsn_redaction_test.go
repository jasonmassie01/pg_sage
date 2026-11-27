package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sagepool"
)

// Invalid input and error propagation: the runtime's own DSN paths (the
// meta database, the monitored database) refuse a DSN that does not parse
// with the fixed sagepool message, keep their prefix, and print no piece
// of a password whose '@', ':' or '/' broke the parse. pgx's own error
// prints the DSN through a redaction that misses these shapes.
func TestRuntimeDSNParseErrorsPrintNoPasswordPiece(t *testing.T) {
	bad := []string{cliPoolSplitDSN("@", "notaport"), cliPoolSplitDSN(":", "notaport"),
		cliPoolSplitDSN("/", "5432")}
	opens := map[string]struct {
		prefix string
		open   func(string) error
	}{
		"meta": {"parsing meta-db DSN: ", func(dsn string) error {
			_, err := connectMetaDB(dsn)
			return err
		}},
		"monitored": {"invalid DSN: ", func(dsn string) error {
			_, err := connectMonitoredDB(dsn, 3)
			return err
		}},
	}
	for name, o := range opens {
		for _, dsn := range bad {
			err := o.open(dsn)
			if !errors.Is(err, sagepool.ErrUnparseableDSN) {
				t.Fatalf("%s %q: err = %v, want sagepool.ErrUnparseableDSN", name, dsn, err)
			}
			want := o.prefix + sagepool.ErrUnparseableDSN.Error()
			if err.Error() != want {
				t.Errorf("%s: error %q, want exactly %q", name, err, want)
			}
			for _, piece := range []string{cliPoolHalfOne, cliPoolHalfTwo} {
				if strings.Contains(err.Error(), piece) {
					t.Errorf("%s: error %q prints password piece %q", name, err, piece)
				}
			}
		}
	}
}

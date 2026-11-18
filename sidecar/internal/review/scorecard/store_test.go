package scorecard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// failingDB fails every query with err; seen records the SQL it was asked.
type failingDB struct {
	err  error
	seen []string
}

func (f *failingDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.seen = append(f.seen, sql)
	return nil, f.err
}

func TestStoreRead_ErrorNamesTheQuery(t *testing.T) {
	db := &failingDB{err: errors.New("connection refused")}
	_, err := NewStore(db).Read(context.Background(), "shop", time.Time{}, nil)
	if err == nil || !strings.Contains(err.Error(), `"review runs"`) ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want the query name and the cause", err)
	}
	if len(db.seen) != 1 {
		t.Fatalf("%d queries after a failed first read, want 1", len(db.seen))
	}
}

func TestStoreRead_RefusesNilStoreAndNoDatabase(t *testing.T) {
	var s *Store
	if _, err := s.Read(context.Background(), "shop", time.Time{}, nil); !errors.Is(err,
		ErrNoStore) {
		t.Fatalf("nil store: %v, want ErrNoStore", err)
	}
	if _, err := NewStore(nil).Read(context.Background(), "shop", time.Time{},
		nil); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil pool: %v, want ErrNoStore", err)
	}
	db := &failingDB{err: errors.New("unused")}
	if _, err := NewStore(db).Read(context.Background(), " ", time.Time{}, nil); err == nil ||
		len(db.seen) != 0 {
		t.Fatalf("blank database: %v after %d queries, want a refusal before any", err,
			len(db.seen))
	}
}

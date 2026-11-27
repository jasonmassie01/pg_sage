package apppath

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// No concurrent access tests: Pin and Read are stateless; the session
// they set the path in is the caller's own transaction.

func TestParsePath(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{`"$user", public`, []string{"public"}},
		{`$user,public`, []string{"public"}},
		{`app, public`, []string{"app", "public"}},
		{`App,  "My Schema" , "a""b"`, []string{"app", "My Schema", `a"b`}},
		{`"a,b", c`, []string{"a,b", "c"}},
		{`pg_catalog, pg_temp`, []string{"pg_catalog", "pg_temp"}},
		{``, nil},
		{`  `, nil},
		{`"$user"`, nil},
	} {
		got, err := parsePath(tc.value)
		if err != nil {
			t.Errorf("parsePath(%q) error %v", tc.value, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parsePath(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestParsePathRejectsMalformed(t *testing.T) {
	for _, value := range []string{`app, "broken`, `app,,public`, `"a"b`, `app public`} {
		if got, err := parsePath(value); err == nil {
			t.Errorf("parsePath(%q) = %q, want an error", value, got)
		}
	}
}

func TestRender(t *testing.T) {
	for _, tc := range []struct {
		path []string
		want string
	}{
		{[]string{"public"}, `"public"`},
		{[]string{"My Schema", `a"b`}, `"My Schema", "a""b"`},
		{nil, ""},
	} {
		if got := render(tc.path); got != tc.want {
			t.Errorf("render(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// fakeDB answers the path read with value (nil: SQL NULL) and records
// the statements Pin sends.
type fakeDB struct {
	value   *string
	readErr error
	execErr error
	reads   []string
	execs   []string
	args    [][]any
}

type fakeRow struct{ db *fakeDB }

func (r fakeRow) Scan(dest ...any) error {
	if r.db.readErr != nil {
		return r.db.readErr
	}
	*(dest[0].(**string)) = r.db.value
	return nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.reads = append(f.reads, sql)
	return fakeRow{db: f}
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	f.args = append(f.args, args)
	return pgconn.NewCommandTag("SELECT 1"), f.execErr
}

func text(s string) *string { return &s }

func TestPinSetsTheApplicationPathForTheTransaction(t *testing.T) {
	db := &fakeDB{value: text(`"$user", app, Public`)}
	if err := Pin(t.Context(), db); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if len(db.reads) != 1 || !strings.Contains(db.reads[0], "pg_catalog.pg_db_role_setting") {
		t.Fatalf("reads = %q, want one pg_db_role_setting read", db.reads)
	}
	if len(db.execs) != 1 {
		t.Fatalf("execs = %q, want one set_config", db.execs)
	}
	if !strings.Contains(db.execs[0], "pg_catalog.set_config('search_path', $1, true)") {
		t.Fatalf("exec = %q, want a transaction-local set_config", db.execs[0])
	}
	if want := []any{`"app", "public"`}; !reflect.DeepEqual(db.args[0], want) {
		t.Fatalf("set_config args = %q, want %q", db.args[0], want)
	}
}

// Nil: no setting applies and the server value is unknown, so
// PostgreSQL's default ("$user", public) applies without "$user".
func TestPinDefaultsWhenNoSettingApplies(t *testing.T) {
	db := &fakeDB{}
	if err := Pin(t.Context(), db); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := []any{`"public"`}; len(db.args) != 1 || !reflect.DeepEqual(db.args[0], want) {
		t.Fatalf("set_config args = %q, want %q", db.args, want)
	}
}

// An empty setting is an empty path (pg_catalog only, implicitly).
func TestPinKeepsAnEmptyPathEmpty(t *testing.T) {
	db := &fakeDB{value: text("")}
	if err := Pin(t.Context(), db); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := []any{""}; len(db.args) != 1 || !reflect.DeepEqual(db.args[0], want) {
		t.Fatalf("set_config args = %q, want %q", db.args, want)
	}
}

func TestPinPropagatesTheReadError(t *testing.T) {
	db := &fakeDB{readErr: errors.New("permission denied for table pg_db_role_setting")}
	err := Pin(t.Context(), db)
	if err == nil || !strings.Contains(err.Error(), "read the application search_path") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Pin = %v, want the read error with context", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("execs = %q, want none after a failed read", db.execs)
	}
}

func TestPinPropagatesTheSetError(t *testing.T) {
	db := &fakeDB{value: text("app"), execErr: errors.New("current transaction is aborted")}
	err := Pin(t.Context(), db)
	if err == nil || !strings.Contains(err.Error(), "set the application search_path") ||
		!strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Pin = %v, want the set error with context", err)
	}
}

func TestPinRefusesAnUnreadableSetting(t *testing.T) {
	db := &fakeDB{value: text(`app, "broken`)}
	err := Pin(t.Context(), db)
	if err == nil || !strings.Contains(err.Error(), "broken") ||
		!strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("Pin = %v, want an error naming the setting", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("execs = %q, want none: an unreadable path is never applied", db.execs)
	}
}

func TestPinRefusesANilSession(t *testing.T) {
	if err := Pin(t.Context(), nil); err == nil {
		t.Fatal("Pin(nil) = nil, want an error")
	}
}

func TestReadReturnsTheParsedPath(t *testing.T) {
	got, err := Read(t.Context(), &fakeDB{value: text(`"$user", "App", public`)})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := []string{"App", "public"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %q, want %q", got, want)
	}
}

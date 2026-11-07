package collector

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01: the snapshot carries the holders of this database's xmin
// horizon (probes.XminHorizonSQL, shared with the freeze custodian), the
// lock graph's session rows carry state_change, and prepared transactions
// are this database's only.

// No concurrent access test: scanXminHolder converts one row.

func TestPreparedXactsSQL_OnlyThisDatabase(t *testing.T) {
	if !strings.Contains(preparedXactsSQL, "WHERE database = current_database()") {
		t.Fatalf("preparedXactsSQL reads every database's prepared xacts:\n%s",
			preparedXactsSQL)
	}
}

func TestXminHoldersSQL_IsTheSharedProbe(t *testing.T) {
	if xminHoldersSQL != probes.XminHorizonSQL {
		t.Fatal("the collector must read the shared xmin_horizon query")
	}
	if xminHolderLimit < 1 || xminHolderLimit > 50 {
		t.Fatalf("xminHolderLimit = %d, want a small positive bound", xminHolderLimit)
	}
}

func TestLocksSQL_ReadsStateChange(t *testing.T) {
	if !strings.Contains(locksSQL, "a.state_change") {
		t.Fatal("locksSQL does not read state_change")
	}
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	doc, err := json.Marshal(LockInfo{PID: 1, StateChange: &at})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), `"state_change":"2026-11-06T09:00:00Z"`) {
		t.Fatalf("document %s lacks state_change", doc)
	}
}

// fakeHolderRow scans vals (nil for NULL) into the pointer-to-pointer
// destinations of scanXminHolder.
type fakeHolderRow struct {
	vals []any
	err  error
}

func (r fakeHolderRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case **string:
			*p = ptrOf[string](r.vals[i])
		case **int64:
			*p = ptrOf[int64](r.vals[i])
		case **float64:
			*p = ptrOf[float64](r.vals[i])
		case **bool:
			*p = ptrOf[bool](r.vals[i])
		case **time.Time:
			*p = ptrOf[time.Time](r.vals[i])
		default:
			return errors.New("unexpected destination type")
		}
	}
	return nil
}

func ptrOf[T any](v any) *T {
	if v == nil {
		return nil
	}
	x := v.(T)
	return &x
}

func TestScanXminHolder_Values(t *testing.T) {
	start := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	h, err := scanXminHolder(fakeHolderRow{vals: []any{"session", int64(42), start, nil,
		"idle in transaction", int64(9000), 125.5, "app", nil}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if h.Kind != "session" || h.PID != 42 || !h.BackendStart.Equal(start) ||
		h.State != "idle in transaction" || h.XminAge != 9000 || h.XactAgeS != 125.5 ||
		h.Application != "app" || h.Name != "" || h.Active {
		t.Fatalf("holder = %+v", h)
	}
}

// NULL columns (a slot has no pid, start or transaction age) read as
// zero values, never NaN: the snapshot is stored as JSON.
func TestScanXminHolder_NullsAreZero(t *testing.T) {
	h, err := scanXminHolder(fakeHolderRow{vals: []any{"slot_catalog", nil, nil, "cdc",
		"active", nil, nil, nil, true}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if h.Kind != "slot_catalog" || h.PID != 0 || !h.BackendStart.IsZero() ||
		h.Name != "cdc" || h.XminAge != 0 || h.XactAgeS != 0 || !h.Active {
		t.Fatalf("holder = %+v", h)
	}
	if _, err := json.Marshal(h); err != nil {
		t.Fatalf("a NULL-column holder does not marshal: %v", err)
	}
}

func TestScanXminHolder_ErrorNamesTheRead(t *testing.T) {
	_, err := scanXminHolder(fakeHolderRow{err: errors.New("conn closed")})
	if err == nil || !strings.Contains(err.Error(), "scan xmin holder") ||
		!strings.Contains(err.Error(), "conn closed") {
		t.Fatalf("err = %v, want it wrapped with what was read", err)
	}
}

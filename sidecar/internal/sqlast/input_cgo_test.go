//go:build cgo

package sqlast

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// sizedSQL returns a valid single statement of exactly n bytes.
func sizedSQL(n int) string {
	head := "SELECT 1 -- "
	return head + strings.Repeat("x", n-len(head))
}

type inspection struct {
	name string
	call func(string) error
}

func allInspections() []inspection {
	return []inspection{
		{"InspectDDL", func(s string) error { _, err := InspectDDL(s); return err }},
		{"References", func(s string) error { _, err := References(s); return err }},
		{"Fingerprint", func(s string) error { _, err := Fingerprint(s); return err }},
		{"ReadShape", func(s string) error { _, err := ReadShape(s); return err }},
	}
}

// CHECK-SQL-02: one byte over MaxInputBytes is rejected without calling the
// parser; exactly MaxInputBytes is parsed.
func TestInputSizeLimit(t *testing.T) {
	over := sizedSQL(MaxInputBytes + 1)
	at := sizedSQL(MaxInputBytes)
	if len(over) != 100_001 || len(at) != 100_000 {
		t.Fatalf("fixture sizes %d/%d", len(over), len(at))
	}
	for _, in := range allInspections() {
		before := parseCalls.Load()
		err := in.call(over)
		if !errors.Is(err, ErrRejected) || !errors.Is(err, ErrInputTooLarge) {
			t.Errorf("%s(100001 bytes) error = %v, want ErrRejected+ErrInputTooLarge",
				in.name, err)
		}
		if !strings.Contains(err.Error(), "100000") {
			t.Errorf("%s error %q does not name the limit", in.name, err)
		}
		if calls := parseCalls.Load() - before; calls != 0 {
			t.Errorf("%s called the parser %d times on an oversized input", in.name, calls)
		}
		before = parseCalls.Load()
		if err := in.call(at); errors.Is(err, ErrInputTooLarge) {
			t.Errorf("%s(100000 bytes) error = %v, want accepted size", in.name, err)
		}
		if parseCalls.Load() == before {
			t.Errorf("%s did not parse an input of exactly the limit", in.name)
		}
	}
}

func TestInputAtLimitIsInspected(t *testing.T) {
	got, err := InspectDDL(sizedSQL(MaxInputBytes))
	if err != nil || len(got) != 1 || got[0].Command != "SelectStmt" {
		t.Fatalf("InspectDDL(100000 bytes) = %v, %v; want one SELECT", got, err)
	}
	if fp, err := Fingerprint(sizedSQL(MaxInputBytes)); err != nil || len(fp) != 16 {
		t.Fatalf("Fingerprint(100000 bytes) = %q, %v", fp, err)
	}
}

// Parse errors from every inspection are ErrRejected with a *ParseError.
func TestInspectionsRejectParseErrors(t *testing.T) {
	for _, in := range allInspections() {
		err := in.call("SELECT FROM WHERE (")
		var pe *ParseError
		if !errors.Is(err, ErrRejected) || !errors.As(err, &pe) || pe.Offset < 0 {
			t.Errorf("%s parse error = %v, want *ParseError with an offset", in.name, err)
		}
	}
}

// libpg_query keeps per-thread state; concurrent inspections must give the
// same answers as sequential ones (run under -race).
func TestInspectionsConcurrent(t *testing.T) {
	corpus := loadDDLCorpus(t)
	script, _ := buildCorpusScript(corpus)
	wantDDL, err := InspectDDL(script)
	if err != nil {
		t.Fatalf("InspectDDL error = %v", err)
	}
	const query = "SELECT o.email FROM rv.orders o JOIN rv.items i ON i.order_id = o.id"
	wantRefs, _ := References(query)
	wantFP, _ := Fingerprint(query)
	wantShape, _ := ReadShape("DELETE FROM rv.orders WHERE id = $1")
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				ddl, _ := InspectDDL(script)
				refs, _ := References(query)
				fp, _ := Fingerprint(query)
				shape, _ := ReadShape("DELETE FROM rv.orders WHERE id = $1")
				if !reflect.DeepEqual(ddl, wantDDL) || !reflect.DeepEqual(refs, wantRefs) ||
					fp != wantFP || shape != wantShape {
					errs <- "concurrent result differs from sequential"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if wantFP == "" || wantShape == "" || len(wantRefs.Relations) != 2 {
		t.Fatalf("sequential baseline empty: %q %q %+v", wantFP, wantShape, wantRefs)
	}
}

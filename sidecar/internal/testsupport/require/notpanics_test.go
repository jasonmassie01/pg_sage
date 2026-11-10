package require

import (
	"strings"
	"testing"
)

func TestNotPanics(t *testing.T) {
	rec := &recordingTB{}
	ran := false
	NotPanics(rec, func() { ran = true })
	if !ran || rec.fatals != 0 {
		t.Fatalf("calm fn: ran=%v fatals=%d last=%q", ran, rec.fatals, rec.last)
	}
	NotPanics(rec, func() { panic("boom") }, "stop %s", "db1")
	if rec.fatals != 1 || !strings.Contains(rec.last, "boom") ||
		!strings.Contains(rec.last, "stop db1") {
		t.Fatalf("panicking fn: fatals=%d last=%q, want the value and the message",
			rec.fatals, rec.last)
	}
	// panic(nil) recovers as *runtime.PanicNilError, so it fails too.
	NotPanics(rec, func() { panic(nil) })
	if rec.fatals != 2 {
		t.Fatalf("panic(nil): fatals=%d, want 2", rec.fatals)
	}
}

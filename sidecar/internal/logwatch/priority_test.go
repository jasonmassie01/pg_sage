package logwatch

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// ---------------------------------------------------------------------------
// signalPriority
// ---------------------------------------------------------------------------

func TestSignalPriority_Critical(t *testing.T) {
	s := &rca.Signal{Severity: "critical"}
	got := signalPriority(s)
	if got != 3 {
		t.Errorf("signalPriority(critical) = %d, want 3", got)
	}
}

func TestSignalPriority_Warning(t *testing.T) {
	s := &rca.Signal{Severity: "warning"}
	got := signalPriority(s)
	if got != 2 {
		t.Errorf("signalPriority(warning) = %d, want 2", got)
	}
}

func TestSignalPriority_Info(t *testing.T) {
	s := &rca.Signal{Severity: "info"}
	got := signalPriority(s)
	if got != 1 {
		t.Errorf("signalPriority(info) = %d, want 1", got)
	}
}

func TestSignalPriority_Empty(t *testing.T) {
	s := &rca.Signal{Severity: ""}
	got := signalPriority(s)
	if got != 0 {
		t.Errorf("signalPriority(\"\") = %d, want 0", got)
	}
}

func TestSignalPriority_Unknown(t *testing.T) {
	s := &rca.Signal{Severity: "unknown"}
	got := signalPriority(s)
	if got != 0 {
		t.Errorf("signalPriority(unknown) = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// findLowestPriorityIndex
// ---------------------------------------------------------------------------

func TestFindLowestPriorityIndex_MixedSeverities(t *testing.T) {
	buf := []*rca.Signal{
		{ID: "s0", Severity: "warning", FiredAt: time.Now()},
		{ID: "s1", Severity: "info", FiredAt: time.Now()},
		{ID: "s2", Severity: "critical", FiredAt: time.Now()},
		{ID: "s3", Severity: "warning", FiredAt: time.Now()},
	}
	got := findLowestPriorityIndex(buf)
	if got != 1 {
		t.Errorf("findLowestPriorityIndex(mixed) = %d, want 1 (first info)",
			got)
	}
	if buf[got].ID != "s1" {
		t.Errorf("victim ID = %q, want %q", buf[got].ID, "s1")
	}
}

func TestFindLowestPriorityIndex_AllCritical(t *testing.T) {
	buf := []*rca.Signal{
		{ID: "c0", Severity: "critical", FiredAt: time.Now()},
		{ID: "c1", Severity: "critical", FiredAt: time.Now()},
		{ID: "c2", Severity: "critical", FiredAt: time.Now()},
	}
	got := findLowestPriorityIndex(buf)
	if got != -1 {
		t.Errorf(
			"findLowestPriorityIndex(all critical) = %d, want -1", got)
	}
}

func TestFindLowestPriorityIndex_WarningAndCritical(t *testing.T) {
	buf := []*rca.Signal{
		{ID: "c0", Severity: "critical", FiredAt: time.Now()},
		{ID: "w0", Severity: "warning", FiredAt: time.Now()},
		{ID: "c1", Severity: "critical", FiredAt: time.Now()},
	}
	got := findLowestPriorityIndex(buf)
	if got != 1 {
		t.Errorf(
			"findLowestPriorityIndex(warning+critical) = %d, want 1",
			got)
	}
	if buf[got].ID != "w0" {
		t.Errorf("victim ID = %q, want %q", buf[got].ID, "w0")
	}
}

func TestFindLowestPriorityIndex_Empty(t *testing.T) {
	got := findLowestPriorityIndex(nil)
	if got != -1 {
		t.Errorf("findLowestPriorityIndex(nil) = %d, want -1", got)
	}
}

func TestFindLowestPriorityIndex_SingleInfo(t *testing.T) {
	buf := []*rca.Signal{
		{ID: "i0", Severity: "info", FiredAt: time.Now()},
	}
	got := findLowestPriorityIndex(buf)
	if got != 0 {
		t.Errorf("findLowestPriorityIndex(single info) = %d, want 0",
			got)
	}
}

// ---------------------------------------------------------------------------
// appendCapped — below capacity
// ---------------------------------------------------------------------------

func makeSignals(n int, severity string) []*rca.Signal {
	sigs := make([]*rca.Signal, n)
	for i := range sigs {
		sigs[i] = &rca.Signal{
			ID:       fmt.Sprintf("sig_%s_%d", severity, i),
			Severity: severity,
			FiredAt:  time.Now(),
		}
	}
	return sigs
}

func TestAppendCapped_BelowMax_NoEviction(t *testing.T) {
	buf := makeSignals(100, "info")
	incoming := makeSignals(50, "warning")
	result := appendCapped(buf, incoming)
	if len(result) != 150 {
		t.Errorf("len = %d, want 150 (no eviction needed)", len(result))
	}
	// Verify first and last signals are correct.
	if result[0].ID != "sig_info_0" {
		t.Errorf("first signal = %q, want %q",
			result[0].ID, "sig_info_0")
	}
	if result[149].ID != "sig_warning_49" {
		t.Errorf("last signal = %q, want %q",
			result[149].ID, "sig_warning_49")
	}
}

// ---------------------------------------------------------------------------
// appendCapped — at capacity, evicts lowest priority
// ---------------------------------------------------------------------------

func TestAppendCapped_AtMax_EvictsLowestPriority(t *testing.T) {
	// Fill buffer to exactly maxBufferSize with info signals.
	buf := makeSignals(maxBufferSize, "info")
	// Add one warning — should evict one info.
	incoming := []*rca.Signal{
		{ID: "new_warning", Severity: "warning", FiredAt: time.Now()},
	}
	result := appendCapped(buf, incoming)
	if len(result) != maxBufferSize {
		t.Fatalf("len = %d, want %d", len(result), maxBufferSize)
	}
	// The new warning must be present.
	found := false
	for _, s := range result {
		if s.ID == "new_warning" {
			found = true
			break
		}
	}
	if !found {
		t.Error("new_warning signal not found after eviction")
	}
}

// ---------------------------------------------------------------------------
// appendCapped — all critical, drops oldest
// ---------------------------------------------------------------------------

func TestAppendCapped_AllCriticalBuffer_InfoIncomingEvicted(t *testing.T) {
	buf := makeSignals(maxBufferSize, "critical")
	incoming := []*rca.Signal{
		{ID: "new_info", Severity: "info", FiredAt: time.Now()},
	}
	result := appendCapped(buf, incoming)
	if len(result) != maxBufferSize {
		t.Fatalf("len = %d, want %d", len(result), maxBufferSize)
	}
	// The incoming info signal is the lowest priority, so it gets
	// evicted to make room. All original critical signals survive.
	for _, s := range result {
		if s.ID == "new_info" {
			t.Error("info signal should be evicted when buffer is " +
				"full of critical signals")
		}
	}
	// Original first critical should still be present.
	if result[0].ID != "sig_critical_0" {
		t.Errorf("first critical should survive, got %s", result[0].ID)
	}
}

func TestAppendCapped_AllCritical_CriticalIncoming_DropsOldest(t *testing.T) {
	buf := makeSignals(maxBufferSize, "critical")
	incoming := []*rca.Signal{
		{ID: "new_critical", Severity: "critical", FiredAt: time.Now()},
	}
	result := appendCapped(buf, incoming)
	if len(result) != maxBufferSize {
		t.Fatalf("len = %d, want %d", len(result), maxBufferSize)
	}
	// All are critical, findLowestPriorityIndex returns -1, so
	// oldest (buf[0]) is dropped.
	if result[0].ID == "sig_critical_0" {
		t.Error("oldest critical should be evicted when all are critical")
	}
	// The new critical should be present.
	found := false
	for _, s := range result {
		if s.ID == "new_critical" {
			found = true
			break
		}
	}
	if !found {
		t.Error("new critical signal not found")
	}
}

// ---------------------------------------------------------------------------
// appendCapped — many info + 1 critical → info evicted, critical kept
// ---------------------------------------------------------------------------

func TestAppendCapped_InfoEvictedBeforeCritical(t *testing.T) {
	buf := makeSignals(maxBufferSize, "info")
	incoming := []*rca.Signal{
		{ID: "new_critical", Severity: "critical", FiredAt: time.Now()},
	}
	result := appendCapped(buf, incoming)
	if len(result) != maxBufferSize {
		t.Fatalf("len = %d, want %d", len(result), maxBufferSize)
	}
	// Critical signal must survive.
	var criticalFound bool
	for _, s := range result {
		if s.ID == "new_critical" {
			criticalFound = true
			break
		}
	}
	if !criticalFound {
		t.Error("critical signal was evicted — should never happen " +
			"when info signals exist to evict")
	}
}

// ---------------------------------------------------------------------------
// appendCapped — never evicts critical when non-critical exists
// ---------------------------------------------------------------------------

func TestAppendCapped_NeverEvictsCritical_WhenNonCriticalExists(
	t *testing.T,
) {
	// 9990 info + 10 critical = maxBufferSize
	buf := make([]*rca.Signal, 0, maxBufferSize)
	buf = append(buf, makeSignals(9990, "info")...)
	buf = append(buf, makeSignals(10, "critical")...)

	// Add 5 more warnings — need to evict 5 info signals.
	incoming := makeSignals(5, "warning")
	result := appendCapped(buf, incoming)
	if len(result) != maxBufferSize {
		t.Fatalf("len = %d, want %d", len(result), maxBufferSize)
	}

	// Count critical signals — all 10 must survive.
	critCount := 0
	for _, s := range result {
		if s.Severity == "critical" {
			critCount++
		}
	}
	if critCount != 10 {
		t.Errorf("critical count = %d, want 10 (never evict critical "+
			"when non-critical exists)", critCount)
	}

	// Count warnings — all 5 incoming must survive.
	warnCount := 0
	for _, s := range result {
		if s.Severity == "warning" {
			warnCount++
		}
	}
	if warnCount != 5 {
		t.Errorf("warning count = %d, want 5", warnCount)
	}

	// Info count should be 9985 (9990 - 5 evicted).
	infoCount := 0
	for _, s := range result {
		if s.Severity == "info" {
			infoCount++
		}
	}
	if infoCount != 9985 {
		t.Errorf("info count = %d, want 9985 (5 evicted to make room)",
			infoCount)
	}
}

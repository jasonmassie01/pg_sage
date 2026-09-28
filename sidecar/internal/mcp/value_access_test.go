package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/value"
)

type stubValueReader struct {
	report value.Report
	err    error
	filter value.Filter
}

func (s *stubValueReader) Get(_ context.Context, filter value.Filter) (value.Report, error) {
	s.filter = filter
	return s.report, s.err
}

func TestValueAccessReturnsFleetReportAsMachineJSON(t *testing.T) {
	reader := &stubValueReader{report: value.Report{
		DBAHoursSaved: value.PeriodHours{AllTime: 0.75},
		ByDatabase:    []value.DatabaseHours{{Name: "b", Hours: 0.5}},
		Partial:       true,
		Unavailable:   []string{"c"},
	}}
	result, err := NewValueAccess(reader).GetValue(context.Background())
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if reader.filter != (value.Filter{}) {
		t.Fatalf("MCP value must read the whole fleet, filter = %+v", reader.filter)
	}
	saved, ok := result["dba_hours_saved"].(map[string]any)
	if !ok || saved["all_time"] != 0.75 {
		t.Fatalf("dba_hours_saved = %#v", result["dba_hours_saved"])
	}
	unavailable, ok := result["unavailable"].([]any)
	if result["partial"] != true || !ok || len(unavailable) != 1 || unavailable[0] != "c" {
		t.Fatalf("partial=%v unavailable=%#v", result["partial"], result["unavailable"])
	}
}

func TestValueAccessPropagatesReaderErrorAndFailsClosed(t *testing.T) {
	failure := errors.New("value repository unavailable")
	result, err := NewValueAccess(&stubValueReader{err: failure}).
		GetValue(context.Background())
	if !errors.Is(err, failure) || result != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	var nilAccess *FleetValueAccess
	if _, err := nilAccess.GetValue(context.Background()); !errors.Is(
		err, ErrProductionDependencyUnavailable) {
		t.Fatalf("nil access error = %v", err)
	}
	if _, err := NewValueAccess(nil).GetValue(context.Background()); !errors.Is(
		err, ErrProductionDependencyUnavailable) {
		t.Fatalf("nil reader error = %v", err)
	}
}

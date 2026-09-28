package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestNoticeWindowMeaningChangeLogsOnce(t *testing.T) {
	var lines []string
	logf := func(_, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	noticeWindowMeaningChange("weeknights", logf)
	if len(lines) != 1 || !strings.Contains(lines[0], "weeknights") ||
		!strings.Contains(lines[0], "trust.maintenance_window") {
		t.Fatalf("notice = %v, want one line naming the setting and value", lines)
	}
	lines = nil
	noticeWindowMeaningChange("weekdays 01:00-05:00", logf)
	if len(lines) != 0 {
		t.Fatalf("unchanged window logged %v, want nothing", lines)
	}
}

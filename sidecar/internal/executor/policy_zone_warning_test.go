package executor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-04: startup warns about a policy window that names no zone (it
// is evaluated on the process clock).
//
// No concurrent access tests: the warning runs once at startup.

func TestWarnUnzonedPolicyWindows(t *testing.T) {
	var lines []string
	exec := New(nil, advisoryConfig(), time.Time{}, func(_, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	doc := policy.StaffedProfile()
	raw, err := policy.MarshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	exec.warnUnzonedPolicyWindows(policy.Policy{Version: 3, Document: raw})
	if len(lines) != 1 || !strings.Contains(lines[0], `"weekdays 01:00-05:00"`) ||
		!strings.Contains(lines[0], "no time zone") || !strings.Contains(lines[0], "v3") {
		t.Fatalf("warnings = %q, want one naming the unzoned window", lines)
	}
	lines = nil
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00 America/Chicago", "always"}
	raw, _ = policy.MarshalDocument(doc)
	exec.warnUnzonedPolicyWindows(policy.Policy{Version: 4, Document: raw})
	if len(lines) != 0 {
		t.Fatalf("zoned windows warned: %q", lines)
	}
	exec.warnUnzonedPolicyWindows(policy.Policy{Document: []byte("{")})
	if len(lines) != 0 {
		t.Fatalf("an unparsable policy warned about zones (it is reported elsewhere): %q",
			lines)
	}
}

package main

import (
	"strings"
	"testing"
)

// T6 round 1 (guard-01b): the pressure command's default client is the
// egress client, so a SAGE_API_URL that points at the instance metadata
// range is refused before any request leaves, and the job is told to stop.
// No concurrency test: one command run makes one request.
func TestPressureCmd_DefaultClientRefusesTheMetadataRange(t *testing.T) {
	env := func(name string) string {
		if name == "SAGE_API_URL" {
			return "https://169.254.169.254"
		}
		return "tok"
	}
	code, out, errOut := runPressure(t, env, "--database", "orders")
	if code != pressureExitNoAnswer || out != "" || !strings.Contains(errOut, "link-local") ||
		!strings.Contains(errOut, "stop") {
		t.Fatalf("metadata URL = %d out %q stderr %q, want 3 with the egress refusal",
			code, out, errOut)
	}
}

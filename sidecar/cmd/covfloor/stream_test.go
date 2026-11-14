package main

import (
	"strings"
	"testing"
)

// A setup failure (a missing import) prints "[setup failed]"; it fails like
// a build failure even when the fail event carries no FailedBuild field.
func TestSetupFailureIsReportedAsFailed(t *testing.T) {
	full := modulePrefix + "internal/setup"
	stream := event("start", full, "", "") +
		event("output", full, "", "FAIL\t"+full+" [setup failed]\n") +
		event("fail", full, "", "")
	code, out := covfloor(t, "internal/setup 0 # e2e only\n", stream)
	if code != 1 {
		t.Fatalf("exit %d, want 1 even at floor 0:\n%s", code, out)
	}
	wantContains(t, out, "FAILED", "internal/setup", "build failed")
}

// go test -json can emit an output line far longer than bufio.Scanner's
// 64 KiB default; it must neither stop the read nor count as malformed.
func TestLongStreamLineIsRead(t *testing.T) {
	full := modulePrefix + "internal/a"
	long := event("output", full, "TestA", strings.Repeat("x", 200_000)+"\n")
	code, out := covfloor(t, helperFloors, long+covered("internal/a", "40.0"))
	if code != 1 {
		t.Fatalf("exit %d, want 1 (the package after the long line is under 70%%):\n%s",
			code, out)
	}
	wantContains(t, out, "internal/a", "40.0% < 70%")
	if strings.Contains(out, "malformed") {
		t.Errorf("a long but valid line was counted as malformed:\n%s", out)
	}
}

// The module's root package is matched as ".", not as the module path.
func TestModuleRootPackageIsDot(t *testing.T) {
	stream := strings.ReplaceAll(covered("ROOT", "40.0"), modulePrefix+"ROOT", modulePath)
	code, out := covfloor(t, ". 30 # root package\n", stream)
	if code != 0 {
		t.Fatalf("exit %d, want 0 under the '.' rule (30):\n%s", code, out)
	}
	code, out = covfloor(t, helperFloors, stream)
	if code != 1 {
		t.Fatalf("exit %d, want 1 under the default floor:\n%s", code, out)
	}
	wantContains(t, out, "BELOW     .  40.0% < 70%")
}

// More than ten malformed lines: the count is exact, ten are named.
func TestManyMalformedLinesAreCountedAndTruncated(t *testing.T) {
	stream := covered("internal/a", "90.0") + strings.Repeat("garbage\n", 12)
	code, out := covfloor(t, helperFloors, stream)
	if code != 2 {
		t.Fatalf("exit %d, want 2:\n%s", code, out)
	}
	// The exact list: lines 18 and 19 are not named.
	wantContains(t, out, "12 malformed lines in the go test -json stream "+
		"(lines 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, ...):")
}

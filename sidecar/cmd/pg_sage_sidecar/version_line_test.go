package main

import (
	"runtime"
	"strings"
	"testing"
)

// ci-01: --version names the Go release that built the binary, so an
// operator can check a shipped image or archive against the toolchain pin.
// No concurrency or state surface: versionLine formats build constants.

func TestVersionLineNamesBuildInfoAndGoRelease(t *testing.T) {
	got := versionLine()
	want := "pg_sage " + version + " (commit: " + commit + ", built: " + date +
		", sql-ast: " + sqlASTStatus() + ", go: " + runtime.Version() + ")"
	if got != want {
		t.Fatalf("versionLine() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(runtime.Version(), "go1.") {
		t.Fatalf("runtime.Version() = %q, want a go1.x release", runtime.Version())
	}
}

// The Dockerfiles and scripts/build-local.sh grep for this substring.
func TestVersionLineKeepsTheSQLASTMarker(t *testing.T) {
	if !strings.Contains(versionLine(), "sql-ast: "+sqlASTStatus()+",") {
		t.Fatalf("versionLine() = %q lost the sql-ast segment", versionLine())
	}
}

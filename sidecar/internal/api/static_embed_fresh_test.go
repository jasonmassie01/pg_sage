package api

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

// No concurrent access or database tests: these read the immutable embedded
// filesystem built into the binary.

// embeddedAssetsContaining returns the embedded /assets/*.js chunks that
// carry marker.
func embeddedAssetsContaining(t *testing.T, marker string) []string {
	t.Helper()
	paths, err := fs.Glob(staticFiles, "dist/assets/*.js")
	if err != nil {
		t.Fatalf("glob embedded assets: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("embedded dist has no /assets/*.js chunks")
	}
	var hits []string
	for _, p := range paths {
		body, err := fs.ReadFile(staticFiles, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if bytes.Contains(body, []byte(marker)) {
			hits = append(hits, p)
		}
	}
	return hits
}

// dist-regen: the embedded dashboard carries the Notifications Log read
// failure fix (alerting-06c, 27fe7ef7): the error cell has a test id beside
// the status cell, and a failed read names the HTTP status. A stale build
// has the status cell only.
func TestStaticEmbedded_NotificationLogCarriesReadFailureFix(t *testing.T) {
	withStatus := embeddedAssetsContaining(t, "notify-status-")
	if len(withStatus) != 1 {
		t.Fatalf("chunks with the Notifications Log status cell = %v, want exactly one",
			withStatus)
	}
	body, err := fs.ReadFile(staticFiles, withStatus[0])
	if err != nil {
		t.Fatalf("read %s: %v", withStatus[0], err)
	}
	src := string(body)
	for _, want := range []string{"notify-error-", "Failed to load log"} {
		if !strings.Contains(src, want) {
			t.Errorf("%s lacks %q: embedded dashboard is older than sidecar/web/src",
				withStatus[0], want)
		}
	}
}

// Boundary: a marker no source file carries matches no embedded chunk, so
// the helper cannot pass by matching everything.
func TestStaticEmbedded_AbsentMarkerMatchesNoChunk(t *testing.T) {
	if hits := embeddedAssetsContaining(t, "pg-sage-marker-that-no-source-has"); len(hits) != 0 {
		t.Fatalf("absent marker matched %v", hits)
	}
}

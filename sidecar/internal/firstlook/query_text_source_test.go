package firstlook

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// The first look and the startup checks answer "is query text visible"
// from one expression, so they cannot disagree (firstlook-08).
func TestExtensionsSQLUsesSharedQueryTextVisibility(t *testing.T) {
	if !strings.Contains(extensionsSQL, rolegrants.QueryTextVisibleSQL) {
		t.Fatalf("extensionsSQL does not read rolegrants.QueryTextVisibleSQL:\n%s",
			extensionsSQL)
	}
	if strings.Count(extensionsSQL, "pg_read_all_stats") != 1 {
		t.Fatalf("extensionsSQL tests pg_read_all_stats outside the shared expression:\n%s",
			extensionsSQL)
	}
}

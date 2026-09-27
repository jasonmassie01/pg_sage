package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shipped artifacts must link libpg_query: without cgo the executor falls
// back to text-only SQL validation (internal/sqlast).
func TestShippedBuildsEnableCgoForSQLAST(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	for path, needles := range map[string][]string{
		filepath.Join(sidecarRoot, "Dockerfile"): {
			"build-base", "CGO_ENABLED=1", `grep -q "sql-ast: libpg_query"`,
		},
		filepath.Join(sidecarRoot, "..", ".goreleaser.yml"): {
			"CGO_ENABLED=1", "CC=o64-clang", "CC=oa64-clang",
			"CC=x86_64-linux-gnu-gcc", "CC=aarch64-linux-gnu-gcc",
		},
		filepath.Join(sidecarRoot, "..", ".github", "workflows", "ci.yml"): {
			"ghcr.io/goreleaser/goreleaser-cross:",
		},
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		if strings.Contains(text, "CGO_ENABLED=0") {
			t.Errorf("%s disables cgo", path)
		}
		for _, needle := range needles {
			if !strings.Contains(text, needle) {
				t.Errorf("%s is missing %q", path, needle)
			}
		}
	}
}

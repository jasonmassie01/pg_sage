package workpack

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// evid-02a: a stored pack never changes. No code in the module updates
// sage.workload_packs (Save inserts or reuses; retention only deletes).
// No DB test here (build_db_test.go saves on a real server); no
// concurrency test: a source scan and pure validation.

var updatePacks = regexp.MustCompile(`(?i)UPDATE\s+sage\.workload_packs`)

func TestStore_NoUpdateStatementOnTheTable(t *testing.T) {
	root := filepath.Join("..", "..", "..") // the module root (sidecar/)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "store_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if updatePacks.Match(src) {
			t.Errorf("%s updates sage.workload_packs: a stored pack must never change", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan the module: %v", err)
	}
}

func TestValidSHA_AcceptsOnlyLowerHex64(t *testing.T) {
	good := strings.Repeat("0a", 32)
	for s, want := range map[string]bool{good: true, strings.ToUpper(good): false,
		good[:63]: false, good + "0": false, "": false,
		strings.Repeat("g", 64): false, "../" + good[3:]: false} {
		if got := ValidSHA(s); got != want {
			t.Errorf("ValidSHA(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestMetrics_CountBuildsAndRememberBytes(t *testing.T) {
	NoteBuild("mx_db", ResultBuilt, 1234)
	NoteBuild("mx_db", ResultReused, 1234)
	NoteBuild("mx_db", ResultFailed, 0)
	NoteBuild("mx_db", ResultReused, 99)
	counts := map[string]uint64{}
	for _, c := range BuildCounts() {
		if c.Database == "mx_db" {
			counts[c.Result] = c.Count
		}
	}
	if counts[ResultBuilt] != 1 || counts[ResultReused] != 2 || counts[ResultFailed] != 1 {
		t.Fatalf("counts %v", counts)
	}
	for _, b := range PackBytes() {
		if b.Database == "mx_db" && b.Bytes != 99 {
			t.Fatalf("bytes %d, want the last pack's 99", b.Bytes)
		}
	}
}

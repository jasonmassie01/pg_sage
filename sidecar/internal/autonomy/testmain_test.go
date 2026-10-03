package autonomy

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The schema guard scans the whole catalog, so this package runs on its
// own fixture database: other packages' tables cannot leak into a scan.
func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/autonomy"))
}

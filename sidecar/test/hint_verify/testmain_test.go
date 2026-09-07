package hint_verify

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "test/hint_verify"))
}

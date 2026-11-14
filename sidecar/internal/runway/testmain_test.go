package runway

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.RunBootstrapped(m.Run, "internal/runway", schema.Bootstrap))
}

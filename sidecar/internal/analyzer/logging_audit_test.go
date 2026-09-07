package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// No shared-state concurrency: each case owns its pool and captured output.
func TestAuditDatabaseWarningsPreserveError(t *testing.T) {
	for _, name := range []string{"extension", "work_mem"} {
		t.Run(name, func(t *testing.T) {
			pool, err := pgxpool.New(context.Background(),
				"postgres://invalid@127.0.0.1:1/invalid?connect_timeout=1")
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			cfg := &config.Config{}
			cfg.Analyzer.WorkMemPromotionThreshold = 5
			var logs []string
			a := &Analyzer{pool: pool, cfg: cfg, logFn: func(level, format string, args ...any) {
				logs = append(logs, level+": "+fmt.Sprintf(format, args...))
			}}
			if name == "extension" {
				a.checkExtensionDrift(context.Background())
			} else {
				a.checkWorkMemPromotion(context.Background())
			}
			if len(logs) != 1 || strings.Contains(logs[0], "%!") ||
				!strings.Contains(logs[0], "failed to connect") {
				t.Fatalf("expected actionable formatted connection warning; got %q", logs)
			}
		})
	}
}

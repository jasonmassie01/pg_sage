package autonomy

import (
	"context"
	"fmt"
)

// runSchemaGuardNow runs one database's schema guard scan synchronously,
// through the same gate a worker cycle takes, for tests that need a scan
// to have finished. Production asks for a scan with RequestSchemaGuard.
func runSchemaGuardNow(ctx context.Context, s *Supervisor, database string) error {
	for _, config := range s.configs {
		if config.Database == database {
			return runSchemaGuard(ctx, config)
		}
	}
	return fmt.Errorf("schema guard database %s is unknown", database)
}

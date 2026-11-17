package schema

// MigrationStepNames lists the migration steps this build applies, in
// order: the ids sage.schema_migrations records once a bootstrap ran them.
// pg_sage doctor compares the two (ux-04a). The slice is a fresh copy.
func MigrationStepNames() []string {
	steps := migrationSteps()
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.name)
	}
	return names
}

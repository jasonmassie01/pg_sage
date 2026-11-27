//go:build !cgo

package sqlast

// InspectStatements needs the libpg_query parser; without cgo it reports
// ErrUnavailable so callers treat the script as unvalidated.
func InspectStatements(string) ([]Statement, error) { return nil, ErrUnavailable }

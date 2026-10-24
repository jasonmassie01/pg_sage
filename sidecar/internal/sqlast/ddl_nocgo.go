//go:build !cgo

package sqlast

// InspectDDL needs the libpg_query parser; without cgo it reports
// ErrUnavailable so callers fall back to text mode.
func InspectDDL(string) ([]DDLStatement, error) { return nil, ErrUnavailable }

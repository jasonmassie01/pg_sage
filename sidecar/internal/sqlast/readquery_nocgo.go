//go:build !cgo

package sqlast

// InspectReadQuery needs the libpg_query parser; without cgo it reports
// ErrUnavailable so callers fail closed.
func InspectReadQuery(string) (ReadQuery, error) { return ReadQuery{}, ErrUnavailable }

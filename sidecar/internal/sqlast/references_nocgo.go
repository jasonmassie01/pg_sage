//go:build !cgo

package sqlast

// References needs the libpg_query parser; without cgo it reports
// ErrUnavailable so callers fall back to text mode.
func References(string) (Refs, error) { return Refs{}, ErrUnavailable }

//go:build !cgo

package sqlast

// ReadShape needs the libpg_query parser; without cgo it reports
// ErrUnavailable so callers fall back to text mode.
func ReadShape(string) (string, error) { return "", ErrUnavailable }

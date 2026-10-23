//go:build !cgo

package sqlast

// Fingerprint needs the libpg_query parser; without cgo it reports
// ErrUnavailable so callers fall back to text mode.
func Fingerprint(string) (string, error) { return "", ErrUnavailable }

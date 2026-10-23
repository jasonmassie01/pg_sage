//go:build !cgo

package changereview

// astKeys is unavailable without cgo: statements are keyed by text.
func astKeys(string) ([]string, bool) { return nil, false }

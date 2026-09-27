//go:build !cgo

package sqlast

// Available reports whether parse-tree validation is compiled in.
func Available() bool { return false }

// Check is a no-op without cgo; the text validator stays in charge.
func Check(string, Rules) error { return nil }

//go:build !pinpath

package selfmonitor

// pinPathHarness is off: sessions keep the role's search_path until
// security-01c-r pins it unconditionally (the pinpath tag turns it on).
const pinPathHarness = false

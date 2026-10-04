//go:build !unix && !windows

package selfbudget

import "time"

// ProcessCPU is unknown on this platform.
func ProcessCPU() (time.Duration, bool) { return 0, false }

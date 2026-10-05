//go:build unix

package selfbudget

import (
	"syscall"
	"time"
)

// ProcessCPU is the process's user + system CPU time so far.
func ProcessCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}

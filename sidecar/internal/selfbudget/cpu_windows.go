//go:build windows

package selfbudget

import (
	"syscall"
	"time"
)

// ProcessCPU is the process's user + kernel CPU time so far.
func ProcessCPU() (time.Duration, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	// A FILETIME counts 100-nanosecond intervals.
	ticks := func(f syscall.Filetime) int64 {
		return int64(f.HighDateTime)<<32 | int64(f.LowDateTime)
	}
	return time.Duration((ticks(kernel) + ticks(user)) * 100), true
}

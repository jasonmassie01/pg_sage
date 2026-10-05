package tuning

import "github.com/pg-sage/sidecar/internal/advisor"

// hostMemory is the live host memory when telemetry knows it, else the
// static figure (0 = unknown: shared_buffers stays advisory).
func (s Settings) hostMemory() advisor.HostMemory {
	if s.HostMemory != nil {
		if m := s.HostMemory(); m.TotalBytes > 0 {
			return m
		}
	}
	return advisor.HostMemory{TotalBytes: s.HostMemoryBytes}
}

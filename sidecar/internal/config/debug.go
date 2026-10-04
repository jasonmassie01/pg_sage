package config

// DebugConfig holds diagnostics that are off unless an operator asks.
type DebugConfig struct {
	PprofEnabled bool `yaml:"pprof_enabled" doc:"Serve the Go profiler (CPU, heap, goroutines, trace) at /api/v1/debug/pprof/ on the API listener, to signed-in admins only. Profiles reveal code paths and memory; enable while diagnosing, then turn off. Default: false."`
}

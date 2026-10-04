package api

import (
	"net/http"
	"net/http/pprof"
	"runtime"
	runtimepprof "runtime/pprof"
	"strconv"
)

// Authenticated pprof (debug.pprof_enabled, off by default): the Go
// profiler on the API listener, so a CPU profile or goroutine dump of a
// running sidecar needs no signal and no restart. It is registered on the
// API mux, behind the session middleware every API route uses, and only
// admins may read it: profiles reveal code paths, memory and command line.

const pprofPrefix = "/api/v1/debug/pprof/"

// maxPprofSeconds keeps a CPU profile or trace inside the API's 30 s
// request deadline.
const maxPprofSeconds = 25

// defaultPprofSeconds replaces net/http/pprof's 30 s default, which the
// request deadline would cut short.
const defaultPprofSeconds = 10

// registerPprofRoutes serves the profiler to admins when enabled; when
// disabled nothing is registered (404).
func registerPprofRoutes(mux *http.ServeMux, enabled bool) {
	if !enabled {
		return
	}
	// Block and mutex profiles stay empty unless sampling is on; enabling
	// pprof turns on light sampling (a blocking event per ms spent
	// blocked, one mutex contention in 100).
	runtime.SetBlockProfileRate(1_000_000)
	runtime.SetMutexProfileFraction(100)
	admin := RequireRole("admin")
	mux.Handle("GET "+pprofPrefix+"{$}", admin(noStore(http.HandlerFunc(pprof.Index))))
	mux.Handle("GET "+pprofPrefix+"{name}", admin(noStore(http.HandlerFunc(pprofNamed))))
}

// pprofNamed serves one profile by name.
func pprofNamed(w http.ResponseWriter, r *http.Request) {
	switch name := r.PathValue("name"); name {
	case "cmdline":
		pprof.Cmdline(w, r)
	case "symbol":
		pprof.Symbol(w, r)
	case "profile":
		withPprofSeconds(pprof.Profile)(w, r)
	case "trace":
		withPprofSeconds(pprof.Trace)(w, r)
	default:
		if runtimepprof.Lookup(name) == nil {
			jsonError(w, "unknown profile", http.StatusNotFound)
			return
		}
		pprof.Handler(name).ServeHTTP(w, r)
	}
}

// withPprofSeconds bounds ?seconds= to 1-25 (default 10).
func withPprofSeconds(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		raw := q.Get("seconds")
		if raw == "" {
			q.Set("seconds", strconv.Itoa(defaultPprofSeconds))
			r.URL.RawQuery = q.Encode()
		} else if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > maxPprofSeconds {
			jsonError(w, "seconds must be an integer 1-25", http.StatusBadRequest)
			return
		}
		h(w, r)
	}
}

// noStore keeps profiles out of every cache.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

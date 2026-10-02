package srebench

import "testing"

// The full bench runs only in its own CI step, so the ./... suites stay
// within their timeouts. Anything but an explicit "1" keeps it off.
func TestBenchRunRequested(t *testing.T) {
	for value, want := range map[string]bool{
		"1": true, " 1 ": true, "": false, "0": false, "true": false, "yes": false,
	} {
		got := BenchRunRequested(func(key string) string {
			if key != EnvRun {
				t.Fatalf("read %q, want %q", key, EnvRun)
			}
			return value
		})
		if got != want {
			t.Fatalf("BenchRunRequested(%q) = %v, want %v", value, got, want)
		}
	}
}

// No concurrent access tests: BenchRunRequested is pure.

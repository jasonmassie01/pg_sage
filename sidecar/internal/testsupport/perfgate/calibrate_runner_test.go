package perfgate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// The calibration records the runner's type, its CPU model as
// /proc/cpuinfo names it, so reference runs can be grouped by runner type.

const x86CPUInfo = "processor\t: 0\nvendor_id\t: AuthenticAMD\n" +
	"model name\t: AMD EPYC 7763 64-Core Processor\nflags\t\t: fpu vme\n\n" +
	"processor\t: 1\nvendor_id\t: AuthenticAMD\n" +
	"model name\t: AMD EPYC 7763 64-Core Processor\n"

const armCPUInfo = "processor\t: 0\nBogoMIPS\t: 50.00\nCPU implementer\t: 0x41\n" +
	"CPU part\t: 0xd0c\n"

func TestParseCPUModel(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"x86: the first processor's model", x86CPUInfo, "AMD EPYC 7763 64-Core Processor"},
		{"ARM names no model", armCPUInfo, ""},
		{"empty", "", ""},
		{"spaces around key and value",
			"  model name   :   Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz  \n",
			"Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz"},
		{"a value holding a colon", "model name\t: Vendor: Part 9\n", "Vendor: Part 9"},
		{"a key without a colon is no model", "model name\n", ""},
		{"a longer key is another key", "model name extra\t: X\nmodel name\t: Y\n", "Y"},
		{"two models: the first", "model name\t: First\nmodel name\t: Second\n", "First"},
	}
	for _, c := range cases {
		got, err := parseCPUModel(strings.NewReader(c.in))
		if err != nil || got != c.want {
			t.Fatalf("%s: model %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestParseCPUModelReportsReadErrors(t *testing.T) {
	got, err := parseCPUModel(iotest.ErrReader(errors.New("device gone")))
	if err == nil || got != "" || !strings.Contains(err.Error(), "device gone") {
		t.Fatalf("model %q, %v; want the read error", got, err)
	}
}

// A missing file is a runner that is not Linux: no model, no error. A file
// that cannot be read is an error naming it.
func TestCPUModelFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	if err := os.WriteFile(path, []byte(x86CPUInfo), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := cpuModelFrom(path); err != nil ||
		got != "AMD EPYC 7763 64-Core Processor" {
		t.Fatalf("model %q, %v; want the file's first model", got, err)
	}
	if got, err := cpuModelFrom(filepath.Join(dir, "absent")); err != nil || got != "" {
		t.Fatalf("absent file: model %q, %v; want none and no error", got, err)
	}
	if got, err := cpuModelFrom(dir); err == nil || got != "" ||
		!strings.Contains(err.Error(), dir) {
		t.Fatalf("unreadable file: model %q, %v; want an error naming %s", got, err, dir)
	}
}

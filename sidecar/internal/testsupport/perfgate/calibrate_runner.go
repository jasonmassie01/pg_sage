package perfgate

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// cpuinfoPath names the runner's CPU on Linux, where CI runs.
const cpuinfoPath = "/proc/cpuinfo"

// cpuModelFrom reads the first processor's model name from a cpuinfo
// file. A missing file is a runner that is not Linux: no model, no error.
func cpuModelFrom(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("perfgate calibration: CPU model: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush
	model, err := parseCPUModel(f)
	if err != nil {
		return "", fmt.Errorf("perfgate calibration: CPU model: %w", err)
	}
	return model, nil
}

// parseCPUModel returns the first "model name" of cpuinfo text, or ""
// when it names none (ARM's names none).
func parseCPUModel(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", scanner.Err()
}

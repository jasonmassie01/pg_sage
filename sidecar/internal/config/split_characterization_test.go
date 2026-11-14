package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// config-30 characterization: config.go was split by concern (types,
// load, validate, defaults). These tests pin what the split must not
// change: every default (golden JSON), the exact error each validator
// returns and which one wins when two fail, the load warnings byte for
// byte, and that the load path takes no hot-reload lock.
//
// State transitions: not applicable (config load has no states; the
// controller's generations live in controller.go and are untouched).
// Integration: not applicable (no database access in this package's
// moved code).

var updateSplitGolden = flag.Bool("config.update-golden", false,
	"rewrite testdata/default_config.golden.json from DefaultConfig()")

const defaultConfigGolden = "testdata/default_config.golden.json"

// isolateSplitEnv runs a test in an empty directory with every
// registered environment variable (and its _FILE twin) unset.
func isolateSplitEnv(t *testing.T) {
	t.Helper()
	chdirTemp(t)
	for _, v := range envRegistry {
		for _, name := range []string{v.Name, v.Name + secretFileSuffix} {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatalf("unset %s: %v", name, err)
			}
		}
	}
}

func marshalConfig(t *testing.T, cfg *Config) string {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return string(data) + "\n"
}

func readGolden(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(defaultConfigGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func TestSplit_DefaultConfigMatchesGolden(t *testing.T) {
	got := marshalConfig(t, DefaultConfig())
	if *updateSplitGolden {
		if err := os.MkdirAll(filepath.Dir(defaultConfigGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(defaultConfigGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readGolden(t); got != want {
		t.Fatalf("DefaultConfig() JSON differs from %s; a default moved or "+
			"changed in the split", defaultConfigGolden)
	}
}

// The golden holds real defaults, not zero values (default value masking).
func TestSplit_GoldenPinsNonZeroDefaults(t *testing.T) {
	var golden map[string]any
	if err := json.Unmarshal([]byte(readGolden(t)), &golden); err != nil {
		t.Fatalf("golden is not JSON: %v", err)
	}
	analyzer, ok := golden["Analyzer"].(map[string]any)
	if !ok {
		t.Fatalf("golden lacks Analyzer: %v", golden["Analyzer"])
	}
	if got := analyzer["UnusedIndexWindowDays"]; got != float64(7) {
		t.Fatalf("Analyzer.UnusedIndexWindowDays = %v, want 7", got)
	}
	if golden["Mode"] != "standalone" {
		t.Fatalf("Mode = %v, want standalone", golden["Mode"])
	}
	collector := golden["Collector"].(map[string]any)
	if collector["IntervalSeconds"] != float64(60) {
		t.Fatalf("Collector.IntervalSeconds = %v, want 60", collector["IntervalSeconds"])
	}
}

// DefaultConfig returns a fresh value each call: callers mutate it.
func TestSplit_DefaultConfigIsFresh(t *testing.T) {
	a, b := DefaultConfig(), DefaultConfig()
	a.Collector.IntervalSeconds = 1
	a.Analyzer.LockChain.SafePatterns[0] = "changed"
	if b.Collector.IntervalSeconds != 60 ||
		b.Analyzer.LockChain.SafePatterns[0] != "pg_sage" {
		t.Fatalf("DefaultConfig values share state: %d %q",
			b.Collector.IntervalSeconds, b.Analyzer.LockChain.SafePatterns[0])
	}
}

func loadCapture(t *testing.T, args []string) (*Config, string, error) {
	t.Helper()
	var out bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &out
	t.Cleanup(func() { configWarningOutput = previous })
	cfg, err := Load(args)
	return cfg, out.String(), err
}

// Nil/empty: no file and an empty file both load the same config, with no
// warnings, and the file-independent fields keep their defaults.
func TestSplit_LoadWithoutFileAndWithEmptyFileAgree(t *testing.T) {
	isolateSplitEnv(t)
	noFile, warnNoFile, err := loadCapture(t, []string{})
	if err != nil {
		t.Fatalf("Load without a file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	empty, warnEmpty, err := loadCapture(t, []string{"--config", path})
	if err != nil {
		t.Fatalf("Load with an empty file: %v", err)
	}
	if warnNoFile != "" || warnEmpty != "" {
		t.Fatalf("warnings without settings: %q / %q", warnNoFile, warnEmpty)
	}
	if noFile.ConfigPath != "" || empty.ConfigPath != path {
		t.Fatalf("ConfigPath = %q / %q, want empty / %q",
			noFile.ConfigPath, empty.ConfigPath, path)
	}
	empty.ConfigPath = ""
	if a, b := marshalConfig(t, noFile), marshalConfig(t, empty); a != b {
		t.Fatal("an empty file loads a different config than no file")
	}
	if noFile.Mode != "standalone" || noFile.Analyzer.UnusedIndexWindowDays != 7 ||
		noFile.Collector.IntervalSeconds != 60 || noFile.Postgres.Port != 5432 {
		t.Fatalf("loaded defaults masked: mode %q window %d interval %d port %d",
			noFile.Mode, noFile.Analyzer.UnusedIndexWindowDays,
			noFile.Collector.IntervalSeconds, noFile.Postgres.Port)
	}
}

// The load warnings are written once each, in order, byte for byte.
func TestSplit_LoadWarningsByteForByte(t *testing.T) {
	isolateSplitEnv(t)
	path := filepath.Join(t.TempDir(), "warn.yaml")
	body := "safety:\n  cpu_ceiling_pct: 50\nllm:\n  api_key: ${SPLIT_UNSET_KEY}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPLIT_UNSET_KEY", "")
	_, out, err := loadCapture(t, []string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "WARNING: config " + strconv.Quote(path) + " references ${SPLIT_UNSET_KEY} but it " +
		"is not set in the environment\n" +
		"WARN: safety.host_cpu_ceiling_pct is unset, so index verification inherits " +
		"safety.cpu_ceiling_pct=50 as its host CPU ceiling; cpu_ceiling_pct is the " +
		"collector's active sessions / max_connections ceiling. Set " +
		"safety.host_cpu_ceiling_pct to tune them separately.\n"
	if out != want {
		t.Fatalf("warnings:\n%q\nwant:\n%q", out, want)
	}
}

func TestSplit_LoadUnreadablePathNamesIt(t *testing.T) {
	isolateSplitEnv(t)
	path := filepath.Join(t.TempDir(), "missing", "sage.yaml")
	cfg, _, err := loadCapture(t, []string{"--config", path})
	if err == nil || cfg != nil {
		t.Fatalf("Load(%s) = %v, %v; want nil and an error", path, cfg, err)
	}
	if !strings.HasPrefix(err.Error(), "loading config "+path+": ") ||
		!os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("error %q does not name the path as a missing file", err)
	}
}

func unwrapAll(err error) error {
	for {
		next, ok := err.(interface{ Unwrap() error })
		if !ok || next.Unwrap() == nil {
			return err
		}
		err = next.Unwrap()
	}
}

func TestSplit_LoadRetiredTopLevelKey(t *testing.T) {
	isolateSplitEnv(t)
	path := filepath.Join(t.TempDir(), "retired.yaml")
	if err := os.WriteFile(path, []byte("notifications:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadCapture(t, []string{"--config", path})
	want := "loading config " + path + ": configuration key \"notifications\" is " +
		"retired; use \"alerting\" instead"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Load reports the mode error before any validator runs.
func TestSplit_LoadModeErrorWinsOverValidation(t *testing.T) {
	isolateSplitEnv(t)
	path := filepath.Join(t.TempDir(), "mode.yaml")
	body := "mode: bogus\ncollector:\n  interval_seconds: 0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadCapture(t, []string{"--config", path})
	want := "invalid mode \"bogus\": must be 'standalone', 'fleet', or 'meta'"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	t.Setenv("SAGE_MODE", "")
	if err := os.WriteFile(path, []byte("collector:\n  interval_seconds: 0\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = loadCapture(t, []string{"--config", path})
	want = "config validation: collector.interval_seconds must be positive"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

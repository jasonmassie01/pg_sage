package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tuning-11: tuner.param_shape_min_variants defaults to 20 with no config
// file and is validated to 2-1000.

func TestDefaultConfig_ParamShapeMinVariants(t *testing.T) {
	if got := DefaultConfig().Tuner.ParamShapeMinVariants; got != 20 ||
		DefaultTunerParamShapeMinVariants != 20 {
		t.Fatalf("param_shape_min_variants default = %d (const %d), want 20", got,
			DefaultTunerParamShapeMinVariants)
	}
}

func writeParamShapeConfig(t *testing.T, tunerBody string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "mode: standalone\npostgres:\n  host: localhost\ntuner:\n" + tunerBody
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_ParamShapeMinVariants(t *testing.T) {
	cfg, err := Load([]string{"--config", writeParamShapeConfig(t,
		"  param_shape_min_variants: 50\n")})
	if err != nil || cfg.Tuner.ParamShapeMinVariants != 50 {
		t.Fatalf("explicit 50: %v, %v", cfg, err)
	}
	cfg, err = Load([]string{"--config", writeParamShapeConfig(t, "  enabled: true\n")})
	if err != nil || cfg.Tuner.ParamShapeMinVariants != 20 {
		t.Fatalf("a file without the key keeps 20: %v, %v", cfg, err)
	}
}

// Boundary and invalid input: 2 and 1000 load; 0, 1, 1001 and -5 are
// refused with the key named.
func TestLoad_ParamShapeMinVariantsBounds(t *testing.T) {
	for _, v := range []int{2, 1000} {
		cfg, err := Load([]string{"--config", writeParamShapeConfig(t,
			"  param_shape_min_variants: "+strconv.Itoa(v)+"\n")})
		if err != nil || cfg.Tuner.ParamShapeMinVariants != v {
			t.Errorf("%d: %v, %v", v, cfg, err)
		}
	}
	for _, v := range []int{0, 1, 1001, -5} {
		_, err := Load([]string{"--config", writeParamShapeConfig(t,
			"  param_shape_min_variants: "+strconv.Itoa(v)+"\n")})
		if err == nil || !strings.Contains(err.Error(), "tuner.param_shape_min_variants") ||
			!strings.Contains(err.Error(), "2-1000") {
			t.Errorf("%d: err = %v, want a 2-1000 range error naming the key", v, err)
		}
	}
}

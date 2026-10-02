package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Sage SRE pooler telemetry (sre.poolers, CHECK-04): optional, off unless
// a pooler is listed. The admin-console DSN is a secret: it comes from
// ${ENV} substitution or a mounted file, is tagged secret, and never
// appears in a validation error.

func TestSREPoolers_DefaultIsNone(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.SRE.Poolers) != 0 {
		t.Fatalf("default poolers = %+v, want none", cfg.SRE.Poolers)
	}
}

func TestSREPoolers_LoadsWithDefaultsAndEnvDSN(t *testing.T) {
	t.Setenv("PGB_ADMIN_DSN", "postgres://stats:s3cret@pgb:6432/pgbouncer")
	cfg, err := loadRCAYAML(t, "sre:\n  poolers:\n    - name: pgb-1\n"+
		"      dsn: ${PGB_ADMIN_DSN}\n      databases: [orders]\n"+
		"      pools: [orders, orders_ro]\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.SRE.Poolers) != 1 {
		t.Fatalf("poolers = %+v", cfg.SRE.Poolers)
	}
	p := cfg.SRE.Poolers[0]
	if p.Name != "pgb-1" || p.DSN != "postgres://stats:s3cret@pgb:6432/pgbouncer" ||
		len(p.Databases) != 1 || len(p.Pools) != 2 || p.TimeoutMS != 1000 {
		t.Fatalf("pooler = %+v (timeout must default to 1000 ms)", p)
	}
	if p.Timeout() != time.Second {
		t.Fatalf("Timeout() = %s", p.Timeout())
	}
	dsn, err := p.ResolveDSN()
	if err != nil || dsn != p.DSN {
		t.Fatalf("ResolveDSN = %q (%v)", dsn, err)
	}
}

func TestSREPoolers_DSNFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgb.dsn")
	if err := os.WriteFile(path, []byte("postgres://stats:f1le@pgb/pgbouncer\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadRCAYAML(t, "sre:\n  poolers:\n    - name: pgb-1\n"+
		"      dsn_file: "+filepath.ToSlash(path)+"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dsn, err := cfg.SRE.Poolers[0].ResolveDSN()
	if err != nil || dsn != "postgres://stats:f1le@pgb/pgbouncer" {
		t.Fatalf("dsn from file = %q (%v), want it trimmed", dsn, err)
	}
	missing := SREPoolerConfig{Name: "pgb-1", DSNFile: filepath.Join(t.TempDir(), "none")}
	if _, err := missing.ResolveDSN(); err == nil {
		t.Fatal("a missing dsn_file resolved")
	}
}

func TestSREPoolers_Invalid(t *testing.T) {
	const secret = "postgres://stats:LEAK-CANARY@pgb/pgbouncer"
	cases := map[string]string{
		"no name":      "    - dsn: " + secret + "\n",
		"bad name":     "    - name: \"pgb 1;\"\n      dsn: " + secret + "\n",
		"no dsn":       "    - name: pgb-1\n",
		"dsn and file": "    - name: pgb-1\n      dsn: " + secret + "\n      dsn_file: /x\n",
		"duplicate": "    - name: pgb-1\n      dsn: " + secret + "\n" +
			"    - name: pgb-1\n      dsn: " + secret + "\n",
		"timeout low":  "    - name: pgb-1\n      dsn: " + secret + "\n      timeout_ms: 50\n",
		"timeout high": "    - name: pgb-1\n      dsn: " + secret + "\n      timeout_ms: 2001\n",
		"empty pool":   "    - name: pgb-1\n      dsn: " + secret + "\n      pools: [\"\"]\n",
		"unknown key":  "    - name: pgb-1\n      dsn: " + secret + "\n      sql: SHOW CLIENTS\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadRCAYAML(t, "sre:\n  poolers:\n"+body)
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "LEAK-CANARY") {
				t.Fatalf("validation error leaks the DSN: %v", err)
			}
		})
	}
}

// Boundaries of timeout_ms: 100 and 2000 are accepted; 0 means default.
func TestSREPoolers_TimeoutBoundaries(t *testing.T) {
	for _, ms := range []string{"100", "2000", "0"} {
		cfg, err := loadRCAYAML(t, "sre:\n  poolers:\n    - name: pgb-1\n"+
			"      dsn: postgres://x@pgb/pgbouncer\n      timeout_ms: "+ms+"\n")
		if err != nil {
			t.Fatalf("timeout_ms %s: %v", ms, err)
		}
		if ms == "0" && cfg.SRE.Poolers[0].TimeoutMS != 1000 {
			t.Fatalf("timeout_ms 0 = %d, want the 1000 ms default",
				cfg.SRE.Poolers[0].TimeoutMS)
		}
	}
}

// A pooler with no databases fronts every database of this sidecar; a
// listed one only those.
func TestSREPoolers_Fronts(t *testing.T) {
	all := SREPoolerConfig{Name: "a"}
	some := SREPoolerConfig{Name: "b", Databases: []string{"orders"}}
	if !all.Fronts("orders") || !all.Fronts("billing") {
		t.Fatal("a pooler without databases must front every database")
	}
	if !some.Fronts("orders") || some.Fronts("billing") || some.Fronts("") {
		t.Fatal("a pooler with databases fronts only those")
	}
}

func TestSREPoolers_SecretAndDocTags(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SRE.Poolers = []SREPoolerConfig{{}}
	docs := collectDocTags(t, cfg)
	for _, key := range []string{"name", "dsn", "dsn_file", "databases", "pools",
		"timeout_ms"} {
		if strings.TrimSpace(docs["sre.poolers[]."+key]) == "" {
			t.Errorf("sre.poolers[].%s has no doc tag", key)
		}
	}
	f, ok := reflect.TypeOf(SREPoolerConfig{}).FieldByName("DSN")
	if !ok || f.Tag.Get("secret") != "true" {
		t.Fatal("sre.poolers[].dsn must be tagged secret")
	}
}

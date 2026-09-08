package config

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimeConnectionOptionsPreserveTLSWithoutSerialization(t *testing.T) {
	cfg := DatabaseConfig{Host: "db.example", Port: 5432, User: "worker",
		Database: "app", SSLMode: "verify-full"}
	values := url.Values{"sslrootcert": {"private-ca-path"}, "sslcert": {"private-cert-path"},
		"sslkey": {"private-key-path"}, "channel_binding": {"require"}, "dbname": {"wrong"}}
	cfg.SetRuntimeConnectionOptions(values)
	values.Set("sslrootcert", "changed")
	uri, err := url.Parse(cfg.ConnString())
	if err != nil || uri.Query().Get("sslrootcert") != "private-ca-path" ||
		uri.Query().Get("sslcert") != "private-cert-path" ||
		uri.Query().Get("sslkey") != "private-key-path" ||
		uri.Query().Get("channel_binding") != "require" || uri.Query().Get("dbname") != "" {
		t.Fatal("transport options or target identity changed")
	}
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	yamlBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonBytes)+string(yamlBytes), "private-") {
		t.Fatal("runtime connection options leaked into persistent configuration")
	}
	cloned := Clone(&Config{Databases: []DatabaseConfig{cfg}})
	if cloned.Databases[0].ConnString() != cfg.ConnString() {
		t.Fatal("clone lost connection options")
	}
}

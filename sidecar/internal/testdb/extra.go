package testdb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

var extraLabelPattern = regexp.MustCompile(`[^a-z0-9]+`)

// CreateDatabase creates one extra disposable database on the designated
// test server for tests that need several physical databases (fleet
// fan-out). It skips when no live server is configured, and it force-drops
// the database when the test ends. The returned DSN keeps the designated
// server's connection policy and only swaps the database name.
func CreateDatabase(t testing.TB, label string) string {
	t.Helper()
	base := SkipUnlessLive(t)
	name, err := extraDatabaseName(label)
	if err != nil {
		t.Fatalf("name extra test database: %v", err)
	}
	dsn, err := createFixture(base, name)
	if err != nil {
		t.Fatalf("create extra test database: %v", err)
	}
	t.Cleanup(func() {
		if err := dropFixture(base, name); err != nil {
			t.Errorf("drop extra test database: %v", err)
		}
	})
	return dsn
}

func extraDatabaseName(label string) (string, error) {
	clean := extraLabelPattern.ReplaceAllString(strings.ToLower(label), "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		clean = "extra"
	}
	if len(clean) > 24 {
		clean = clean[:24]
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("random suffix: %w", err)
	}
	name := fmt.Sprintf("%sx_%s_%d_%s", fixturePrefix, clean, os.Getpid(),
		hex.EncodeToString(suffix))
	return name, validateFixtureDatabaseName(name)
}

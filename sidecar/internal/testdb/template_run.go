package testdb

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// diag is where the fixture says what it did: os.Stderr as it is when it
// speaks (tests that redirect os.Stderr see it), or a test's writer.
var diag = func() io.Writer { return os.Stderr }

// noTemplateOnce says "no fixture template" once per process.
var noTemplateOnce = newOnce()

func newOnce() *sync.Once { return new(sync.Once) }

// fixtureTemplate is the template this process's fixtures copy.
var fixtureTemplate atomic.Value

// RunBootstrapped is Run for a package whose tests need the sage schema:
// its fixture is a copy of the run's template database, bootstrapped once
// with bootstrap (schema.Bootstrap) under a cluster lock, so packages do
// not all bootstrap at the same moment (ci-15). Tests still call
// schema.Bootstrap; on a copy it only checks. Tests of bootstrap or
// migration behaviour keep Run: their databases must start empty.
func RunBootstrapped(run func() int, packageID string, bootstrap BootstrapFunc) int {
	root, err := moduleRoot()
	if err != nil {
		root = "" // chooseTemplate falls back to template0 and says why
	}
	return runFixtureWith(run, packageID, root, bootstrap)
}

// chooseTemplate returns the template the fixtures copy: template0 without
// a bootstrap, or (said once) without a migration checksum file under
// root; otherwise the run's template, built first if no package has.
func chooseTemplate(baseDSN, root string, bootstrap BootstrapFunc) (string, error) {
	if bootstrap == nil {
		return "template0", nil
	}
	hash, err := templateHash(root)
	if err != nil {
		noTemplateOnce.Do(func() {
			_, _ = fmt.Fprintf(diag(), "testdb: no fixture template (%v); fixtures start "+
				"from template0\n", err)
		})
		return "template0", nil
	}
	name := templatePrefix + hash
	ctx, cancel := context.WithTimeout(context.Background(), templateBuildTimeout)
	defer cancel()
	if err := ensureTemplate(ctx, baseDSN, name, bootstrap); err != nil {
		return "", err
	}
	return name, nil
}

// FixtureTemplate names the database this process's fixtures were copied
// from: "template0", a pgsage_tpl_ template, or "" before Run.
func FixtureTemplate() string {
	name, _ := fixtureTemplate.Load().(string)
	return name
}

func setFixtureTemplate(name string) { fixtureTemplate.Store(name) }

// CreateBootstrappedDatabase is CreateDatabase copying the package's
// fixture template, so the caller's schema.Bootstrap only checks. The
// package's TestMain must call RunBootstrapped.
func CreateBootstrappedDatabase(t testing.TB, label string) string {
	t.Helper()
	base := SkipUnlessLive(t)
	template := FixtureTemplate()
	if template == "" {
		t.Fatal("CreateBootstrappedDatabase needs testdb.RunBootstrapped in TestMain")
	}
	name, err := extraDatabaseName(label)
	if err != nil {
		t.Fatalf("name extra test database: %v", err)
	}
	dsn, err := createFixtureFrom(base, name, "designated", template)
	if err != nil {
		t.Fatalf("create extra test database from %s: %v", template, err)
	}
	t.Cleanup(func() {
		if err := dropFixture(base, name); err != nil {
			t.Errorf("drop extra test database: %v", err)
		}
	})
	return dsn
}

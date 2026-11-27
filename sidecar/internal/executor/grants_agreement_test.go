package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/onboarding"
)

// The startup check (VerifyGrants) and the Grant more guide read the same
// live privileges and must agree: a schema CREATE grant the startup check
// warns about is the one the guide marks missing, with the same SQL, and
// once granted neither asks for it.

// No concurrent access tests: both checks are read-only catalog queries.

func guideSchemaGrant(t *testing.T, pool *pgxpool.Pool, level string) onboarding.Grant {
	t.Helper()
	g, err := onboarding.CheckGrants(context.Background(), pool)
	if err != nil {
		t.Fatalf("onboarding grants: %v", err)
	}
	levels := onboarding.TrustGuide(onboarding.GuideInput{
		Current: onboarding.LevelObservation, Grants: g})
	for _, l := range levels {
		if l.Level != level {
			continue
		}
		for _, gr := range l.Grants {
			if gr.Name == onboarding.GrantSchemaCreate {
				return gr
			}
		}
		return onboarding.Grant{}
	}
	t.Fatalf("no level %q in the guide", level)
	return onboarding.Grant{}
}

func schemaWarnings(pool *pgxpool.Pool, level string) []string {
	var lines []logLine
	VerifyGrants(context.Background(), pool, "ignored", level, captureLog(&lines))
	var out []string
	for _, l := range lines {
		if strings.Contains(l.text, "GRANT CREATE ON SCHEMA") {
			out = append(out, l.text)
		}
	}
	return out
}

func TestVerifyGrantsAgreesWithGrantMore(t *testing.T) {
	dsn := tablesInApp(t)
	pool, role := grantsRole(t, dsn)
	for _, level := range []string{"advisory", "autonomous"} {
		g := guideSchemaGrant(t, pool, level)
		warn := schemaWarnings(pool, level)
		if g.Present == nil || *g.Present || len(warn) != 1 {
			t.Fatalf("%s: guide %+v, startup %q: want both to report it missing", level,
				g, warn)
		}
		fix := strings.TrimSuffix(g.SQL, ";")
		if fix != "GRANT CREATE ON SCHEMA app TO "+role || !strings.HasSuffix(warn[0], fix) {
			t.Fatalf("%s: guide SQL %q and startup fix %q differ", level, g.SQL, warn[0])
		}
	}
	if g := guideSchemaGrant(t, pool, "observation"); g.Name != "" {
		t.Fatalf("observation guide lists %+v; observation runs no DDL", g)
	}
	if w := schemaWarnings(pool, "observation"); len(w) != 0 {
		t.Fatalf("observation startup warns %q; observation runs no DDL", w)
	}
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	grant := "GRANT CREATE ON SCHEMA app TO " + role
	if _, err := admin.Exec(context.Background(), grant); err != nil {
		t.Fatalf("grant: %v", err)
	}
	g := guideSchemaGrant(t, pool, "advisory")
	if g.Present == nil || !*g.Present || len(schemaWarnings(pool, "advisory")) != 0 {
		t.Fatalf("after grant: guide %+v, startup %q: want both satisfied", g,
			schemaWarnings(pool, "advisory"))
	}
}

// guideGrant is the named grant of a level in the live Grant more guide.
func guideGrant(t *testing.T, pool *pgxpool.Pool, level, name string) onboarding.Grant {
	t.Helper()
	g, err := onboarding.CheckGrants(context.Background(), pool)
	if err != nil {
		t.Fatalf("onboarding grants: %v", err)
	}
	for _, l := range onboarding.TrustGuide(onboarding.GuideInput{Grants: g}) {
		if l.Level != level {
			continue
		}
		for _, gr := range l.Grants {
			if gr.Name == name {
				return gr
			}
		}
	}
	t.Fatalf("no %s grant at level %s", name, level)
	return onboarding.Grant{}
}

func startupLines(pool *pgxpool.Pool, level, match string) []string {
	var lines []logLine
	VerifyGrants(context.Background(), pool, "ignored", level, captureLog(&lines))
	var out []string
	for _, l := range lines {
		if strings.Contains(l.text, match) {
			out = append(out, l.text)
		}
	}
	return out
}

// pg_signal_backend: both checks use the shared probe, so a role without it
// is missing in both with the same SQL, and once granted in neither.
func TestVerifyGrantsAgreesOnSignalBackend(t *testing.T) {
	dsn := tablesInApp(t)
	pool, role := grantsRole(t, dsn)
	g := guideGrant(t, pool, "advisory", onboarding.GrantSignalBackend)
	warn := startupLines(pool, "advisory", "pg_signal_backend")
	fix := strings.TrimSuffix(g.SQL, ";")
	if g.Present == nil || *g.Present || len(warn) != 1 ||
		fix != "GRANT pg_signal_backend TO "+role || !strings.HasSuffix(warn[0], fix) {
		t.Fatalf("guide %+v, startup %q: want both missing with one SQL", g, warn)
	}
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(context.Background(), fix); err != nil {
		t.Fatalf("%s: %v", fix, err)
	}
	g = guideGrant(t, pool, "advisory", onboarding.GrantSignalBackend)
	if w := startupLines(pool, "advisory", "pg_signal_backend"); g.Present == nil ||
		!*g.Present || len(w) != 0 {
		t.Fatalf("after grant: guide %+v, startup %q: want both satisfied", g, w)
	}
}

// Sequences: the startup check names the same unreadable schemas and SQL as
// the observation grant, at every level, since reading needs no trust.
func TestVerifyGrantsAgreesOnSequences(t *testing.T) {
	dsn := tablesInApp(t)
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	seqs := "CREATE TABLE app.s (id serial); CREATE SCHEMA other; CREATE SEQUENCE other.q"
	if _, err := admin.Exec(context.Background(), seqs); err != nil {
		t.Fatalf("sequences: %v", err)
	}
	pool, role := grantsRole(t, dsn)
	g := guideGrant(t, pool, "observation", onboarding.GrantSequenceSelect)
	fix := strings.TrimSuffix(g.SQL, ";")
	if g.Present == nil || *g.Present ||
		fix != "GRANT SELECT ON ALL SEQUENCES IN SCHEMA app, other TO "+role {
		t.Fatalf("guide sequence grant = %+v", g)
	}
	for _, level := range []string{"observation", "advisory"} {
		w := startupLines(pool, level, "SEQUENCES")
		if len(w) != 1 || !strings.HasSuffix(w[0], fix) || strings.Contains(w[0], "WARNING") {
			t.Fatalf("%s startup %q, want one note ending in %q", level, w, fix)
		}
	}
	if _, err := admin.Exec(context.Background(), fix); err != nil {
		t.Fatalf("%s: %v", fix, err)
	}
	g = guideGrant(t, pool, "observation", onboarding.GrantSequenceSelect)
	if w := startupLines(pool, "observation", "SEQUENCES"); g.Present == nil ||
		!*g.Present || len(w) != 0 {
		t.Fatalf("after grant: guide %+v, startup %q: want both satisfied", g, w)
	}
}

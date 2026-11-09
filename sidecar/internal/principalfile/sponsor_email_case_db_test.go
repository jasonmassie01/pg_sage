package principalfile

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// apiauth-05: sage.users stores emails in lower case, so a sponsor named
// in any case is the same pg_sage user, and the plan does not keep
// proposing a case-only sponsor change. No concurrent access test: the
// lookups are single read-only statements.

func TestCoreBackendSponsorEmailAnyCase(t *testing.T) {
	pool := coreDB(t, "pf_sponsor_case")
	ctx := context.Background()
	file := strings.ReplaceAll(validFile, "sponsor: alice@example.com",
		"sponsor: Alice@Example.COM")
	if file == validFile {
		t.Fatal("fixture no longer names alice@example.com as a sponsor")
	}
	coreApply(t, pool, mustParse(t, file))
	ci, err := agentguard.NewStore(pool).GetByName(ctx, "ci-bot")
	if err != nil || ci.SponsorUserID == nil {
		t.Fatalf("ci-bot = %+v, %v; want a sponsor", ci, err)
	}
	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM sage.users WHERE id = $1`,
		*ci.SponsorUserID).Scan(&email); err != nil || email != "alice@example.com" {
		t.Fatalf("sponsor = %q (%v), want alice@example.com", email, err)
	}
	cat := NewCoreCatalog(pool, SpecProfiles())
	if !cat.UserExists(" ALICE@example.com") || cat.UserExists("carol@example.com") {
		t.Fatal("UserExists does not match exactly the known users in any case")
	}
}

func TestCoreBackendAmbiguousSponsorRefused(t *testing.T) {
	pool := coreDB(t, "pf_sponsor_dup")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP INDEX sage.users_email_lower;
		INSERT INTO sage.users (email, password, role) VALUES ('Alice@example.com', 'x',
		'viewer')`); err != nil {
		t.Fatalf("seeding a case-variant duplicate: %v", err)
	}
	cat := NewCoreCatalog(pool, SpecProfiles())
	if cat.UserExists("alice@example.com") {
		t.Fatal("an email shared by two accounts counts as one sponsor")
	}
	_, err := NewCoreBackend(pool).sponsorID(ctx, "alice@example.com")
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("ambiguous sponsor err = %v, want a refusal", err)
	}
	if id, err := NewCoreBackend(pool).sponsorID(ctx, "BOB@example.com"); err != nil ||
		id == nil {
		t.Fatalf("bob sponsor = %v, %v", id, err)
	}
}

func TestFieldChangesIgnoreSponsorCase(t *testing.T) {
	want := PrincipalSpec{Name: "p", Sponsor: "Alice@Example.com", Status: "active",
		EnvCeiling: "dev"}
	have := Principal{Name: "p", Sponsor: "alice@example.com", Status: "active",
		EnvCeiling: "dev"}
	if got := fieldChanges(want, have); len(got) != 0 {
		t.Fatalf("case-only sponsor difference = %+v, want no change", got)
	}
	want.Sponsor = "bob@example.com"
	got := fieldChanges(want, have)
	if len(got) != 1 || got[0].Field != "sponsor" || got[0].To != "bob@example.com" {
		t.Fatalf("real sponsor change = %+v", got)
	}
}

//go:build integration

package discovery

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetpol-01b review: once enrolled, a member keeps the tags, creator and
// set it was enrolled under on the real store. The gate reads the creator
// from this row, so a later COMMENT ON DATABASE by whoever owns the
// database must not move the member to another policy set. A candidate
// not yet enrolled still follows its source's latest listing.
func TestPolicyEnrol_EnrolledMemberKeepsItsAttributes(t *testing.T) {
	env := requireLive(t)
	setAgentEnrollment(t, env)
	name := dbName(t, testdb.CreateDatabase(t, "frozen_attrs"))
	commentDatabase(t, env, name, "pg_sage: creator=agent:fixture tier=dev")
	now := time.Now().UTC().Truncate(time.Second)
	r := env.setsRunner(t, &liveMembers{env: env}, &now,
		&instantGrades{clock: func() time.Time { return now }})
	if rep := r.Cycle(env.ctx); len(rep.Errors) != 0 {
		t.Fatalf("first cycle: %+v", rep)
	}
	row := findRow(t, env, name)
	if row.Status != store.DiscoveryEnrolled || row.DatabaseID == nil {
		t.Fatalf("not enrolled: %+v", row)
	}
	commentDatabase(t, env, name, "pg_sage: creator=person:pat tier=prod")
	now = now.Add(2 * time.Minute)
	if rep := r.Cycle(env.ctx); len(rep.Errors) != 0 {
		t.Fatalf("second cycle: %+v", rep)
	}
	row = findRow(t, env, name)
	if row.SetName != "agent-created" || row.Creator != "agent:fixture" ||
		row.Tags["tier"] != "dev" {
		t.Fatalf("enrolled row changed with the comment: set %q creator %q tags %v",
			row.SetName, row.Creator, row.Tags)
	}
	assertResolvesTo(t, env, int64(*row.DatabaseID), "agent-created", "agent:fixture")
}

// A candidate's attributes and set follow the latest listing until it is
// enrolled (the real Upsert, not the fake).
func TestPolicyEnrol_CandidateAttributesFollowTheListing(t *testing.T) {
	env := requireLive(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := []Candidate{{ExternalID: "cand-1", Host: "h", Port: 5432,
		DBName: "freeze_cand_app", Tags: map[string]string{"tier": "dev"}, Creator: "agent:a",
		SetName: "agent-created"}}
	if _, err := env.disc.Upsert(env.ctx, "freeze-src", "team", first, now); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second := []Candidate{{ExternalID: "cand-1", Host: "h", Port: 5432,
		DBName: "freeze_cand_app", Tags: map[string]string{"tier": "prod"}, Creator: "person:pat",
		SetName: "default"}}
	if _, err := env.disc.Upsert(env.ctx, "freeze-src", "team", second,
		now.Add(time.Minute)); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	row := findRow(t, env, "freeze_cand_app")
	if row.Status != store.DiscoveryCandidate || row.SetName != "default" ||
		row.Creator != "person:pat" || row.Tags["tier"] != "prod" {
		t.Fatalf("candidate row = status %q set %q creator %q tags %v", row.Status,
			row.SetName, row.Creator, row.Tags)
	}
}

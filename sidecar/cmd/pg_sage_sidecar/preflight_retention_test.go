package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sequential by design: these probes change the disposable database's global stop flag.
// Unknown/nil dependency checks remain in the existing constructor tests.
//
// The "none" control proves the fixture satisfies every other retention
// precondition (reviewed dry run, standing policy, autonomous trust with
// tier3_moderate, trust ramp and an open window), so each runtime control is
// the only reason the delete is withheld.
func TestPreflightRetentionHonorsRuntimeControls(t *testing.T) {
	controls := []struct{ name, reason string }{
		{"none", ""},
		{"emergency_stop", string(policy.ReasonEmergencyStop)},
		{"observation", string(policy.ReasonObserveOnly)},
		{"manual", string(policy.ReasonObserveOnly)},
		{"disabled", string(policy.ReasonExecutorDisabled)},
	}
	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			p := preflightRuntimePool(t)
			ctx := context.Background()
			table := preflightRetentionTable(t, p)
			supervisorErr := preflightRunRetentionCycles(t, p, table, control.name)
			var remaining, applied int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.retention_run
				WHERE table_name=$1 AND disposition='applied'`, table).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			if control.name == "none" {
				if supervisorErr != nil || remaining != 1 || applied != 1 {
					t.Fatalf("positive control: err=%v remaining=%d want=1 applied=%d want=1",
						supervisorErr, remaining, applied)
				}
				return
			}
			if !errors.Is(supervisorErr, executor.ErrCustodianProposalWithheld) ||
				!strings.Contains(supervisorErr.Error(), control.reason) {
				t.Fatalf("%s: want delete withheld by policy (%s), got %v",
					control.name, control.reason, supervisorErr)
			}
			if remaining != 2 || applied != 0 {
				t.Fatalf("%s violated: remaining=%d want=2, applied=%d want=0",
					control.name, remaining, applied)
			}
		})
	}
}

// preflightRunRetentionCycles wires the executor exactly as startup does
// (standing policy enabled), runs a first schema-guard cycle that records the
// dry run, ages that dry run past its review window, and returns the result
// of the second (apply) cycle.
func preflightRunRetentionCycles(
	t *testing.T, p *pgxpool.Pool, table, control string,
) error {
	t.Helper()
	ctx := context.Background()
	c := config.DefaultConfig()
	c.Trust.Level = "autonomous"
	c.Trust.Tier3Moderate = true
	c.Trust.MaintenanceWindow = "always"
	if control == "observation" {
		c.Trust.Level = "observation"
	}
	e := executor.New(p, c, nil, time.Now().Add(-90*24*time.Hour),
		func(string, string, ...any) {})
	if err := e.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
		t.Fatal(err)
	}
	e.SetExecutionMode("auto")
	if control == "manual" {
		e.SetExecutionMode("manual")
	}
	if control == "disabled" {
		e.SetExecutorEnabled(false)
	}
	if err := executor.SetEmergencyStop(ctx, p, control == "emergency_stop"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executor.SetEmergencyStop(ctx, p, false) })
	supervisor, err := newDatabaseAutonomy(p, c, "preflight", e)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.TriggerSchemaGuard(ctx, "preflight"); err != nil {
		t.Fatalf("dry-run cycle: %v", err)
	}
	tag, err := p.Exec(ctx, `UPDATE sage.retention_run
		SET created_at = created_at - interval '25 hours',
		    cutoff_at = cutoff_at - interval '25 hours'
		WHERE table_name=$1 AND disposition='dry_run'`, table)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age dry run: rows=%d err=%v", tag.RowsAffected(), err)
	}
	return supervisor.TriggerSchemaGuard(ctx, "preflight")
}

func preflightRuntimePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func preflightRetentionTable(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("preflight_retention_%d", time.Now().UnixNano())
	_, err := p.Exec(ctx, "CREATE TABLE "+table+" (id int, created_at timestamptz); "+
		"INSERT INTO "+table+" VALUES (1,'2020-01-01'),(2,'2099-01-01')")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, "DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = p.Exec(ctx, "DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = p.Exec(ctx, "DROP TABLE "+table)
	})
	_, err = p.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name,table_name,append_only,retention_interval,declared_by,evidence_id)
		VALUES ('public',$1,true,interval '30 days','preflight',$1)`, table)
	if err != nil {
		t.Fatal(err)
	}
	preflightActivateUnattended(t, p)
	return table
}

// preflightActivateUnattended makes the unattended profile the active global
// policy through the production propose/ratify path, and restores the prior
// policy afterwards. Bootstrap alone keeps whatever profile an earlier test
// in this package activated (e.g. staffed, whose window is weekdays 01-05).
func preflightActivateUnattended(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	store := policy.NewStore(p)
	prior, err := store.Bootstrap(context.Background(), policy.Scope{}, "unattended", "preflight")
	if err != nil {
		t.Fatal(err)
	}
	if prior.Profile == "unattended" {
		return
	}
	doc, err := policy.MarshalDocument(policy.UnattendedProfile())
	if err != nil {
		t.Fatal(err)
	}
	preflightRatify(t, store, "unattended", doc)
	t.Cleanup(func() { preflightRatify(t, store, prior.Profile, prior.Document) })
}

func preflightRatify(t *testing.T, store *policy.Store, profile string, doc []byte) {
	t.Helper()
	ctx := context.Background()
	current, err := store.Current(ctx, policy.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := store.Propose(ctx, policy.ProposalRequest{
		ExpectedVersion: current.Version, Profile: profile, Document: doc,
		Actor: "preflight",
	})
	if err != nil {
		t.Fatalf("propose %s policy: %v", profile, err)
	}
	if _, err := store.Ratify(ctx, policy.RatifyRequest{
		ProposalID: proposal.ID, ExpectedVersion: current.Version, Actor: "preflight",
	}); err != nil {
		t.Fatalf("ratify %s policy: %v", profile, err)
	}
}

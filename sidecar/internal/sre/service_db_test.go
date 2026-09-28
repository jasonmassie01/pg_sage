package sre

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The read service behind the API, MCP tools and Cases panel: scoped
// reads (CHECK-09), redacted evidence and a redacted export (CHECK-30),
// verified hashes and chain, and tombstones once retention deleted
// evidence.

const secretApp = "postgres://admin:hunter2@db.internal/prod"

// secretRunner scripts a connection investigation whose evidence carries
// a DSN in an application name, a literal and a vector in another one,
// and a failed probe whose error names a password.
func secretRunner() *scriptedRunner {
	vector := "[0.12, -0.5, 0.4, 0.9, 0.1, 0.2, 0.3, 0.33]"
	sample := rows(probes.ConnectionSaturation,
		connRow(secretApp, "idle", 14, 20),
		connRow("WHERE x = 'secret-literal' "+vector, "active", 1, 20))
	failed := probes.Result{ProbeID: probes.LockGraph, Version: "v1",
		Status: probes.StatusError, Reason: "connection_failed",
		Error: "failed to connect: password=hunter2 host=db"}
	return newScriptedRunner().script(probes.ConnectionSaturation, sample, sample).
		script(probes.LockGraph, failed)
}

func secretInvestigation(t *testing.T, ctx context.Context, st *PostgresStore) (*Service,
	Investigation) {
	t.Helper()
	c, _ := testCoordinator(t, ctx, st, secretRunner(), nil)
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "case:conn:secret",
		Kind: TriggerConnections, Subject: "incident s", IdempotencyKey: "incident:s"})
	return NewService("orders", c, st), inv
}

func assertNoSecrets(t *testing.T, what string, raw []byte) {
	t.Helper()
	for _, leak := range []string{"hunter2", "postgres://", "secret-literal", "0.12"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%s leaks %q: %s", what, leak, raw)
		}
	}
}

func TestService_DetailIsRedactedAndVerified(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	svc, inv := secretInvestigation(t, ctx, st)
	d, err := svc.Detail(ctx, inv.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.Database != "orders" || d.Investigation.ID != inv.ID || d.Revisions != 1 ||
		len(d.Hypotheses) == 0 || !d.ChainVerified || !d.EvidenceAvailable ||
		d.EventCount < 4 {
		t.Fatalf("detail = %+v", d)
	}
	for _, e := range d.Evidence {
		if !e.HashVerified || len(e.SHA256) != 64 {
			t.Fatalf("evidence %s hash %q verified=%v", e.ID, e.SHA256, e.HashVerified)
		}
	}
	raw, _ := json.Marshal(d)
	assertNoSecrets(t, "detail", raw)
	if !strings.Contains(string(raw), `"capability_state":"unknown"`) {
		t.Fatalf("the failed probe is not shown: %s", raw)
	}
}

// CHECK-09: another database's service cannot read this investigation or
// its evidence, even with the exact ids.
func TestService_ReadsAreScopedToTheDatabase(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	svc, inv := secretInvestigation(t, ctx, st)
	other, _ := testCoordinator(t, ctx, st, newScriptedRunner(), nil)
	foreign := NewService("billing", other, st)
	if _, err := foreign.Detail(ctx, inv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-database detail = %v, want ErrNotFound", err)
	}
	d, _ := svc.Detail(ctx, inv.ID)
	if _, err := foreign.EvidenceItem(ctx, inv.ID, d.Evidence[0].ID); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("cross-database evidence = %v, want ErrNotFound", err)
	}
	if _, err := svc.EvidenceItem(ctx, inv.ID, NewUUID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown evidence = %v", err)
	}
	ev, err := svc.EvidenceItem(ctx, inv.ID, d.Evidence[0].ID)
	if err != nil || ev.ID != d.Evidence[0].ID {
		t.Fatalf("own evidence = %+v (%v)", ev, err)
	}
	if _, err := svc.Detail(ctx, "not-a-uuid"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed id = %v", err)
	}
	var nilSvc *Service
	if _, err := nilSvc.List(ctx, ListFilter{}); !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("nil service = %v", err)
	}
}

// CHECK-30: the export has a schema version, the diagnosis, hashes and
// the event chain, and no DSN, credential, raw vector or SQL literal.
func TestService_ExportIsRedactedAndComplete(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	svc, inv := secretInvestigation(t, ctx, st)
	doc, err := svc.Export(ctx, inv.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if doc.SchemaVersion != ExportSchemaVersion || doc.Database != "orders" ||
		len(doc.Hypotheses) == 0 || len(doc.Evidence) == 0 || len(doc.Events) < 4 ||
		!doc.ChainVerified || len(doc.Redaction) == 0 || doc.ExportedAt.IsZero() {
		t.Fatalf("export = %+v", doc)
	}
	raw, _ := json.Marshal(doc)
	assertNoSecrets(t, "JSON export", raw)
	md, err := svc.ExportMarkdown(ctx, inv.ID)
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	assertNoSecrets(t, "Markdown export", []byte(md))
	for _, want := range []string{"# Investigation", "Likely explanation",
		"Other explanations", "Ruled out", "Missing evidence", "Evidence",
		ExportSchemaVersion} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
}

// After retention deleted the evidence, the conclusion is still shown but
// marked evidence-unavailable, with the tombstone, never silently whole.
func TestService_PurgedEvidenceIsShownAsUnavailable(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	svc, inv := secretInvestigation(t, ctx, st)
	backdate(t, ctx, pool, inv.ID, 40)
	if _, err := st.Purge(ctx, inv.Scope, testPolicy); err != nil {
		t.Fatalf("purge: %v", err)
	}
	d, err := svc.Detail(ctx, inv.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.EvidenceAvailable || len(d.Evidence) != 0 || len(d.Tombstones) != 1 ||
		len(d.Hypotheses) == 0 || !d.ChainVerified {
		t.Fatalf("detail after purge = %+v", d)
	}
}

func TestService_PinAndEvents(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	svc, inv := secretInvestigation(t, ctx, st)
	pinned, err := svc.SetPinned(ctx, inv.ID, true, "user:7")
	if err != nil || !pinned.Pinned {
		t.Fatalf("pin = %+v (%v)", pinned, err)
	}
	events, verified, err := svc.Events(ctx, inv.ID)
	if err != nil || !verified || events[len(events)-1].Type != EventPinned {
		t.Fatalf("events = %+v verified=%v (%v)", events, verified, err)
	}
	if _, err := svc.SetPinned(ctx, inv.ID, true, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("pin without actor = %v", err)
	}
}

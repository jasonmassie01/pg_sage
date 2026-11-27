package agentguard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-05: every governance change writes one chained sage.auth_audit
// row in the same transaction as the change, and a failed audit write
// fails the change.

type govEvent struct {
	ID     int64
	Event  string
	Actor  int
	Target int
	IP     string
	Detail map[string]any
}

// govEvents are the auth_audit rows naming principal id, oldest first.
func govEvents(t *testing.T, pool *pgxpool.Pool, id string) []govEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, event, actor_user_id,
		target_user_id, source_ip, detail::text FROM sage.auth_audit
		WHERE detail->>'principal_id' = $1 ORDER BY id`, id)
	require.NoError(t, err)
	defer rows.Close()
	out := []govEvent{}
	for rows.Next() {
		var e govEvent
		var detail string
		require.NoError(t, rows.Scan(&e.ID, &e.Event, &e.Actor, &e.Target, &e.IP, &detail))
		require.NoError(t, json.Unmarshal([]byte(detail), &e.Detail))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

// requireChained checks the row is sealed in the auth_audit hash chain.
func requireChained(t *testing.T, pool *pgxpool.Pool, row int64) {
	t.Helper()
	ctx := context.Background()
	links, err := auditchain.LinksForRow(ctx, pool, "auth_audit", row)
	require.NoError(t, err)
	require.Len(t, links, 1)
	rep, err := auditchain.Verify(ctx, pool, auditchain.AuthAudit,
		auditchain.Window{FromSeq: links[0].Seq, ToSeq: links[0].Seq})
	require.NoError(t, err)
	require.True(t, rep.OK(), "auth_audit row %d verifies: %+v", row, rep.Problems)
}

func sub(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	require.True(t, ok, "detail.%s is an object: %v", key, m)
	return v
}

func TestAudit_CreateUpdateWidenRetire(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor := createUser(t, pool, "admin")
	first, second := createUser(t, pool, "operator"), createUser(t, pool, "admin")
	who := Audit{ActorUserID: actor, SourceIP: "203.0.113.9"}
	p, err := s.Create(ctx, CreateRequest{Name: uniqName("audited"), SponsorUserID: &first,
		Profile: "readonly-analyst", EnvCeiling: EnvStage, CreatedBy: "a@example.com",
		Audit: who})
	require.NoError(t, err)
	ev := govEvents(t, pool, p.ID)
	require.Len(t, ev, 1)
	require.Equal(t, auth.AuditAgentCreated, ev[0].Event)
	require.Equal(t, actor, ev[0].Actor)
	require.Equal(t, first, ev[0].Target, "target_user_id is the sponsor")
	require.Equal(t, "203.0.113.9", ev[0].IP)
	after := sub(t, ev[0].Detail, "after")
	require.Equal(t, float64(first), after["sponsor_user_id"])
	require.Equal(t, "readonly-analyst", after["profile"])
	require.Equal(t, "stage", after["env_ceiling"])
	require.Equal(t, p.Name, ev[0].Detail["name"])

	dev := EnvDev
	_, err = s.Update(ctx, p.ID, Patch{SponsorUserID: &second, EnvCeiling: &dev},
		Audit{ActorUserID: actor, SourceIP: "203.0.113.9", Reason: "hand-over"})
	require.NoError(t, err)
	ev = govEvents(t, pool, p.ID)
	require.Len(t, ev, 2)
	require.Equal(t, auth.AuditAgentUpdated, ev[1].Event)
	require.Equal(t, second, ev[1].Target)
	before, after := sub(t, ev[1].Detail, "before"), sub(t, ev[1].Detail, "after")
	require.Equal(t, float64(first), before["sponsor_user_id"])
	require.Equal(t, "stage", before["env_ceiling"])
	require.Equal(t, float64(second), after["sponsor_user_id"])
	require.Equal(t, "dev", after["env_ceiling"])
	require.Equal(t, "hand-over", ev[1].Detail["reason"])

	checkWidenAndRetire(t, s, p.ID, actor, second)
}

// checkWidenAndRetire widens and retires principal id, then checks the
// four events of TestAudit_CreateUpdateWidenRetire.
func checkWidenAndRetire(t *testing.T, s *Store, id string, actor, sponsor int) {
	t.Helper()
	ctx, pool := context.Background(), s.Pool()
	prod := EnvProd
	_, err := s.Update(ctx, id, Patch{EnvCeiling: &prod}, Audit{ActorUserID: actor,
		Event: auth.AuditAgentWidenedSingleOperator, Reason: "reads prod"})
	require.NoError(t, err)
	_, err = s.SetStatus(ctx, id, StatusRetired, Audit{ActorUserID: actor,
		Reason: "replaced"})
	require.NoError(t, err)
	ev := govEvents(t, pool, id)
	require.Len(t, ev, 4)
	require.Equal(t, auth.AuditAgentWidenedSingleOperator, ev[2].Event)
	require.Equal(t, "prod", sub(t, ev[2].Detail, "after")["env_ceiling"])
	require.Equal(t, "dev", sub(t, ev[2].Detail, "before")["env_ceiling"])
	require.Equal(t, "reads prod", ev[2].Detail["reason"])
	require.Equal(t, auth.AuditAgentRetired, ev[3].Event)
	require.Equal(t, "active", ev[3].Detail["before_status"])
	require.Equal(t, "retired", ev[3].Detail["after_status"])
	require.Equal(t, "replaced", ev[3].Detail["reason"])
	require.Equal(t, sponsor, ev[3].Target)
	for _, e := range ev {
		requireChained(t, pool, e.ID)
	}
}

func TestAudit_EmptyPatchWritesNothing(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	p := newPrincipal(t, s, nil)
	got, err := s.Update(context.Background(), p.ID, Patch{}, Audit{})
	require.NoError(t, err)
	require.Equal(t, p.Profile, got.Profile)
	require.Equal(t, p.UpdatedAt, got.UpdatedAt, "an empty patch changes nothing")
	require.Len(t, govEvents(t, pool, p.ID), 1, "only the create event")
}

// refuseAudit makes sage.auth_audit refuse rows whose detail names a
// principal or token called with prefix; dropped at cleanup.
func refuseAudit(t *testing.T, pool *pgxpool.Pool, prefix string) {
	t.Helper()
	ctx := context.Background()
	fn := "sage.refuse_" + strings.ReplaceAll(prefix, "-", "_")
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.detail->>'name' LIKE '%s%%' THEN
			RAISE EXCEPTION 'audit refused by test';
		END IF; RETURN NEW; END $$`, fn, prefix))
	require.NoError(t, err)
	trg := "refuse_" + strings.ReplaceAll(prefix, "-", "_")
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON sage.auth_audit
		FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON sage.auth_audit", trg))
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+fn+"()")
	})
}

func TestAudit_FailedWriteFailsTheChange(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	sponsor := createUser(t, pool, "operator")
	p := newPrincipal(t, s, &sponsor)
	prefix := fmt.Sprintf("auditfail%d", uniqSeq.Add(1))
	_, err := pool.Exec(ctx, "UPDATE sage.guard_principals SET name = $2 WHERE id = $1",
		p.ID, prefix+"-old")
	require.NoError(t, err)
	refuseAudit(t, pool, prefix)

	dev := EnvDev
	_, err = s.Update(ctx, p.ID, Patch{EnvCeiling: &dev}, Audit{ActorUserID: sponsor})
	require.Error(t, err)
	require.Contains(t, err.Error(), "audit")
	_, err = s.SetStatus(ctx, p.ID, StatusRetired, Audit{ActorUserID: sponsor})
	require.Error(t, err)
	got := mustGet(t, s, p.ID)
	require.Equal(t, EnvProd, got.EnvCeiling, "a refused audit leaves the ceiling")
	require.Equal(t, StatusActive, got.Status, "a refused audit leaves the status")

	_, err = s.Create(ctx, CreateRequest{Name: prefix + "-new", SponsorUserID: &sponsor,
		Profile: "readonly-analyst", CreatedBy: "a@example.com"})
	require.Error(t, err)
	_, err = s.GetByName(ctx, prefix+"-new")
	require.ErrorIs(t, err, ErrNotFound, "a refused audit creates no principal")
}

func TestAudit_InvalidAudit(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	id := "agp_abcdefghijklmnopqrst"
	dev := EnvDev
	for name, a := range map[string]Audit{
		"negative actor":  {ActorUserID: -1},
		"control in ip":   {SourceIP: "1.2.3.4\n"},
		"long reason":     {Reason: strings.Repeat("r", maxReasonLen+1)},
		"foreign event":   {Event: auth.AuditUserCreated},
		"retire as patch": {Event: auth.AuditAgentRetired},
	} {
		_, err := s.Update(ctx, id, Patch{EnvCeiling: &dev}, a)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	_, err := s.SetStatus(ctx, id, StatusRetired,
		Audit{Event: auth.AuditAgentWidenedSingleOperator})
	require.ErrorIs(t, err, ErrInvalid, "a status change is not a widening")
	_, err = s.Create(ctx, CreateRequest{Name: "ok-name", Profile: "readonly-analyst",
		CreatedBy: "a", Audit: Audit{Event: auth.AuditAgentUpdated}})
	require.ErrorIs(t, err, ErrInvalid, "create records agent_created only")
	// A valid audit on a store without a database is unavailable, not invalid.
	_, err = s.Update(ctx, id, Patch{EnvCeiling: &dev}, Audit{ActorUserID: 1})
	require.ErrorIs(t, err, ErrUnavailable)
}

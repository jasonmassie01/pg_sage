package agentguard

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-06 review: the PATCH route judges widening on an unlocked
// read, so UpdateNarrowing judges the patch again under the row lock. A
// change committed in between (here a confirmed ceiling raise) must not
// let a patch that was narrowing on the stale read apply unconfirmed.
func TestUpdateNarrowing_RejudgesUnderTheLock(t *testing.T) {
	ctx := context.Background()
	f := newChangeFixture(t)
	p, err := f.s.Create(ctx, CreateRequest{Name: uniqName("narrow"),
		SponsorUserID: &f.sponsor, Profile: "coding-agent", EnvCeiling: EnvStage,
		CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	readonly := "readonly-analyst"
	toReadonly := Patch{Profile: &readonly}
	stale := mustGet(t, f.s, p.ID)
	require.False(t, toReadonly.Widens(stale), "at stage the move narrows")

	_, err = f.s.Update(ctx, p.ID, Patch{EnvCeiling: envPtr(EnvProd)}, Audit{
		ActorUserID: f.b, Event: auth.AuditAgentWidenedSingleOperator, Reason: "prod"})
	require.NoError(t, err)
	before := len(govEvents(t, f.pool, p.ID))

	_, err = f.s.UpdateNarrowing(ctx, p.ID, toReadonly, Audit{ActorUserID: f.a})
	require.True(t, errors.Is(err, ErrChangeWidens), "got %v", err)
	now := mustGet(t, f.s, p.ID)
	require.Equal(t, "coding-agent", now.Profile, "a refused patch applies nothing")
	require.Equal(t, EnvProd, now.EnvCeiling)
	require.Len(t, govEvents(t, f.pool, p.ID), before, "a refused patch records nothing")

	got, err := f.s.UpdateNarrowing(ctx, p.ID, Patch{EnvCeiling: envPtr(EnvDev)},
		Audit{ActorUserID: f.a})
	require.NoError(t, err)
	require.Equal(t, EnvDev, got.EnvCeiling, "a real narrowing still applies at once")
	require.Equal(t, "coding-agent", got.Profile)
	ev := govEvents(t, f.pool, p.ID)
	require.Len(t, ev, before+1)
	require.Equal(t, auth.AuditAgentUpdated, ev[len(ev)-1].Event)
}

func TestUpdateNarrowing_InvalidAndUnknown(t *testing.T) {
	ctx := context.Background()
	f := newChangeFixture(t)
	bad := "codng-agent"
	_, err := f.s.UpdateNarrowing(ctx, f.p.ID, Patch{Profile: &bad}, Audit{})
	require.True(t, errors.Is(err, ErrInvalid), "got %v", err)
	_, err = f.s.UpdateNarrowing(ctx, "agp_aaaaaaaaaaaaaaaaaaaa",
		Patch{EnvCeiling: envPtr(EnvDev)}, Audit{})
	require.True(t, errors.Is(err, ErrNotFound), "got %v", err)
	require.Equal(t, EnvStage, mustGet(t, f.s, f.p.ID).EnvCeiling)
}

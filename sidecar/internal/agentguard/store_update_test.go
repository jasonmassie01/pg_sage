package agentguard

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-06: a profile change widens only when the new profile grants
// more than the old one (classes, effective ceiling, direct lane), and an
// unknown profile is refused before anything is stored.
// No concurrent access tests here: Widens and the profile table are pure
// functions over values; the store's concurrency is in changes_db_test.go.

func profilePtr(s string) *string { return &s }
func envPtr(e Env) *Env           { return &e }

func TestPatchWidens_ProfileBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		from    Principal
		patch   Patch
		widens  bool
		because string
	}{
		{"coding to readonly at stage", Principal{Profile: "coding-agent", EnvCeiling: EnvStage},
			Patch{Profile: profilePtr("readonly-analyst")}, false, "read only, same stage cap"},
		{"coding to readonly at prod", Principal{Profile: "coding-agent", EnvCeiling: EnvProd},
			Patch{Profile: profilePtr("readonly-analyst")}, true,
			"coding-agent capped reads at stage; readonly-analyst reads prod"},
		{"coding@prod to readonly@stage", Principal{Profile: "coding-agent",
			EnvCeiling: EnvProd}, Patch{Profile: profilePtr("readonly-analyst"),
			EnvCeiling: envPtr(EnvStage)}, false, "the lowered ceiling keeps reads at stage"},
		{"readonly to app-writer", Principal{Profile: "readonly-analyst", EnvCeiling: EnvDev},
			Patch{Profile: profilePtr("app-writer")}, true, "write classes and direct lane"},
		{"app-writer to readonly", Principal{Profile: "app-writer", EnvCeiling: EnvProd},
			Patch{Profile: profilePtr("readonly-analyst")}, false, "a subset"},
		{"readonly to legacy", Principal{Profile: "readonly-analyst", EnvCeiling: EnvProd},
			Patch{Profile: profilePtr("legacy")}, false, "the same grant"},
		{"legacy to readonly", Principal{Profile: "legacy", EnvCeiling: EnvProd},
			Patch{Profile: profilePtr("readonly-analyst")}, false, "the same grant"},
		{"readonly to coding", Principal{Profile: "readonly-analyst", EnvCeiling: EnvDev},
			Patch{Profile: profilePtr("coding-agent")}, true, "DDL and delete classes"},
		{"from an unknown profile", Principal{Profile: "old-typo", EnvCeiling: EnvDev},
			Patch{Profile: profilePtr("readonly-analyst")}, true,
			"an unknown profile allowed nothing"},
		{"to an unknown profile", Principal{Profile: "readonly-analyst", EnvCeiling: EnvDev},
			Patch{Profile: profilePtr("codng-agent")}, true, "unknown fails closed"},
		{"same ceiling", Principal{Profile: "app-writer", EnvCeiling: EnvStage},
			Patch{EnvCeiling: envPtr(EnvStage)}, false, "no change"},
		{"stage to prod", Principal{Profile: "app-writer", EnvCeiling: EnvStage},
			Patch{EnvCeiling: envPtr(EnvProd)}, true, "a higher ceiling"},
		{"stage to prod under a stage profile", Principal{Profile: "coding-agent",
			EnvCeiling: EnvStage}, Patch{EnvCeiling: envPtr(EnvProd)}, true,
			"a raised stored ceiling always widens"},
		{"prod to stage", Principal{Profile: "app-writer", EnvCeiling: EnvProd},
			Patch{EnvCeiling: envPtr(EnvStage)}, false, "narrowing"},
		{"same profile", Principal{Profile: "coding-agent", EnvCeiling: EnvDev},
			Patch{Profile: profilePtr("coding-agent")}, false, "no change"},
		{"empty patch", Principal{Profile: "coding-agent", EnvCeiling: EnvDev},
			Patch{}, false, "no change"},
	}
	for _, c := range cases {
		require.Equal(t, c.widens, c.patch.Widens(c.from), "%s: %s", c.name, c.because)
	}
}

func TestGrantWidens_DirectLaneAndClasses(t *testing.T) {
	read := ProfileGrant{Classes: []string{"read"}, EnvCeiling: EnvProd}
	readDirect := ProfileGrant{Classes: []string{"read"}, EnvCeiling: EnvProd,
		DirectLane: true}
	none := ProfileGrant{EnvCeiling: EnvProd}
	require.True(t, grantWidens(read, EnvProd, readDirect, EnvProd), "gaining the direct lane")
	require.False(t, grantWidens(readDirect, EnvProd, read, EnvProd), "losing the direct lane")
	require.True(t, grantWidens(none, EnvProd, read, EnvProd), "gaining a class")
	require.False(t, grantWidens(read, EnvProd, none, EnvProd), "losing every class")
	require.False(t, grantWidens(read, EnvProd, read, EnvProd), "identical")
	require.True(t, grantWidens(read, EnvDev, read, EnvStage), "a higher effective ceiling")
	require.False(t, grantWidens(read, Env("bogus"), read, Env("bogus")),
		"an invalid ceiling counts as branch on both sides")
}

func TestProfileCatalog_IsTheBuiltInTable(t *testing.T) {
	names := ProfileNames()
	require.Equal(t, []string{"app-writer", "coding-agent", "legacy", "readonly-analyst"},
		names)
	require.True(t, sort.StringsAreSorted(names))
	for _, n := range names {
		require.True(t, KnownProfile(n), n)
	}
	for _, bad := range []string{"", "codng-agent", "Readonly-analyst", "readonly-analyst "} {
		require.False(t, KnownProfile(bad), "%q", bad)
	}
	cat := ProfileCatalog()
	require.Len(t, cat, 4)
	require.Equal(t, "coding-agent", cat[1].Name)
	require.Equal(t, EnvStage, cat[1].EnvCeiling)
	require.Contains(t, cat[1].Classes, "ddl_locking")
	require.False(t, cat[1].DirectLane)
	require.True(t, cat[0].DirectLane, "app-writer takes the direct lane")
	// The catalog is a copy: changing it changes nothing shipped.
	cat[0].Classes[0] = "ddl_destructive"
	require.Equal(t, "read", ProfileCatalog()[0].Classes[0])
}

func TestUnknownProfile_RefusedBeforeStorage(t *testing.T) {
	s := NewStore(nil) // a storage call would be ErrUnavailable, not ErrInvalid
	ctx := context.Background()
	_, err := s.Create(ctx, CreateRequest{Name: "ok-bot", Profile: "codng-agent",
		CreatedBy: "a"})
	require.ErrorIs(t, err, ErrInvalid)
	require.Contains(t, err.Error(), "readonly-analyst", "the error names the profiles")
	_, err = s.Update(ctx, "agp_abcdefghijklmnopqrst",
		Patch{Profile: profilePtr("codng-agent")}, Audit{ActorUserID: 1})
	require.ErrorIs(t, err, ErrInvalid)
	require.Contains(t, err.Error(), "coding-agent")
	_, err = s.Update(ctx, "agp_abcdefghijklmnopqrst",
		Patch{Profile: profilePtr("coding-agent")}, Audit{ActorUserID: 1})
	require.ErrorIs(t, err, ErrUnavailable, "a known profile reaches storage")
}

func validChange() ChangeRequest {
	return ChangeRequest{PrincipalID: "agp_abcdefghijklmnopqrst",
		Patch: Patch{EnvCeiling: envPtr(EnvProd)}, Actor: "a@example.com",
		TTL: 15 * time.Minute, Audit: Audit{ActorUserID: 1}}
}

func TestChangeRequest_InvalidNeverTouchesStorage(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	bad := map[string]func(*ChangeRequest){
		"empty patch":     func(r *ChangeRequest) { r.Patch = Patch{} },
		"no admin":        func(r *ChangeRequest) { r.Audit.ActorUserID = 0 },
		"no actor":        func(r *ChangeRequest) { r.Actor = "" },
		"zero ttl":        func(r *ChangeRequest) { r.TTL = 0 },
		"unknown profile": func(r *ChangeRequest) { r.Patch.Profile = profilePtr("x-y") },
		"bad ceiling":     func(r *ChangeRequest) { r.Patch.EnvCeiling = envPtr("qa") },
		"long reason": func(r *ChangeRequest) {
			r.Audit.Reason = strings.Repeat("r", maxReasonLen+1)
		},
	}
	for name, mutate := range bad {
		req := validChange()
		mutate(&req)
		_, err := s.RequestChange(ctx, req)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	req := validChange()
	req.PrincipalID = "nope"
	_, err := s.RequestChange(ctx, req)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.RequestChange(ctx, validChange())
	require.ErrorIs(t, err, ErrUnavailable, "a valid request reaches storage")
}

func TestChangeConfirm_InvalidNeverTouchesStorage(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	ok := ChangeConfirm{PrincipalID: "agp_abcdefghijklmnopqrst", ChangeID: 3,
		Patch: Patch{EnvCeiling: envPtr(EnvProd)}, Actor: "b@example.com",
		Audit: Audit{ActorUserID: 2}}
	zero := ok
	zero.ChangeID = 0
	_, err := s.ConfirmChange(ctx, zero)
	require.ErrorIs(t, err, ErrChangeNotFound)
	noAdmin := ok
	noAdmin.Audit.ActorUserID = 0
	_, err = s.ConfirmChange(ctx, noAdmin)
	require.ErrorIs(t, err, ErrInvalid)
	empty := ok
	empty.Patch = Patch{}
	_, err = s.ConfirmChange(ctx, empty)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = s.ConfirmChange(ctx, ok)
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = s.PendingChanges(ctx, "agp_abcdefghijklmnopqrst")
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = s.PendingChanges(ctx, "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPatchDoc_RoundTripAndEquality(t *testing.T) {
	two := 2
	p := Patch{SponsorUserID: &two, Profile: profilePtr("app-writer"),
		EnvCeiling: envPtr(EnvProd)}
	back := p.doc().patch()
	require.True(t, p.equal(back), "%+v", back)
	require.False(t, p.equal(Patch{Profile: profilePtr("app-writer")}), "missing fields")
	require.False(t, Patch{EnvCeiling: envPtr(EnvProd)}.equal(
		Patch{EnvCeiling: envPtr(EnvStage)}), "another ceiling")
	require.True(t, Patch{}.equal(Patch{}))
	three := 3
	require.False(t, Patch{SponsorUserID: &two}.equal(Patch{SponsorUserID: &three}))
}

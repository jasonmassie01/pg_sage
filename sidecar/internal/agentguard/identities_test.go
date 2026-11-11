package agentguard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-03 without a database: the shape of an identity is checked
// before any storage call, and a store with no pool is ErrUnavailable.
// No concurrent access here: validation is pure; concurrency is in
// identities_db_test.go.

const someID = "agp_aaaaaaaaaaaaaaaaaaaa"

func TestCheckIdentity(t *testing.T) {
	cases := []struct {
		name, issuer, subject string
		ok                    bool
	}{
		{"valid", testIssuer, "agent-client", true},
		{"subject at the limit", testIssuer, strings.Repeat("s", MaxSubjectBytes), true},
		{"subject one over", testIssuer, strings.Repeat("s", MaxSubjectBytes+1), false},
		// 128 two-byte runes are 256 bytes: the limit is bytes, not runes.
		{"multibyte subject over", testIssuer, strings.Repeat("é", 128), false},
		{"empty subject", testIssuer, "", false},
		{"control character", testIssuer, "agent\n1", false},
		{"invalid utf-8", testIssuer, "agent\xff", false},
		{"empty issuer", "", "agent", false},
		{"issuer too long", "https://" + strings.Repeat("a", maxIssuerBytes), "agent", false},
		{"issuer control character", "https://idp\t", "agent", false},
	}
	for _, tc := range cases {
		err := checkIdentity(tc.issuer, tc.subject)
		if tc.ok {
			require.NoError(t, err, tc.name)
			continue
		}
		require.True(t, errors.Is(err, ErrInvalid), "%s: %v", tc.name, err)
	}
}

func TestIdentities_NoPoolIsUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, s := range []*Store{nil, NewStore(nil)} {
		_, err := s.BindIdentity(ctx, someID, BindRequest{Issuer: testIssuer,
			Subject: "agent", CreatedBy: "admin@example.com"})
		require.True(t, errors.Is(err, ErrUnavailable), "bind: %v", err)
		_, err = s.ListIdentities(ctx, someID)
		require.True(t, errors.Is(err, ErrUnavailable), "list: %v", err)
		_, err = s.UnbindIdentity(ctx, someID, testIssuer, "agent", Audit{})
		require.True(t, errors.Is(err, ErrUnavailable), "unbind: %v", err)
	}
}

func TestIdentities_InvalidBeforeStorage(t *testing.T) {
	ctx := context.Background()
	s := NewStore(nil)
	_, err := s.BindIdentity(ctx, someID, BindRequest{Issuer: testIssuer, Subject: "",
		CreatedBy: "admin@example.com"})
	require.True(t, errors.Is(err, ErrInvalid), "empty subject: %v", err)
	_, err = s.BindIdentity(ctx, someID, BindRequest{Issuer: testIssuer, Subject: "a",
		CreatedBy: ""})
	require.True(t, errors.Is(err, ErrInvalid), "no created_by: %v", err)
	_, err = s.BindIdentity(ctx, someID, BindRequest{Issuer: testIssuer, Subject: "a",
		CreatedBy: "admin@example.com", Audit: Audit{ActorUserID: -1}})
	require.True(t, errors.Is(err, ErrInvalid), "bad audit: %v", err)
	_, err = s.UnbindIdentity(ctx, someID, testIssuer, "", Audit{})
	require.True(t, errors.Is(err, ErrInvalid), "unbind empty subject: %v", err)
}

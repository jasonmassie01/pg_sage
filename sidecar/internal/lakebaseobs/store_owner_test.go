package lakebaseobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// No concurrent access or state transition tests here: these are input checks
// that return before any shared state or database transaction is touched.

const (
	ownerDeployment = "5a4d6cd4-9fbe-49ed-a33f-c6b707462101"
	ownerDatabase   = "5a4d6cd4-9fbe-49ed-a33f-c6b707462102"
	nilUUID         = "00000000-0000-0000-0000-000000000000"
)

func TestValidateOwnerRejectsEmptyNilAndMalformedIDs(t *testing.T) {
	cases := []struct {
		name, deployment, database string
		want                       error
	}{
		{"valid", ownerDeployment, ownerDatabase, nil},
		{"empty deployment", "", ownerDatabase, ErrScope},
		{"empty database", ownerDeployment, "", ErrScope},
		{"both empty", "", "", ErrScope},
		{"nil deployment", nilUUID, ownerDatabase, ErrScope},
		{"nil database", ownerDeployment, nilUUID, ErrScope},
		{"malformed deployment", "not-a-uuid", ownerDatabase, ErrScope},
		{"malformed database", ownerDeployment, "5a4d6cd4-9fbe", ErrScope},
		{"sql text database", ownerDeployment, "x'; DROP TABLE sage.x;--", ErrScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOwner(tc.deployment, tc.database)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("validateOwner(%q, %q) = %v, want nil",
						tc.deployment, tc.database, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("validateOwner(%q, %q) = %v, want %v",
					tc.deployment, tc.database, err, tc.want)
			}
		})
	}
}

// A Store without a pool proves the checks run before any transaction: a
// check that reached the pool would panic on the nil pointer.
func TestCleanupOwnerRejectsBadInputBeforeTouchingThePool(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	now := time.Now().UTC()
	good := strings.Repeat("a", 64)
	cases := []struct {
		name      string
		dep, db   string
		allowed   []string
		retention time.Duration
	}{
		{"empty deployment", "", ownerDatabase, []string{good}, SourceRetention},
		{"empty database", ownerDeployment, "", []string{good}, SourceRetention},
		{"zero retention", ownerDeployment, ownerDatabase, []string{good}, 0},
		{"negative retention", ownerDeployment, ownerDatabase, nil, -time.Second},
		{"retention past source", ownerDeployment, ownerDatabase, nil,
			SourceRetention + time.Nanosecond},
		{"short scope", ownerDeployment, ownerDatabase, []string{strings.Repeat("a", 63)},
			SourceRetention},
		{"long scope", ownerDeployment, ownerDatabase, []string{strings.Repeat("a", 65)},
			SourceRetention},
		{"non-hex scope", ownerDeployment, ownerDatabase, []string{good, strings.Repeat("z", 64)},
			SourceRetention},
		{"empty scope", ownerDeployment, ownerDatabase, []string{""}, SourceRetention},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.CleanupOwnerScopes(ctx, tc.dep, tc.db, tc.allowed, true, tc.retention, now)
			if !errors.Is(err, ErrScope) {
				t.Fatalf("CleanupOwnerScopes = %v, want ErrScope", err)
			}
		})
	}
}

func TestOwnerPolicyEntryPointsRejectBadBindingsBeforeThePool(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, binding := range []string{strings.Repeat("a", 63), strings.Repeat("g", 64), "x"} {
		err := s.CleanupOwnerPolicy(ctx, ownerDeployment, ownerDatabase, binding, true,
			SourceRetention, now)
		if !errors.Is(err, ErrScope) {
			t.Fatalf("CleanupOwnerPolicy(%q) = %v, want ErrScope", binding, err)
		}
		scopes, err := s.PolicyOwnerScopes(ctx, ownerDeployment, ownerDatabase, binding)
		if !errors.Is(err, ErrScope) || scopes != nil {
			t.Fatalf("PolicyOwnerScopes(%q) = %v, %v, want nil, ErrScope", binding, scopes, err)
		}
	}
	// An empty binding is a valid disable for cleanup but never a lookup key.
	scopes, err := s.PolicyOwnerScopes(ctx, ownerDeployment, ownerDatabase, "")
	if !errors.Is(err, ErrScope) || scopes != nil {
		t.Fatalf("PolicyOwnerScopes(\"\") = %v, %v, want nil, ErrScope", scopes, err)
	}
	if _, err = s.PolicyOwnerScopes(ctx, "", ownerDatabase, strings.Repeat("a", 64)); !errors.Is(
		err, ErrScope) {
		t.Fatalf("PolicyOwnerScopes with empty deployment = %v, want ErrScope", err)
	}
	if _, err = s.OwnerScopes(ctx, ownerDeployment, ""); !errors.Is(err, ErrScope) {
		t.Fatalf("OwnerScopes with empty database = %v, want ErrScope", err)
	}
}

func TestEnforceRetentionRejectsBadScopeAndRetentionBeforeThePool(t *testing.T) {
	s := &Store{}
	good := strings.Repeat("a", 64)
	cases := []struct {
		name      string
		scope     string
		retention time.Duration
	}{
		{"empty scope", "", SourceRetention},
		{"short scope", good[:63], SourceRetention},
		{"non-hex scope", strings.Repeat("q", 64), SourceRetention},
		{"zero retention", good, 0},
		{"retention past source", good, SourceRetention + time.Nanosecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.EnforceRetention(context.Background(), tc.scope, tc.retention); !errors.Is(
				err, ErrScope) {
				t.Fatalf("EnforceRetention = %v, want ErrScope", err)
			}
		})
	}
}

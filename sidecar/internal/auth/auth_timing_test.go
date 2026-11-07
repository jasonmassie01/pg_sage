//go:build integration

package auth

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// apiauth-06: at the production bcrypt cost, a missing email, an SSO-only
// account and a wrong password take the same time to refuse.

const timingRounds = 9

func TestAuthenticate_RefusalTimingDoesNotRevealAccounts(t *testing.T) {
	if got := hashCost.Load(); got != BcryptCost {
		t.Fatalf("hash cost = %d, want production cost %d for a timing test",
			got, BcryptCost)
	}
	pool := setupTestPool(t)
	ctx := context.Background()
	const pwEmail, ssoEmail = "timing-pw@fixture.invalid", "timing-sso@fixture.invalid"
	_, _ = pool.Exec(ctx, `DELETE FROM sage.users WHERE email = ANY($1)`,
		[]string{pwEmail, ssoEmail})
	if _, err := CreateUser(ctx, pool, pwEmail, "timing-right-password", RoleViewer); err != nil {
		t.Fatalf("create password user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.users (email, password, role,
		oauth_provider) VALUES ($1, NULL, 'viewer', 'oidc')`, ssoEmail); err != nil {
		t.Fatalf("create SSO-only user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.users WHERE email = ANY($1)`, []string{pwEmail, ssoEmail})
	})
	cases := []string{"missing-timing@fixture.invalid", ssoEmail, pwEmail}
	samples := make([][]time.Duration, len(cases))
	// Interleave the cases so machine load drifts across all of them alike.
	for round := 0; round < timingRounds; round++ {
		for i, email := range cases {
			start := time.Now()
			u, err := Authenticate(ctx, pool, email, "timing-wrong-password")
			samples[i] = append(samples[i], time.Since(start))
			if u != nil || !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("%s: user=%v err=%v, want ErrInvalidCredentials", email, u, err)
			}
		}
	}
	medians := make([]time.Duration, len(cases))
	for i := range cases {
		medians[i] = median(samples[i])
	}
	lo, hi := medians[0], medians[0]
	for _, m := range medians[1:] {
		lo, hi = min(lo, m), max(hi, m)
	}
	t.Logf("median refusal times: missing=%v sso=%v wrong-password=%v",
		medians[0], medians[1], medians[2])
	if float64(hi) > 1.2*float64(lo) {
		t.Fatalf("median refusal times differ by more than 20%%: missing=%v "+
			"sso=%v wrong-password=%v", medians[0], medians[1], medians[2])
	}
}

func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

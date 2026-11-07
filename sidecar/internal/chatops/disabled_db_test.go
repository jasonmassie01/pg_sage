package chatops

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-01: a deactivated account cannot approve from chat. Its mapping
// stays, so re-enabling the account restores chat approvals.
func TestResolveRefusesDisabledAccount(t *testing.T) {
	s, pool, ctx := liveStore(t)
	user := newUser(t, ctx, pool, auth.RoleOperator)
	admin := newUser(t, ctx, pool, auth.RoleAdmin)
	if _, err := s.Link(ctx, Identity{Provider: ProviderSlack, TeamID: "TD",
		ExternalUserID: "UOFF", UserID: user, CreatedBy: admin}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := auth.SetUserDisabled(ctx, pool, user, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.Resolve(ctx, ProviderSlack, "TD", "UOFF"); !errors.Is(err, ErrUnmapped) {
		t.Fatalf("resolve disabled account: err = %v, want ErrUnmapped", err)
	}
	if _, err := auth.SetUserDisabled(ctx, pool, user, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	u, err := s.Resolve(ctx, ProviderSlack, "TD", "UOFF")
	if err != nil || u.ID != user || u.Role != auth.RoleOperator {
		t.Fatalf("resolve re-enabled account = (%+v, %v)", u, err)
	}
}

package api

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// earned-07: an admin re-approves a self-initiated level capped by a
// PostgreSQL major upgrade (POST /api/v1/sre/autonomy/revalidate); other
// roles get 403, and a pair that is not capped is a conflict.

const revalidatePath = "/api/v1/sre/autonomy/revalidate?database=orders"

// registerWithMajor re-registers orders with a limiter that reads major.
func (f *autonomyAPIFixture) registerWithMajor(major *int) {
	f.t.Helper()
	src := earned.MajorSource(func(context.Context) (int, error) { return *major, nil })
	f.ledger.WithServerMajor(src)
	f.reg.Register("orders", earned.RegistryEntry{Service: f.ledger,
		Limiter: f.ledger.Limiter(earned.Binding{Database: "orders", ServerMajor: src})})
}

func TestAutonomyAPI_RevalidateRoles(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	body := `{"family":"hygiene","class":"vacuum","note":"checked"}`
	for _, u := range []struct {
		name string
		code int
	}{{"viewer", 403}, {"operator", 403}} {
		user := testViewerUser()
		if u.name == "operator" {
			user = testOperatorUser()
		}
		if code, out := autonomyCall(t, f.router(user), "POST", revalidatePath,
			body); code != u.code {
			t.Errorf("%s revalidate = %d %v, want %d", u.name, code, out, u.code)
		}
	}
	if code, _ := autonomyCall(t, f.router(nil), "POST", revalidatePath, body); code != 401 {
		t.Errorf("anonymous revalidate = %d, want 401", code)
	}
}

func TestAutonomyAPI_RevalidateClearsTheMajorCap(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	major := 16
	f.registerWithMajor(&major)
	ramp := policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
		TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true,
		RampStart: f.clock.Now().Add(-60 * 24 * time.Hour)}
	if _, err := f.ledger.SeedGrandfathered(context.Background(), "orders", ramp); err != nil {
		t.Fatal(err)
	}
	admin := f.router(testAdminUser())
	body := `{"family":"hygiene","class":"vacuum","note":"checked the planner"}`
	if code, out := autonomyCall(t, admin, "POST", revalidatePath, body); code != 409 ||
		out["code"] != "not_capped" {
		t.Fatalf("revalidate on the same major = %d %v, want 409 not_capped", code, out)
	}
	major = 17
	code, out := autonomyCall(t, admin, "POST", revalidatePath, body)
	if code != 200 {
		t.Fatalf("revalidate = %d %v", code, out)
	}
	st, ok := out["state"].(map[string]any)
	if !ok || st["server_major"] != float64(17) || st["level"] != "L3" {
		t.Fatalf("revalidated state = %v", out)
	}
	evs, err := f.ledger.History(context.Background(), earned.EventFilter{
		Family: earned.FamilyHygiene, Class: earned.ClassVacuum, Database: "orders"})
	if err != nil || len(evs) == 0 || evs[0].Type != earned.EventCapCleared ||
		evs[0].Actor != autonomyActor(testAdminUser()) {
		t.Fatalf("history = %+v, %v", evs, err)
	}
	for path, b := range map[string]string{
		revalidatePath:          `{"family":"hygiene","class":"vacuum","bogus":1}`,
		revalidatePath + "&x=1": `{"family":"wraparound_runway","class":"freeze"}`,
	} {
		if code, out := autonomyCall(t, admin, "POST", path, b); code != 400 ||
			out["code"] != "invalid_request" {
			t.Errorf("%s %s = %d %v, want 400", path, b, code, out)
		}
	}
}

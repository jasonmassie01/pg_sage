package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// No concurrent-access tests: the handler holds no state; the posture
// function it calls is the runtime's.

func postureMux(fn func(context.Context) (SecretsPosture, error)) *http.ServeMux {
	mux := http.NewServeMux()
	registerSecurityPostureRoutes(mux, fn)
	return mux
}

func TestSecurityPosture_AdminGetsPosture(t *testing.T) {
	mux := postureMux(func(context.Context) (SecretsPosture, error) {
		return SecretsPosture{SecretsAtRest: "plaintext", KeySource: "none",
			PlaintextRows: 3}, nil
	})
	code, body := doJSON(t, mux, testAdminUser(), http.MethodGet,
		"/api/v1/security/posture", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["secrets_at_rest"] != "plaintext" || body["key_source"] != "none" ||
		body["plaintext_rows"] != float64(3) {
		t.Fatalf("body = %v", body)
	}
}

func TestSecurityPosture_NonAdminRefused(t *testing.T) {
	called := false
	mux := postureMux(func(context.Context) (SecretsPosture, error) {
		called = true
		return SecretsPosture{SecretsAtRest: "sealed"}, nil
	})
	for _, user := range []*auth.User{testOperatorUser(), nil} {
		code, _ := doJSON(t, mux, user, http.MethodGet, "/api/v1/security/posture", nil)
		if code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Errorf("user %v: status = %d, want 401/403", user, code)
		}
	}
	if called {
		t.Fatal("posture was computed for a non-admin")
	}
}

func TestSecurityPosture_ErrorIsServerErrorWithoutDetail(t *testing.T) {
	mux := postureMux(func(context.Context) (SecretsPosture, error) {
		return SecretsPosture{}, errors.New("dial tcp 10.0.0.5: secret-host")
	})
	code, body := doJSON(t, mux, testAdminUser(), http.MethodGet,
		"/api/v1/security/posture", nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if msg, _ := body["error"].(string); msg == "" || strings.Contains(msg, "secret-host") {
		t.Fatalf("error body = %v, want a generic message", body)
	}
}

func TestSecurityPosture_NilFuncRegistersNothing(t *testing.T) {
	mux := postureMux(nil)
	code, _ := doJSON(t, mux, testAdminUser(), http.MethodGet, "/api/v1/security/posture", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 without a posture source", code)
	}
}

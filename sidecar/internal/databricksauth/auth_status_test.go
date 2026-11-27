package databricksauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const statusWorkspace = "https://workspace.cloud.databricks.com"

// fakeWorkspace replaces the SDK workspace for Authorize. authErr is what the
// SDK's Authenticate returns; configErr makes the workspace itself fail.
type fakeWorkspace struct {
	mu        sync.Mutex
	configErr error
	authErr   error
}

func (f *fakeWorkspace) set(configErr, authErr error) {
	f.mu.Lock()
	f.configErr, f.authErr = configErr, authErr
	f.mu.Unlock()
}

func (f *fakeWorkspace) resolve() (string, func(*http.Request) error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return "", nil, f.configErr
	}
	authErr := f.authErr
	return statusWorkspace, func(r *http.Request) error {
		if authErr != nil {
			return authErr
		}
		r.Header.Set("Authorization", "Bearer secret-gateway-token")
		return nil
	}, nil
}

// useFakeWorkspace swaps in f and a clock that advances one second per call,
// so every recorded outcome is strictly newer than the previous one.
func useFakeWorkspace(t *testing.T, f *fakeWorkspace) {
	t.Helper()
	oldAuth, oldNow := workspaceAuth, authNow
	var mu sync.Mutex
	clock := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	workspaceAuth = f.resolve
	authNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Second)
		return clock
	}
	workspaceMetadata.Store(nil)
	t.Cleanup(func() {
		workspaceAuth, authNow = oldAuth, oldNow
		workspaceMetadata.Store(nil)
	})
}

func gatewayRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, target, nil)
}

const gatewayURL = statusWorkspace + "/ai-gateway/mlflow/v1/chat/completions"

// State transitions: success, then failure, then success again.
func TestAuthorizeRecordsSuccessThenFailureThenRecovery(t *testing.T) {
	f := &fakeWorkspace{}
	useFakeWorkspace(t, f)
	if m := CurrentAuthMetadata(); m.AuthenticationObserved || m.LastAuthSuccessAt != nil ||
		m.LastAuthFailureAt != nil || m.LastFailureClass != "" {
		t.Fatalf("before any request metadata = %+v; want no outcome", m)
	}
	if err := Authorize(gatewayRequest(t, gatewayURL)); err != nil {
		t.Fatalf("first Authorize = %v", err)
	}
	first := CurrentAuthMetadata()
	if !first.AuthenticationObserved || first.LastAuthSuccessAt == nil ||
		first.LastAuthFailureAt != nil {
		t.Fatalf("after success metadata = %+v; want observed with a success time", first)
	}
	f.set(nil, errors.New("invalid_client: secret rotated"))
	err := Authorize(gatewayRequest(t, gatewayURL))
	if err == nil || !strings.Contains(err.Error(), "authenticate Databricks request") {
		t.Fatalf("Authorize after rotation = %v; want the authenticate error", err)
	}
	failed := CurrentAuthMetadata()
	if failed.AuthenticationObserved {
		t.Fatalf("after failure metadata = %+v; want authentication_observed false", failed)
	}
	if failed.LastAuthFailureAt == nil ||
		!failed.LastAuthFailureAt.After(*failed.LastAuthSuccessAt) {
		t.Fatalf("failure time %v not newer than success %v",
			failed.LastAuthFailureAt, failed.LastAuthSuccessAt)
	}
	if failed.LastFailureClass != FailureAuthenticate {
		t.Fatalf("failure class = %q; want %q", failed.LastFailureClass, FailureAuthenticate)
	}
	if !failed.LastAuthSuccessAt.Equal(*first.LastAuthSuccessAt) {
		t.Fatal("a failure overwrote the last success time")
	}
	f.set(nil, nil)
	if err := Authorize(gatewayRequest(t, gatewayURL)); err != nil {
		t.Fatalf("recovery Authorize = %v", err)
	}
	recovered := CurrentAuthMetadata()
	if !recovered.AuthenticationObserved ||
		!recovered.LastAuthSuccessAt.After(*recovered.LastAuthFailureAt) {
		t.Fatalf("after recovery metadata = %+v; want observed again", recovered)
	}
	if recovered.LastFailureClass != FailureAuthenticate ||
		!recovered.LastAuthFailureAt.Equal(*failed.LastAuthFailureAt) {
		t.Fatalf("recovery lost the last failure record: %+v", recovered)
	}
}

// Error propagation and classes: each failure path names its class only.
func TestAuthorizeClassifiesEachFailurePathWithoutErrorText(t *testing.T) {
	for _, tc := range []struct {
		name, target, class string
		configErr, authErr  error
	}{
		{"config", gatewayURL, FailureConfig, errors.New("secret-config-detail"), nil},
		{"origin", "https://other.cloud.databricks.com/ai-gateway/mlflow/v1",
			FailureOrigin, nil, nil},
		{"plain http", "http://workspace.cloud.databricks.com/ai-gateway/mlflow/v1",
			FailureOrigin, nil, nil},
		{"authenticate", gatewayURL, FailureAuthenticate, nil,
			errors.New("secret-authenticate-detail")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeWorkspace{configErr: tc.configErr, authErr: tc.authErr}
			useFakeWorkspace(t, f)
			if err := Authorize(gatewayRequest(t, tc.target)); err == nil {
				t.Fatal("Authorize succeeded; want an error")
			}
			m := CurrentAuthMetadata()
			if m.LastFailureClass != tc.class || m.LastAuthFailureAt == nil ||
				m.AuthenticationObserved || m.LastAuthSuccessAt != nil {
				t.Fatalf("metadata = %+v; want class %q and no success", m, tc.class)
			}
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "secret-") {
				t.Fatalf("metadata JSON carries error text: %s", body)
			}
			for _, key := range []string{`"last_auth_failure_at"`, `"last_failure_class"`} {
				if !strings.Contains(string(body), key) {
					t.Fatalf("metadata JSON lacks %s: %s", key, body)
				}
			}
		})
	}
}

// Nil/empty: a nil request URL is an origin failure, not a panic.
func TestAuthorizeNilURLIsAnOriginFailure(t *testing.T) {
	useFakeWorkspace(t, &fakeWorkspace{})
	r := gatewayRequest(t, gatewayURL)
	r.URL = nil
	if err := Authorize(r); err == nil {
		t.Fatal("Authorize with a nil URL succeeded")
	}
	if m := CurrentAuthMetadata(); m.LastFailureClass != FailureOrigin {
		t.Fatalf("failure class = %q; want origin", m.LastFailureClass)
	}
}

// The success token stays on the request and out of the metadata.
func TestAuthorizeSuccessNeverStoresTheToken(t *testing.T) {
	useFakeWorkspace(t, &fakeWorkspace{})
	r := gatewayRequest(t, gatewayURL)
	if err := Authorize(r); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer secret-gateway-token" {
		t.Fatal("fake SDK did not authenticate the request")
	}
	body, err := json.Marshal(CurrentAuthMetadata())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret-") || !strings.Contains(string(body),
		`"last_auth_success_at"`) {
		t.Fatalf("metadata JSON = %s; want a success time and no token", body)
	}
}

// Concurrent access: parallel successes and failures never lose either
// outcome, and the observed flag always matches the two times.
func TestAuthorizeConcurrentOutcomesStayConsistent(t *testing.T) {
	useFakeWorkspace(t, &fakeWorkspace{})
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := gatewayURL
			if i%2 == 1 {
				target = "https://other.example/ai-gateway"
			}
			_ = Authorize(gatewayRequest(t, target))
			m := CurrentAuthMetadata()
			want := m.LastAuthSuccessAt != nil && (m.LastAuthFailureAt == nil ||
				m.LastAuthSuccessAt.After(*m.LastAuthFailureAt))
			if m.AuthenticationObserved != want {
				t.Errorf("observed=%v disagrees with times %+v", m.AuthenticationObserved, m)
			}
		}(i)
	}
	wg.Wait()
	m := CurrentAuthMetadata()
	if m.LastAuthSuccessAt == nil || m.LastAuthFailureAt == nil {
		t.Fatalf("final metadata = %+v; want both outcomes recorded", m)
	}
}

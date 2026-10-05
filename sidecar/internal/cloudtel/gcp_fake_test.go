package cloudtel

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCP stands in for the OAuth token endpoint, the metadata server,
// Cloud Monitoring (timeSeries.list) and the Cloud SQL Admin API.

type gcpSeries struct {
	metric, databaseID string
	labels             map[string]string
	points             []fakePoint
	int64Value         bool
	intervalSeconds    int // DELTA metrics: end - start
}

type fakeGCP struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	now time.Time
	key *rsa.PrivateKey

	instances map[string]map[string]any // name -> instance JSON
	series    []gcpSeries
	fail      map[string]fakeFailure // "token", "metadata", "monitoring", "sqladmin"
	dateSkew  time.Duration

	calls        map[string]int
	tokenForms   []url.Values
	bearers      []string
	filters      []string
	metadataUp   bool
	issuedTokens int
}

func newFakeGCP(t *testing.T, now time.Time) *fakeGCP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGCP{t: t, now: now, key: key, instances: map[string]map[string]any{},
		fail: map[string]fakeFailure{}, calls: map[string]int{}, metadataUp: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGCP) called(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeGCP) failed(w http.ResponseWriter, key string) bool {
	f.mu.Lock()
	f.calls[key]++
	fail, ok := f.fail[key]
	f.mu.Unlock()
	if !ok {
		return false
	}
	w.WriteHeader(fail.status)
	if fail.body != "" {
		_, _ = io.WriteString(w, fail.body)
		return true
	}
	if key == "token" {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fail.code,
			"error_description": fail.message})
		return true
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": fail.status, "message": fail.message, "status": fail.code}})
	return true
}

func (f *fakeGCP) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Date", f.now.Add(f.dateSkew).UTC().Format(http.TimeFormat))
	switch {
	case r.URL.Path == "/token":
		f.serveToken(w, r)
	case strings.HasPrefix(r.URL.Path, "/computeMetadata/"):
		f.serveMetadata(w, r)
	case strings.HasPrefix(r.URL.Path, "/v3/projects/"):
		f.recordBearer(r)
		f.serveMonitoring(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/projects/"):
		f.recordBearer(r)
		f.serveSQLAdmin(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGCP) recordBearer(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bearers = append(f.bearers, r.Header.Get("Authorization"))
}

func (f *fakeGCP) serveToken(w http.ResponseWriter, r *http.Request) {
	if f.failed(w, "token") {
		return
	}
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.tokenForms = append(f.tokenForms, form)
	f.issuedTokens++
	n := f.issuedTokens
	f.mu.Unlock()
	if form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		if err := f.verifyJWT(form.Get("assertion")); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant",
				"error_description": err.Error()})
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.token-" +
		strconv.Itoa(n), "expires_in": 3600, "token_type": "Bearer"})
}

// verifyJWT checks the service-account assertion: RS256 over the fake's
// key, issuer, audience, scope and a one-hour lifetime issued "now".
func (f *fakeGCP) verifyJWT(assertion string) error {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return fmt.Errorf("assertion is not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("bad signature")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss, Aud, Scope string
		Iat, Exp        int64
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return fmt.Errorf("claims")
	}
	switch {
	case claims.Iss != "sage@proj-1.iam.gserviceaccount.com":
		return fmt.Errorf("iss %q", claims.Iss)
	case claims.Aud != f.srv.URL+"/token":
		return fmt.Errorf("aud %q", claims.Aud)
	case !strings.Contains(claims.Scope, "https://www.googleapis.com/auth/cloud-platform"):
		return fmt.Errorf("scope %q", claims.Scope)
	case claims.Exp-claims.Iat != 3600 || claims.Iat != f.now.Unix():
		return fmt.Errorf("Invalid JWT: Token must be a short-lived token (60 minutes) " +
			"and in a reasonable timeframe. Check your iat and exp values")
	}
	return nil
}

func (f *fakeGCP) serveMetadata(w http.ResponseWriter, r *http.Request) {
	if !f.metadataUp || r.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	if f.failed(w, "metadata") {
		return
	}
	switch r.URL.Path {
	case "/computeMetadata/v1/project/project-id":
		_, _ = io.WriteString(w, "proj-meta")
	case "/computeMetadata/v1/instance/service-accounts/default/token":
		f.mu.Lock()
		f.issuedTokens++
		n := f.issuedTokens
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.meta-" +
			strconv.Itoa(n), "expires_in": 1800, "token_type": "Bearer"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGCP) serveSQLAdmin(w http.ResponseWriter, r *http.Request) {
	if f.failed(w, "sqladmin") {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/projects/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(parts) == 2 && parts[1] == "instances" {
		items := []map[string]any{}
		for _, inst := range f.instances {
			items = append(items, inst)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		return
	}
	if len(parts) != 3 || parts[1] != "instances" {
		http.NotFound(w, r)
		return
	}
	inst, ok := f.instances[parts[2]]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": 404, "message": "instance does not exist", "status": "NOT_FOUND"}})
		return
	}
	_ = json.NewEncoder(w).Encode(inst)
}

func (f *fakeGCP) serveMonitoring(w http.ResponseWriter, r *http.Request) {
	if f.failed(w, "monitoring") {
		return
	}
	filter := r.URL.Query().Get("filter")
	// Cloud Monitoring answers a filter that matches several metric types
	// with 400 INVALID_ARGUMENT (found against a live Cloud SQL instance).
	if strings.Count(filter, cloudSQLMetric) != 1 || strings.Contains(filter, "metric.type = one_of") {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": 400, "status": "INVALID_ARGUMENT", "message": "The provided filter " +
				"matches more than one metric."}})
		return
	}
	f.mu.Lock()
	f.filters = append(f.filters, filter)
	series := append([]gcpSeries(nil), f.series...)
	f.mu.Unlock()
	out := []map[string]any{}
	for _, s := range series {
		if !strings.Contains(filter, `"`+s.metric+`"`) ||
			!strings.Contains(filter, `"`+s.databaseID+`"`) {
			continue
		}
		out = append(out, s.render())
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"timeSeries": out})
}

func (s gcpSeries) render() map[string]any {
	points := []map[string]any{}
	for _, p := range s.points {
		value := map[string]any{"doubleValue": p.value}
		if s.int64Value {
			value = map[string]any{"int64Value": strconv.FormatInt(int64(p.value), 10)}
		}
		interval := map[string]any{"endTime": p.at.UTC().Format(time.RFC3339Nano)}
		if s.intervalSeconds > 0 {
			interval["startTime"] = p.at.Add(-time.Duration(s.intervalSeconds) *
				time.Second).UTC().Format(time.RFC3339Nano)
		}
		points = append(points, map[string]any{"interval": interval, "value": value})
	}
	labels := map[string]string{}
	for k, v := range s.labels {
		labels[k] = v
	}
	return map[string]any{
		"metric": map[string]any{"type": s.metric, "labels": labels},
		"resource": map[string]any{"type": "cloudsql_database",
			"labels": map[string]string{"database_id": s.databaseID}},
		"points": points,
	}
}

// serviceAccountFile writes a service-account key file whose token_uri is
// the fake's token endpoint.
func (f *fakeGCP) serviceAccountFile(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(f.key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return writeJSON(t, "sa.json", map[string]string{"type": "service_account",
		"project_id": "proj-1", "private_key_id": "kid-1", "private_key": pemKey,
		"client_email": "sage@proj-1.iam.gserviceaccount.com",
		"token_uri":    f.srv.URL + "/token"})
}

func writeJSON(t *testing.T, name string, v any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// authOptions isolates a test from the machine's own credentials.
func (f *fakeGCP) authOptions() GCPAuthOptions {
	return GCPAuthOptions{CredentialsFile: "", WellKnownFile: "-",
		MetadataHost: "-", TokenURL: f.srv.URL + "/token",
		HTTPClient: f.srv.Client(), Now: func() time.Time { return f.now }}
}

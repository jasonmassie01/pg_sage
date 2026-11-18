package specialist

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// incident.io webhooks (mcp-02): Svix-style signatures (webhook-id,
// webhook-timestamp, webhook-signature) over "id.timestamp.body"; an
// incident_created event whose Database field value is mapped opens an
// investigation. A bearer token is still required.

var ioKey = []byte("incident-io-signing-key-32-bytes")

var ioSecret = "whsec_" + base64.StdEncoding.EncodeToString(ioKey)

// ioSign is the signature computed independently of the adapter.
func ioSign(key []byte, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func ioEvent(eventType, incidentID, field, value string) []byte {
	key := eventType
	return []byte(fmt.Sprintf(`{"event_type":%s,%s:{"id":%s,"name":"Checkout DB latency",
		"reference":"INC-42","permalink":"https://app.incident.io/acme/incidents/42",
		"created_at":"2026-10-04T11:52:00Z","custom_field_entries":[{"custom_field":
		{"id":"01CF","name":%s,"field_type":"single_select"},"values":[{"value_option":
		{"id":"01OPT","value":%s}}]}]}}`, jsonString(eventType), jsonString(key),
		jsonString(incidentID), jsonString(field), jsonString(value)))
}

const ioCreated = "public_incident.incident_created_v2"

func ioHandler(t *testing.T) (http.Handler, *harness) {
	t.Helper()
	h := newHarness(t, DefaultLimits())
	routes, err := ParseAlertRoutes("specialist.incidentio.routes",
		[]string{"Orders=orders:lock_blocking", "billing-db=billing"}, true)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(h.svc, testAuth, HandlerOptions{
		IncidentIO: &IncidentIOAdapter{Secret: ioSecret, Field: "Database", Routes: routes},
		Now:        h.clock.Now})
	return handler, h
}

func ioHeaders(h *harness, body []byte) map[string]string {
	ts := strconv.FormatInt(h.clock.Now().Unix(), 10)
	return map[string]string{"webhook-id": "msg_2abc", "webhook-timestamp": ts,
		"webhook-signature": ioSign(ioKey, "msg_2abc", ts, body)}
}

const ioPath = base + "/adapters/incidentio"

func TestIncidentIO_CreatedIncidentOpensAnInvestigation(t *testing.T) {
	h, hs := ioHandler(t)
	body := ioEvent(ioCreated, "01HABCDEFGHJKMNPQRSTVWXYZ0", "Database", "orders")
	w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
	var open OpenResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &open) != nil || !open.Created ||
		open.Database != "orders" {
		t.Fatalf("incident.io %d %s", w.Code, w.Body.String())
	}
	if tr := hs.orders.started[0]; tr.Kind != sre.TriggerLock {
		t.Fatalf("the route's family picks the trigger: %+v", tr)
	}
	rec := hs.store.all()[0]
	if rec.ExternalRef == nil || rec.ExternalRef.System != "incident_io" ||
		rec.ExternalRef.ID != "01HABCDEFGHJKMNPQRSTVWXYZ0" ||
		rec.ExternalRef.URL != "https://app.incident.io/acme/incidents/42" ||
		rec.Symptom == nil || rec.Symptom.Summary != "Checkout DB latency" ||
		rec.Window == nil || rec.Transport != "http" || rec.Outbound != OutboundNone {
		t.Fatalf("record %+v", rec)
	}
}

func TestIncidentIO_FieldValueMatchesWithoutCase(t *testing.T) {
	h, hs := ioHandler(t)
	// The propose token may only name orders, so the case test uses its route.
	body := ioEvent(ioCreated, "01HB", "database", "ORDERS")
	w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
	if w.Code != 201 || len(hs.orders.started) != 1 || len(hs.other.started) != 0 {
		t.Fatalf("orders route: %d %s", w.Code, w.Body.String())
	}
}

func TestIncidentIO_BadSignatureIs401(t *testing.T) {
	h, hs := ioHandler(t)
	body := ioEvent(ioCreated, "01HABC", "Database", "orders")
	good := ioHeaders(hs, body)
	stale := strconv.FormatInt(hs.clock.Now().Add(-6*time.Minute).Unix(), 10)
	ts := good["webhook-timestamp"]
	tampered := append(append([]byte(nil), body...), ' ')
	cases := map[string]map[string]string{
		"missing": {},
		"wrong key": {"webhook-signature": ioSign([]byte("other"), "msg_2abc", ts,
			body)},
		"other id": {"webhook-id": "msg_other"},
		"tampered body": {"webhook-signature": ioSign(ioKey, "msg_2abc", ts,
			tampered)},
		"stale": {"webhook-timestamp": stale,
			"webhook-signature": ioSign(ioKey, "msg_2abc", stale, body)},
		"wrong version": {"webhook-signature": strings.Replace(
			good["webhook-signature"], "v1,", "v2,", 1)},
	}
	for name, change := range cases {
		headers := map[string]string{}
		if name != "missing" {
			for k, v := range good {
				headers[k] = v
			}
		}
		for k, v := range change {
			headers[k] = v
		}
		w := postAdapter(h, ioPath, "propose-token", body, headers)
		if w.Code != 401 || errorBody(t, w).Code != "signature_invalid" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(hs.orders.started) != 0 {
		t.Fatalf("an unsigned event opened an investigation: %+v", hs.orders.started)
	}
	// The bearer token is still required with a valid signature.
	if w := postAdapter(h, ioPath, "", body, good); w.Code != 401 ||
		errorBody(t, w).Code != "unauthenticated" {
		t.Fatalf("no token: %d %s", w.Code, w.Body.String())
	}
}

func TestVerifyIncidentIOSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"event_type":"x"}`)
	good := ioSign(ioKey, "msg_1", ts, body)
	if err := VerifyIncidentIOSignature(ioSecret, body, "msg_1", ts, good, now); err != nil {
		t.Fatal(err)
	}
	// Rotation: several space-separated signatures, one of them current.
	rotated := "v1," + base64.StdEncoding.EncodeToString([]byte("old")) + " " + good
	if err := VerifyIncidentIOSignature(ioSecret, body, "msg_1", ts, rotated, now); err != nil {
		t.Fatalf("one matching signature suffices: %v", err)
	}
	// Boundary: the 5 minute tolerance is inclusive on both sides.
	for _, skew := range []time.Duration{5 * time.Minute, -5 * time.Minute} {
		at := strconv.FormatInt(now.Add(skew).Unix(), 10)
		sig := ioSign(ioKey, "msg_1", at, body)
		if err := VerifyIncidentIOSignature(ioSecret, body, "msg_1", at, sig, now); err != nil {
			t.Errorf("skew %s: %v", skew, err)
		}
	}
	for name, args := range map[string][4]string{
		"empty secret": {"", "msg_1", ts, good}, "no id": {ioSecret, "", ts, good},
		"bad timestamp":     {ioSecret, "msg_1", "soon", good},
		"secret not base64": {"whsec_%%%", "msg_1", ts, good},
		"past tolerance":    {ioSecret, "msg_1", strconv.FormatInt(now.Unix()-301, 10), good},
	} {
		if err := VerifyIncidentIOSignature(args[0], body, args[1], args[2], args[3],
			now); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: want ErrSignature, got %v", name, err)
		}
	}
}

func TestIncidentIO_UnmappedIncidentIsUnknownDatabase(t *testing.T) {
	h, hs := ioHandler(t)
	for name, body := range map[string][]byte{
		"unmapped value": ioEvent(ioCreated, "01HC", "Database", "inventory"),
		"no field":       ioEvent(ioCreated, "01HD", "Team", "orders"),
		"empty value":    ioEvent(ioCreated, "01HE", "Database", ""),
	} {
		w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
		var ig WebhookIgnored
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &ig) != nil || !ig.Ignored ||
			ig.Code != CodeUnknownDatabase ||
			!strings.Contains(ig.Reason, "specialist.incidentio.routes") ||
			!strings.Contains(ig.Reason, `"Database"`) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(hs.orders.started)+len(hs.other.started) != 0 {
		t.Fatal("an unmapped incident opened an investigation")
	}
}

func TestIncidentIO_OtherEventsAreIgnored(t *testing.T) {
	h, hs := ioHandler(t)
	body := ioEvent("public_incident.incident_updated_v2", "01HF", "Database", "orders")
	w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
	var ig WebhookIgnored
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &ig) != nil || !ig.Ignored ||
		ig.Code != "" || !strings.Contains(ig.Reason, "incident_updated_v2") {
		t.Fatalf("update event: %d %s", w.Code, w.Body.String())
	}
}

func TestIncidentIO_InvalidPayloads(t *testing.T) {
	h, hs := ioHandler(t)
	for name, body := range map[string][]byte{
		"not json":      []byte(`{"event_type":`),
		"no event body": []byte(`{"event_type":"` + ioCreated + `"}`),
		"bad id":        ioEvent(ioCreated, "01H; DROP", "Database", "orders"),
		"empty id":      ioEvent(ioCreated, "", "Database", "orders"),
	} {
		w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
		if w.Code != 400 || errorBody(t, w).Code != "invalid_request" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestIncidentIO_DisabledIs404(t *testing.T) {
	hs := newHarness(t, DefaultLimits())
	h := NewHandler(hs.svc, testAuth, HandlerOptions{Now: hs.clock.Now})
	body := ioEvent(ioCreated, "01HG", "Database", "orders")
	if w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body)); w.Code != 404 ||
		errorBody(t, w).Code != "disabled" {
		t.Fatalf("unconfigured adapter: %d %s", w.Code, w.Body.String())
	}
}

// padBody pads a JSON body with trailing spaces to exactly n bytes.
func padBody(t *testing.T, body []byte, n int) []byte {
	t.Helper()
	if len(body) > n {
		t.Fatalf("body %d is over %d", len(body), n)
	}
	return append(body, []byte(strings.Repeat(" ", n-len(body)))...)
}

func TestIncidentIO_BodyAtTheLimitIsAcceptedOneMoreByteIsNot(t *testing.T) {
	h, hs := ioHandler(t)
	body := padBody(t, ioEvent(ioCreated, "01HH", "Database", "orders"), maxAdapterBody)
	if w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body)); w.Code != 201 {
		t.Fatalf("at the limit: %d %s", w.Code, w.Body.String())
	}
	over := append(append([]byte(nil), body...), ' ')
	w := postAdapter(h, ioPath, "propose-token", over, ioHeaders(hs, over))
	if w.Code != 413 || errorBody(t, w).Code != "payload_too_large" {
		t.Fatalf("one byte over: %d %s", w.Code, w.Body.String())
	}
}

func TestIncidentIO_RedeliveriesOfOneIncidentShareOneInvestigation(t *testing.T) {
	h, hs := ioHandler(t)
	body := ioEvent(ioCreated, "01HJ", "Database", "orders")
	headers := ioHeaders(hs, body)
	codes, ids := concurrentPosts(h, ioPath, body, headers, 10)
	assertOneInvestigation(t, codes, ids)
	if n := len(hs.orders.invs); n != 1 {
		t.Fatalf("%d investigations for one incident", n)
	}
}

// concurrentPosts sends n identical adapter requests at once.
func concurrentPosts(h http.Handler, path string, body []byte, headers map[string]string,
	n int) ([]int, []string) {
	codes, ids := make([]int, n), make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := postAdapter(h, path, "propose-token", body, headers)
			var open OpenResponse
			_ = json.Unmarshal(w.Body.Bytes(), &open)
			codes[i], ids[i] = w.Code, open.Investigation.ID
		}(i)
	}
	wg.Wait()
	return codes, ids
}

func assertOneInvestigation(t *testing.T, codes []int, ids []string) {
	t.Helper()
	created := 0
	for i, code := range codes {
		if code == 201 {
			created++
		} else if code != 200 {
			t.Fatalf("delivery %d: %d", i, code)
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("deliveries landed on different investigations: %v", ids)
		}
	}
	if created != 1 {
		t.Fatalf("%d deliveries created an investigation, want 1: %v", created, codes)
	}
}

func TestParseAlertRoutes(t *testing.T) {
	routes, err := ParseAlertRoutes("specialist.x.routes", []string{"Orders=orders",
		" /SUBSCRIPTIONS/S/RG = billing:wal_retention "}, true)
	if err != nil || routes["orders"] != (PagerDutyRoute{Database: "orders"}) ||
		routes["/subscriptions/s/rg"] != (PagerDutyRoute{Database: "billing",
			Family: "wal_retention"}) {
		t.Fatalf("routes %+v %v", routes, err)
	}
	if routes, err := ParseAlertRoutes("specialist.x.routes", nil, true); err != nil ||
		len(routes) != 0 {
		t.Fatalf("empty: %+v %v", routes, err)
	}
	for _, bad := range [][]string{{"orders"}, {"=orders"}, {"k="}, {"k=orders:operator"},
		{"K=orders", "k=billing"}, {"k=bad;name"}, {"k\x01=orders"},
		{strings.Repeat("k", 513) + "=orders"}} {
		_, err := ParseAlertRoutes("specialist.x.routes", bad, true)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "specialist.x.routes") {
			t.Errorf("%q: want ErrInvalid naming the setting, got %v", bad, err)
		}
	}
}

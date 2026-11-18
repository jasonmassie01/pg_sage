package specialist

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Azure Monitor alerts (mcp-02) in the common alert schema: action groups
// cannot sign a webhook, so the operator's shared key travels in the
// X-Sage-Azure-Key header or the key query parameter (constant-time
// compared). A Fired alert on a mapped resource opens an investigation.

const azSecret = "azure-shared-key"

const azResource = "/subscriptions/1111/resourceGroups/rg-db/providers/" +
	"Microsoft.DBforPostgreSQL/flexibleServers/orders-pg"

const azAlertGUID = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"

func azAlert(alertID, condition, target string) []byte {
	return []byte(fmt.Sprintf(`{"schemaId":"azureMonitorCommonAlertSchema","data":{
		"essentials":{"alertId":%s,"alertRule":"orders-pg connections > 90%%",
		"severity":"Sev2","signalType":"Metric","monitorCondition":%s,
		"monitoringService":"Platform","alertTargetIDs":[%s],
		"configurationItems":["orders-pg"],"originAlertId":"x",
		"firedDateTime":"2026-10-04T11:52:00.000Z","description":"connections high",
		"essentialsVersion":"1.0","alertContextVersion":"1.0"},"alertContext":{}}}`,
		jsonString(alertID), jsonString(condition), jsonString(target)))
}

func azAlertID(guid string) string {
	return "/subscriptions/1111/providers/Microsoft.AlertsManagement/alerts/" + guid
}

func azHandler(t *testing.T) (http.Handler, *harness) {
	t.Helper()
	h := newHarness(t, DefaultLimits())
	routes, err := ParseAlertRoutes("specialist.azure_monitor.resources",
		[]string{azResource + "=orders:connection_pressure"}, true)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(h.svc, testAuth, HandlerOptions{
		AzureMonitor: &AzureMonitorAdapter{Secret: azSecret, Resources: routes},
		Now:          h.clock.Now})
	return handler, h
}

const azPath = base + "/adapters/azure-monitor"

var azKey = map[string]string{"X-Sage-Azure-Key": azSecret}

func TestAzureMonitor_FiredAlertOpensAnInvestigation(t *testing.T) {
	h, hs := azHandler(t)
	// Azure lower-cases resource ids inconsistently: matching ignores case.
	body := azAlert(azAlertID(azAlertGUID), "Fired", strings.ToLower(azResource))
	w := postAdapter(h, azPath, "propose-token", body, azKey)
	var open OpenResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &open) != nil || !open.Created {
		t.Fatalf("azure %d %s", w.Code, w.Body.String())
	}
	if tr := hs.orders.started[0]; tr.Kind != triggerFor("connection_pressure") ||
		tr.Kind == sre.TriggerLock {
		t.Fatalf("the route's family picks the trigger: %+v", tr)
	}
	rec := hs.store.all()[0]
	if rec.ExternalRef == nil || rec.ExternalRef.System != "azure_monitor" ||
		rec.ExternalRef.ID != azAlertGUID || rec.Symptom == nil ||
		rec.Symptom.Summary != "orders-pg connections > 90%" || rec.Window == nil ||
		rec.Transport != "http" {
		t.Fatalf("record %+v", rec)
	}
}

func TestAzureMonitor_KeyInTheQueryIsAccepted(t *testing.T) {
	h, _ := azHandler(t)
	body := azAlert(azAlertID(azAlertGUID), "Fired", azResource)
	if w := postAdapter(h, azPath+"?key="+azSecret, "propose-token", body, nil); w.Code != 201 {
		t.Fatalf("query key: %d %s", w.Code, w.Body.String())
	}
}

func TestAzureMonitor_WrongOrMissingKeyIs401(t *testing.T) {
	h, hs := azHandler(t)
	body := azAlert(azAlertID(azAlertGUID), "Fired", azResource)
	for name, path := range map[string]string{"missing": azPath,
		"wrong query": azPath + "?key=nope", "prefix": azPath + "?key=azure-shared"} {
		w := postAdapter(h, path, "propose-token", body, nil)
		if w.Code != 401 || errorBody(t, w).Code != "signature_invalid" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	w := postAdapter(h, azPath, "propose-token", body,
		map[string]string{"X-Sage-Azure-Key": azSecret + "x"})
	if w.Code != 401 {
		t.Fatalf("wrong header: %d", w.Code)
	}
	if w := postAdapter(h, azPath, "", body, azKey); w.Code != 401 ||
		errorBody(t, w).Code != "unauthenticated" {
		t.Fatalf("no token: %d %s", w.Code, w.Body.String())
	}
	if len(hs.orders.started) != 0 {
		t.Fatal("an unauthenticated alert opened an investigation")
	}
}

func TestVerifyAzureMonitorKey(t *testing.T) {
	if err := VerifyAzureMonitorKey(azSecret, azSecret, ""); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := VerifyAzureMonitorKey(azSecret, "", azSecret); err != nil {
		t.Fatalf("query: %v", err)
	}
	for name, args := range map[string][3]string{"empty secret": {"", "", ""},
		"empty secret, any key": {"", "k", "k"}, "none": {azSecret, "", ""},
		"wrong header beats right query": {azSecret, "bad", azSecret}} {
		if err := VerifyAzureMonitorKey(args[0], args[1], args[2]); !errors.Is(err,
			ErrSignature) {
			t.Errorf("%s: want ErrSignature, got %v", name, err)
		}
	}
}

func TestAzureMonitor_MissingAlertIDIs400NamingTheField(t *testing.T) {
	h, _ := azHandler(t)
	w := postAdapter(h, azPath, "propose-token", azAlert("", "Fired", azResource), azKey)
	if w.Code != 400 || errorBody(t, w).Code != "invalid_request" ||
		!strings.Contains(errorBody(t, w).Error, "data.essentials.alertId") {
		t.Fatalf("missing alertId: %d %s", w.Code, w.Body.String())
	}
}

func TestAzureMonitor_InvalidPayloads(t *testing.T) {
	h, _ := azHandler(t)
	wrongSchema := strings.Replace(string(azAlert(azAlertID(azAlertGUID), "Fired",
		azResource)), "azureMonitorCommonAlertSchema", "Microsoft.Insights/activityLogs", 1)
	for name, c := range map[string]struct {
		body, field string
	}{
		"not json":     {`{"schemaId":`, "azure alert"},
		"wrong schema": {wrongSchema, "schemaId"},
		"no data":      {`{"schemaId":"azureMonitorCommonAlertSchema"}`, "alertId"},
		"bad guid": {string(azAlert(azAlertID("x;y"), "Fired", azResource)),
			"data.essentials.alertId"},
	} {
		w := postAdapter(h, azPath, "propose-token", []byte(c.body), azKey)
		if w.Code != 400 || !strings.Contains(errorBody(t, w).Error, c.field) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestAzureMonitor_ResolvedAlertIsIgnored(t *testing.T) {
	h, hs := azHandler(t)
	w := postAdapter(h, azPath, "propose-token",
		azAlert(azAlertID(azAlertGUID), "Resolved", azResource), azKey)
	var ig WebhookIgnored
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &ig) != nil || !ig.Ignored ||
		ig.Code != "" || !strings.Contains(ig.Reason, "Resolved") {
		t.Fatalf("resolved: %d %s", w.Code, w.Body.String())
	}
	if len(hs.orders.started) != 0 {
		t.Fatal("a resolved alert opened an investigation")
	}
}

func TestAzureMonitor_UnmappedResourceIsUnknownDatabase(t *testing.T) {
	h, hs := azHandler(t)
	for name, target := range map[string]string{"other server": strings.Replace(azResource,
		"orders-pg", "inventory-pg", 1), "no target": ""} {
		body := azAlert(azAlertID(azAlertGUID), "Fired", target)
		if target == "" {
			body = []byte(strings.Replace(string(body), `"alertTargetIDs":[""]`,
				`"alertTargetIDs":[]`, 1))
		}
		w := postAdapter(h, azPath, "propose-token", body, azKey)
		var ig WebhookIgnored
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &ig) != nil ||
			ig.Code != CodeUnknownDatabase ||
			!strings.Contains(ig.Reason, "specialist.azure_monitor.resources") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(hs.orders.started) != 0 {
		t.Fatal("an unmapped alert opened an investigation")
	}
}

func TestAzureMonitor_DisabledIs404(t *testing.T) {
	hs := newHarness(t, DefaultLimits())
	h := NewHandler(hs.svc, testAuth, HandlerOptions{Now: hs.clock.Now})
	w := postAdapter(h, azPath, "propose-token",
		azAlert(azAlertID(azAlertGUID), "Fired", azResource), azKey)
	if w.Code != 404 || errorBody(t, w).Code != "disabled" {
		t.Fatalf("unconfigured adapter: %d %s", w.Code, w.Body.String())
	}
}

func TestAzureMonitor_BodyAtTheLimitIsAcceptedOneMoreByteIsNot(t *testing.T) {
	h, _ := azHandler(t)
	body := padBody(t, azAlert(azAlertID(azAlertGUID), "Fired", azResource), maxAdapterBody)
	if w := postAdapter(h, azPath, "propose-token", body, azKey); w.Code != 201 {
		t.Fatalf("at the limit: %d %s", w.Code, w.Body.String())
	}
	over := append(append([]byte(nil), body...), ' ')
	if w := postAdapter(h, azPath, "propose-token", over, azKey); w.Code != 413 ||
		errorBody(t, w).Code != "payload_too_large" {
		t.Fatalf("one byte over: %d %s", w.Code, w.Body.String())
	}
}

func TestAzureMonitor_TenFiringsOfOneAlertShareOneInvestigation(t *testing.T) {
	h, hs := azHandler(t)
	body := azAlert(azAlertID(azAlertGUID), "Fired", azResource)
	codes, ids := concurrentPosts(h, azPath, body, azKey, 10)
	assertOneInvestigation(t, codes, ids)
	if n := len(hs.orders.invs); n != 1 {
		t.Fatalf("%d investigations for one alert", n)
	}
}

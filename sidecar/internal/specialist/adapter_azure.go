package specialist

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// Azure Monitor alerts in the common alert schema (mcp-02), no vendor
// SDK. An action group's webhook cannot sign its body, so the operator's
// shared key travels in the X-Sage-Azure-Key header or the key query
// parameter of the webhook URL and is compared in constant time; the
// bearer MCP token is still required (an action group cannot set headers,
// so it reaches pg_sage through a relay that adds it, such as a Logic App,
// or the Azure SRE Agent calls the contract directly). A Fired alert on a
// mapped resource opens an investigation; Resolved and other conditions
// are ignored.

const azureCommonSchema = "azureMonitorCommonAlertSchema"

// azureAlertGUID is the last segment of essentials.alertId.
var azureAlertGUID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// AzureMonitorAdapter verifies and translates Azure Monitor alerts.
type AzureMonitorAdapter struct {
	Secret    string
	Resources map[string]PagerDutyRoute // by lower-cased resource id
}

// VerifyAzureMonitorKey checks the shared key from the header, else the
// query parameter (a header that is present decides alone).
func VerifyAzureMonitorKey(secret, header, query string) error {
	got := header
	if got == "" {
		got = query
	}
	if secret == "" || got == "" {
		return ErrSignature
	}
	want, have := sha256.Sum256([]byte(secret)), sha256.Sum256([]byte(got))
	if subtle.ConstantTimeCompare(want[:], have[:]) != 1 {
		return ErrSignature
	}
	return nil
}

type azureAlert struct {
	SchemaID string `json:"schemaId"`
	Data     struct {
		Essentials struct {
			AlertID          string    `json:"alertId"`
			AlertRule        string    `json:"alertRule"`
			MonitorCondition string    `json:"monitorCondition"`
			AlertTargetIDs   []string  `json:"alertTargetIDs"`
			FiredDateTime    time.Time `json:"firedDateTime"`
		} `json:"essentials"`
	} `json:"data"`
}

// decodeAzureAlert reads and checks the common-schema essentials; the
// errors name the field the operator's action group must supply.
func decodeAzureAlert(body []byte) (azureAlert, string, error) {
	var a azureAlert
	if err := json.Unmarshal(body, &a); err != nil {
		return a, "", invalidf("azure alert: %v", err)
	}
	if a.SchemaID != azureCommonSchema {
		return a, "", invalidf("azure alert: schemaId must be %s (enable the common alert "+
			"schema on the action group)", azureCommonSchema)
	}
	e := a.Data.Essentials
	if strings.TrimSpace(e.AlertID) == "" {
		return a, "", invalidf("azure alert: data.essentials.alertId is required")
	}
	guid := path.Base(strings.TrimSpace(e.AlertID))
	if !azureAlertGUID.MatchString(guid) {
		return a, "", invalidf("azure alert: data.essentials.alertId is malformed")
	}
	return a, guid, nil
}

// route is the first alert target a resource route names.
func (ad *AzureMonitorAdapter) route(a azureAlert) (PagerDutyRoute, bool) {
	for _, target := range a.Data.Essentials.AlertTargetIDs {
		if r, ok := ad.Resources[strings.ToLower(strings.TrimSpace(target))]; ok {
			return r, true
		}
	}
	return PagerDutyRoute{}, false
}

func azureRequest(a azureAlert, guid string, route PagerDutyRoute, now time.Time) OpenRequest {
	e := a.Data.Essentials
	title := dataText(e.AlertRule, maxSummaryRunes)
	if title == "" {
		title = "Azure Monitor alert " + guid
	}
	req := OpenRequest{Symptom: &Symptom{Summary: title}, Family: route.Family,
		ExternalRef:    &ExternalRef{System: "azure_monitor", ID: guid},
		IdempotencyKey: "azure_monitor:" + guid}
	return withAlertWindow(req, e.FiredDateTime, now)
}

func (h *handler) azureMonitor(w http.ResponseWriter, r *http.Request, id Identity) {
	ad := h.opts.AzureMonitor
	if ad == nil {
		writeError(w, ErrDisabled)
		return
	}
	body, err := readAdapterBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := VerifyAzureMonitorKey(ad.Secret, r.Header.Get("X-Sage-Azure-Key"),
		r.URL.Query().Get("key")); err != nil {
		writeError(w, err)
		return
	}
	alert, guid, err := decodeAzureAlert(body)
	if err != nil {
		writeError(w, err)
		return
	}
	if c := alert.Data.Essentials.MonitorCondition; c != "Fired" {
		ignoreEvent(w, "monitor condition "+safeLabel(c)+" does not open an investigation")
		return
	}
	route, ok := ad.route(alert)
	if !ok {
		ignoreUnmapped(w, "alert "+safeLabel(guid)+"'s target resource",
			"specialist.azure_monitor.resources")
		return
	}
	h.respondOpen(w, r.Context(), id, route.Database, azureRequest(alert, guid, route,
		h.opts.Now()))
}

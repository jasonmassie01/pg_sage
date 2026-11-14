package notify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// alerting-06b: the YAML alerting block is reconciled into config-managed
// notification channels and rules, so the notify outbox is the one engine.

const (
	testSlackURL = "https://hooks.slack.example.com/services/T0/B0/x"
	testPDKey    = "0123456789abcdef0123456789abcdef"
)

type reconcileLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *reconcileLog) fn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *reconcileLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func baseAlerting() config.AlertingConfig {
	return config.AlertingConfig{
		Enabled:             true,
		SlackWebhookURL:     testSlackURL,
		PagerDutyRoutingKey: testPDKey,
		Webhooks: []config.WebhookConfig{{Name: "ops",
			URL: "https://ops.example.com/hook", Headers: map[string]string{"X-T": "s"}}},
		Routes: []config.AlertRoute{
			{Severity: "critical", Channels: []string{"slack", "pagerduty"}},
			{Severity: "warning", Channels: []string{"webhook:ops"}},
		},
	}
}

func reconcileMem(
	t *testing.T, m *MemoryStore, a config.AlertingConfig,
) (ReconcileResult, *reconcileLog) {
	t.Helper()
	logs := &reconcileLog{}
	res, err := Reconcile(context.Background(), m, a, TargetPolicy{}, logs.fn)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res, logs
}

// ruleView is a rule with its channel's name, sorted for comparison.
func ruleView(m *MemoryStore) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.rules {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", m.channels[r.ChannelID].Name,
			r.Event, r.MinSeverity, r.ManagedBy))
	}
	sort.Strings(out)
	return out
}

func channelNames(m *MemoryStore) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, ch := range m.channels {
		out = append(out, ch.Name+"|"+ch.Type+"|"+ch.ManagedBy)
	}
	sort.Strings(out)
	return out
}

func TestReconcile_FirstRunCreatesThenSecondIsNoop(t *testing.T) {
	m := NewMemoryStore()
	res, _ := reconcileMem(t, m, baseAlerting())
	want := ReconcileResult{ChannelsCreated: 3, RulesCreated: 6}
	if res != want {
		t.Fatalf("first reconcile = %+v, want %+v", res, want)
	}
	gotCh := strings.Join(channelNames(m), ",")
	wantCh := "config:pagerduty:pagerduty|pagerduty|config,config:slack:slack|slack|config," +
		"config:webhook:ops|webhook|config"
	if gotCh != wantCh {
		t.Fatalf("channels = %s, want %s", gotCh, wantCh)
	}
	wantRules := []string{
		"config:pagerduty:pagerduty|finding_opened|critical|config",
		"config:pagerduty:pagerduty|finding_resolved|critical|config",
		"config:slack:slack|finding_opened|critical|config",
		"config:slack:slack|finding_resolved|critical|config",
		"config:webhook:ops|finding_opened|warning|config",
		"config:webhook:ops|finding_resolved|warning|config",
	}
	if got := ruleView(m); strings.Join(got, ",") != strings.Join(wantRules, ",") {
		t.Fatalf("rules = %v, want %v", got, wantRules)
	}
	again, _ := reconcileMem(t, m, baseAlerting())
	if again != (ReconcileResult{}) {
		t.Fatalf("second reconcile = %+v, want all zero", again)
	}
}

func TestReconcile_ChannelSecretsAndHeadersAreStored(t *testing.T) {
	m := NewMemoryStore()
	reconcileMem(t, m, baseAlerting())
	got := map[string]map[string]string{}
	m.mu.Lock()
	for _, ch := range m.channels {
		got[ch.Name] = ch.Config
	}
	m.mu.Unlock()
	if got["config:slack:slack"]["webhook_url"] != testSlackURL {
		t.Fatalf("slack config = %v", got["config:slack:slack"])
	}
	if got["config:pagerduty:pagerduty"]["routing_key"] != testPDKey {
		t.Fatalf("pagerduty config = %v", got["config:pagerduty:pagerduty"])
	}
	hook := got["config:webhook:ops"]
	if hook["webhook_url"] != "https://ops.example.com/hook" || hook["headers"] != `{"X-T":"s"}` {
		t.Fatalf("webhook config = %v", hook)
	}
}

func TestReconcile_RemovingARouteDeletesOnlyItsRules(t *testing.T) {
	m := NewMemoryStore()
	reconcileMem(t, m, baseAlerting())
	a := baseAlerting()
	a.Routes = a.Routes[:1]
	res, _ := reconcileMem(t, m, a)
	if res != (ReconcileResult{RulesDeleted: 2}) {
		t.Fatalf("reconcile without the warning route = %+v, want 2 rules deleted", res)
	}
	for _, r := range ruleView(m) {
		if strings.HasPrefix(r, "config:webhook:ops") {
			t.Fatalf("webhook rule %s survived its route", r)
		}
	}
	if n := len(channelNames(m)); n != 3 {
		t.Fatalf("channels = %d, want the 3 targets kept", n)
	}
}

func TestReconcile_RenamingAWebhookReplacesItsChannel(t *testing.T) {
	m := NewMemoryStore()
	reconcileMem(t, m, baseAlerting())
	a := baseAlerting()
	a.Webhooks[0].Name = "oncall"
	a.Routes[1].Channels = []string{"oncall"}
	res, _ := reconcileMem(t, m, a)
	want := ReconcileResult{ChannelsCreated: 1, ChannelsDeleted: 1, RulesCreated: 2,
		RulesDeleted: 2}
	if res != want {
		t.Fatalf("rename = %+v, want %+v", res, want)
	}
	names := strings.Join(channelNames(m), ",")
	if strings.Contains(names, "config:webhook:ops") ||
		!strings.Contains(names, "config:webhook:oncall") {
		t.Fatalf("channels after rename = %s", names)
	}
}

func TestReconcile_ChangedSecretUpdatesInPlace(t *testing.T) {
	m := NewMemoryStore()
	reconcileMem(t, m, baseAlerting())
	before := channelID(t, m, "config:slack:slack")
	a := baseAlerting()
	a.SlackWebhookURL = testSlackURL + "y"
	res, _ := reconcileMem(t, m, a)
	if res != (ReconcileResult{ChannelsUpdated: 1}) {
		t.Fatalf("changed URL = %+v, want one update", res)
	}
	if after := channelID(t, m, "config:slack:slack"); after != before {
		t.Fatalf("slack channel id %d -> %d, want updated in place", before, after)
	}
}

func channelID(t *testing.T, m *MemoryStore, name string) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ch := range m.channels {
		if ch.Name == name {
			return id
		}
	}
	t.Fatalf("channel %s not found", name)
	return 0
}

func TestReconcile_UnknownRouteChannelIsSkippedAndLogged(t *testing.T) {
	m := NewMemoryStore()
	a := baseAlerting()
	a.Routes = append(a.Routes, config.AlertRoute{Severity: "info",
		Channels: []string{"teams"}})
	res, logs := reconcileMem(t, m, a)
	if res.RulesCreated != 6 {
		t.Fatalf("rules created = %d, want 6 (the unknown channel builds none)", res.RulesCreated)
	}
	if got := logs.joined(); !strings.Contains(got, `info route channel "teams"`) {
		t.Fatalf("log = %q, want the route and channel named", got)
	}
}

func TestReconcile_TargetRefusedByPolicyBuildsNoChannel(t *testing.T) {
	m := NewMemoryStore()
	a := baseAlerting()
	a.SlackWebhookURL = "http://169.254.169.254/latest"
	res, logs := reconcileMem(t, m, a)
	if res.ChannelsCreated != 2 || res.RulesCreated != 4 {
		t.Fatalf("result = %+v, want slack left out", res)
	}
	if strings.Contains(strings.Join(channelNames(m), ","), "config:slack:slack") {
		t.Fatal("a refused slack target built a channel")
	}
	if got := logs.joined(); !strings.Contains(got, "alerting.slack_webhook_url") {
		t.Fatalf("log = %q, want the refused target named", got)
	}
}

func TestReconcile_DisabledBlockDeletesOnlyConfigRows(t *testing.T) {
	m := NewMemoryStore()
	apiID := m.AddChannel(Channel{Name: "team-slack", Type: "slack", Enabled: true,
		Config: map[string]string{"webhook_url": testSlackURL}})
	m.AddRule(Rule{ChannelID: apiID, Event: "finding_opened", MinSeverity: "info",
		Enabled: true})
	reconcileMem(t, m, baseAlerting())
	res, _ := reconcileMem(t, m, config.AlertingConfig{})
	if res != (ReconcileResult{ChannelsDeleted: 3, RulesDeleted: 6}) {
		t.Fatalf("no alerting block = %+v, want every config row deleted", res)
	}
	if got := strings.Join(channelNames(m), ","); got != "team-slack|slack|" {
		t.Fatalf("channels = %s, want the API channel only", got)
	}
	if got := ruleView(m); len(got) != 1 || got[0] != "team-slack|finding_opened|info|" {
		t.Fatalf("rules = %v, want the API rule only", got)
	}
}

func TestReconcile_EmptyBlockIsZeroRows(t *testing.T) {
	m := NewMemoryStore()
	a := config.AlertingConfig{Enabled: true}
	res, _ := reconcileMem(t, m, a)
	if res != (ReconcileResult{}) || len(channelNames(m)) != 0 {
		t.Fatalf("empty block = %+v, channels %v; want nothing", res, channelNames(m))
	}
}

func TestReconcile_TwoRoutesOnOneChannelMapMinSeverityExactly(t *testing.T) {
	m := NewMemoryStore()
	a := baseAlerting()
	a.Webhooks = nil
	a.PagerDutyRoutingKey = ""
	a.Routes = []config.AlertRoute{
		{Severity: "critical", Channels: []string{"slack"}},
		{Severity: "warning", Channels: []string{"slack"}},
	}
	res, _ := reconcileMem(t, m, a)
	if res != (ReconcileResult{ChannelsCreated: 1, RulesCreated: 2}) {
		t.Fatalf("result = %+v, want one channel and two rules", res)
	}
	want := "config:slack:slack|finding_opened|warning|config," +
		"config:slack:slack|finding_resolved|warning|config"
	if got := strings.Join(ruleView(m), ","); got != want {
		t.Fatalf("rules = %s, want %s", got, want)
	}
}

func TestReconcile_StoreErrorRollsBackAndNamesTheChannel(t *testing.T) {
	m := NewMemoryStore()
	m.FailReconcile, m.FailReconcileAt = errors.New("disk full"), "config:webhook:ops"
	_, err := Reconcile(context.Background(), m, baseAlerting(), TargetPolicy{}, nil)
	if err == nil || !strings.Contains(err.Error(), `"config:webhook:ops"`) ||
		!strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the failing channel and cause", err)
	}
	if n := len(channelNames(m)); n != 0 || len(ruleView(m)) != 0 {
		t.Fatalf("after a failed reconcile: %d channels, rules %v; want none", n, ruleView(m))
	}
}

func TestReconcile_NilStoreIsAnError(t *testing.T) {
	if _, err := Reconcile(context.Background(), nil, baseAlerting(),
		TargetPolicy{}, nil); err == nil {
		t.Fatal("nil store: want an error")
	}
}

func TestReconcile_ConcurrentReconcilersConverge(t *testing.T) {
	m := NewMemoryStore()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Reconcile(context.Background(), m, baseAlerting(),
				TargetPolicy{}, nil); err != nil {
				t.Errorf("Reconcile: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(channelNames(m)); n != 3 || len(ruleView(m)) != 6 {
		t.Fatalf("channels %d, rules %d; want 3 and 6", n, len(ruleView(m)))
	}
}

func TestConfigChannelName(t *testing.T) {
	if got := ConfigChannelName("webhook", "ops"); got != "config:webhook:ops" {
		t.Fatalf("ConfigChannelName = %q", got)
	}
	if !IsConfigChannelName("config:slack:slack") || IsConfigChannelName("team-slack") {
		t.Fatal("IsConfigChannelName misclassified a name")
	}
}

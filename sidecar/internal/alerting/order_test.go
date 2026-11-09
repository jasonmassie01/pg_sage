package alerting

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Delivery order (alerting-05): severity groups are dispatched critical,
// warning, info, then any other severity in sorted order, so a critical
// page never queues behind warning sends. With a nil pool logAlert does
// nothing, so these tests need no database.
//
// Concurrent access: not applicable; delivery stays serial by design (the
// per-channel outbox is alerting-06).

// sentAlert is one Send call seen by orderChannel.
type sentAlert struct {
	severity string
	id       int64
	resolved bool
}

// orderChannel records every alert it receives, in call order.
type orderChannel struct {
	name string
	mu   sync.Mutex
	sent []sentAlert
}

func (c *orderChannel) Name() string { return c.name }

func (c *orderChannel) Send(_ context.Context, a Alert) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range a.Findings {
		c.sent = append(c.sent, sentAlert{a.Severity, f.ID, a.Resolved})
	}
	return nil
}

func (c *orderChannel) severities() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sent))
	for _, s := range c.sent {
		out = append(out, s.severity)
	}
	return out
}

func (c *orderChannel) ids() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int64, 0, len(c.sent))
	for _, s := range c.sent {
		out = append(out, s.id)
	}
	return out
}

func orderFinding(id int64, sev string) AlertFinding {
	return AlertFinding{ID: id, Severity: sev, Category: "order_test",
		ObjectIdentifier: fmt.Sprintf("obj_%d", id)}
}

// routeAll routes every given severity to the one channel ch.
func routeAll(ch Channel, sevs ...string) map[string][]Channel {
	routes := map[string][]Channel{}
	for _, s := range sevs {
		routes[s] = []Channel{ch}
	}
	return routes
}

// TestDispatchFindings_CriticalFirstAlways is CHECK-14: 20 warnings and
// one critical (last in the input, as a pending finding would be), run
// 200 times. The critical is the first send every time.
func TestDispatchFindings_CriticalFirstAlways(t *testing.T) {
	findings := make([]AlertFinding, 0, 21)
	for i := int64(1); i <= 20; i++ {
		findings = append(findings, orderFinding(i, "warning"))
	}
	findings = append(findings, orderFinding(99, "critical"))
	misordered := 0
	for run := 0; run < 200; run++ {
		ch := &orderChannel{name: "slack"}
		m := New(nil, ManagerConfig{}, routeAll(ch, "critical", "warning"), noopLog)
		m.dispatchFindings(context.Background(), findings)
		ids := ch.ids()
		if len(ids) != 21 {
			t.Fatalf("run %d: %d sends, want 21 (one per finding)", run, len(ids))
		}
		if ids[0] != 99 {
			misordered++
		}
	}
	if misordered != 0 {
		t.Errorf("critical not sent first in %d of 200 runs", misordered)
	}
}

// TestDispatchFindings_KeepsOrderWithinGroup: inside a severity the input
// order (fresh by last_seen DESC, then pending) is kept.
func TestDispatchFindings_KeepsOrderWithinGroup(t *testing.T) {
	findings := []AlertFinding{
		orderFinding(5, "warning"), orderFinding(3, "critical"),
		orderFinding(1, "warning"), orderFinding(4, "info"),
		orderFinding(2, "critical"), orderFinding(6, "warning"),
	}
	ch := &orderChannel{name: "slack"}
	m := New(nil, ManagerConfig{}, routeAll(ch, "critical", "warning", "info"), noopLog)
	m.dispatchFindings(context.Background(), findings)
	got := fmt.Sprint(ch.ids())
	if want := "[3 2 5 1 6 4]"; got != want {
		t.Errorf("send order = %s, want %s", got, want)
	}
}

// TestDispatchFindings_UnknownSeverityAfterInfo: a routed severity outside
// the known three ("notice", "debug") is sent after info, in sorted name
// order, on every run.
func TestDispatchFindings_UnknownSeverityAfterInfo(t *testing.T) {
	findings := []AlertFinding{
		orderFinding(1, "notice"), orderFinding(2, "info"),
		orderFinding(3, "debug"), orderFinding(4, "warning"),
		orderFinding(5, "critical"),
	}
	for run := 0; run < 50; run++ {
		ch := &orderChannel{name: "slack"}
		routes := routeAll(ch, "critical", "warning", "info", "notice", "debug")
		m := New(nil, ManagerConfig{}, routes, noopLog)
		m.dispatchFindings(context.Background(), findings)
		got := strings.Join(ch.severities(), ",")
		if want := "critical,warning,info,debug,notice"; got != want {
			t.Fatalf("run %d: severity order = %s, want %s", run, got, want)
		}
	}
}

// TestDispatchFindings_UnroutedSeveritySkipped: a severity without a route
// sends nothing; the routed ones are still sent in order.
func TestDispatchFindings_UnroutedSeveritySkipped(t *testing.T) {
	findings := []AlertFinding{
		orderFinding(1, "info"), orderFinding(2, "warning"),
		orderFinding(3, "critical"),
	}
	ch := &orderChannel{name: "slack"}
	m := New(nil, ManagerConfig{}, routeAll(ch, "critical", "info"), noopLog)
	m.dispatchFindings(context.Background(), findings)
	if got := fmt.Sprint(ch.ids()); got != "[3 1]" {
		t.Errorf("send order = %s, want [3 1] (warning unrouted)", got)
	}
}

// TestDispatchFindings_ChannelOrderWithinFinding: each finding goes to its
// route's channels in route order, before the next finding is sent.
func TestDispatchFindings_ChannelOrderWithinFinding(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	rec := func(name string) Channel {
		return &hookChannel{name: name, hook: func(a Alert) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, fmt.Sprintf("%s:%d", name, a.Findings[0].ID))
		}}
	}
	pd, slack := rec("pagerduty"), rec("slack")
	routes := map[string][]Channel{"critical": {pd, slack}, "warning": {slack}}
	m := New(nil, ManagerConfig{}, routes, noopLog)
	m.dispatchFindings(context.Background(), []AlertFinding{
		orderFinding(1, "warning"), orderFinding(2, "critical"), orderFinding(3, "critical"),
	})
	got := strings.Join(calls, " ")
	if want := "pagerduty:2 slack:2 pagerduty:3 slack:3 slack:1"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// hookChannel runs hook on every send.
type hookChannel struct {
	name string
	hook func(Alert)
}

func (c *hookChannel) Name() string { return c.name }

func (c *hookChannel) Send(_ context.Context, a Alert) error {
	c.hook(a)
	return nil
}

// TestDispatchFindings_Empty: no findings means no sends and no panic,
// with routes, without routes and with a nil route map.
func TestDispatchFindings_Empty(t *testing.T) {
	ch := &orderChannel{name: "slack"}
	for _, routes := range []map[string][]Channel{routeAll(ch, "critical"), {}, nil} {
		m := New(nil, ManagerConfig{}, routes, noopLog)
		m.dispatchFindings(context.Background(), nil)
		m.dispatchFindings(context.Background(), []AlertFinding{})
	}
	if n := len(ch.ids()); n != 0 {
		t.Errorf("%d sends for no findings, want 0", n)
	}
}

// TestOrderedSeverityGroups covers the ordering helper directly.
func TestOrderedSeverityGroups(t *testing.T) {
	cases := []struct {
		name string
		in   []AlertFinding
		want string
	}{
		{"nil", nil, ""},
		{"empty", []AlertFinding{}, ""},
		{"known order", []AlertFinding{orderFinding(1, "info"),
			orderFinding(2, "warning"), orderFinding(3, "critical"),
			orderFinding(4, "warning")}, "critical[3] warning[2 4] info[1]"},
		{"unknown sorted after info", []AlertFinding{orderFinding(1, "zeta"),
			orderFinding(2, "alpha"), orderFinding(3, "info")},
			"info[3] alpha[2] zeta[1]"},
		{"empty severity is unknown", []AlertFinding{orderFinding(1, ""),
			orderFinding(2, "critical")}, "critical[2] [1]"},
		{"case-sensitive names", []AlertFinding{orderFinding(1, "Critical"),
			orderFinding(2, "info")}, "info[2] Critical[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			for _, g := range orderedSeverityGroups(tc.in) {
				ids := make([]int64, 0, len(g.findings))
				for _, f := range g.findings {
					if f.Severity != g.severity {
						t.Errorf("finding %d (%s) in group %s", f.ID, f.Severity, g.severity)
					}
					ids = append(ids, f.ID)
				}
				parts = append(parts, fmt.Sprintf("%s%v", g.severity, ids))
			}
			if got := strings.Join(parts, " "); got != tc.want {
				t.Errorf("groups = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOrderResolveTargets: resolve notifications walk critical first too,
// keeping row order (finding id) inside a severity.
func TestOrderResolveTargets(t *testing.T) {
	targets := groupResolveRows([]resolveRow{
		{finding: orderFinding(1, "warning"), channel: "slack"},
		{finding: orderFinding(2, "notice"), channel: "slack"},
		{finding: orderFinding(3, "critical"), channel: "slack"},
		{finding: orderFinding(4, "info"), channel: "slack"},
		{finding: orderFinding(5, "warning"), channel: "slack"},
		{finding: orderFinding(6, "critical"), channel: "pagerduty"},
	})
	orderResolveTargets(targets)
	ids := make([]int64, 0, len(targets))
	for _, tg := range targets {
		ids = append(ids, tg.finding.ID)
	}
	if got := fmt.Sprint(ids); got != "[3 6 1 5 4 2]" {
		t.Errorf("resolve order = %s, want [3 6 1 5 4 2]", got)
	}
	orderResolveTargets(nil) // nil/empty: must not panic
}

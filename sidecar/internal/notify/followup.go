package notify

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// Diagnosis follow-up (sre-08). When a Sage SRE investigation of an
// incident concludes, the engineer who was paged gets the diagnosis on the
// channels the page reached: never on others, never on PagerDuty (Events
// v2 has no note), threaded under the Telegram page. The follow-up is not
// a rule event, so no notification rule selects it and there is no new
// configuration.

// EventInvestigationConcluded is the follow-up's event type in
// sage.notification_log.
const EventInvestigationConcluded = "investigation_concluded"

// ErrFollowUpUnsupported is returned by FollowUp when the dispatcher's
// store does not record incident pages.
var ErrFollowUpUnsupported = errors.New("notification store does not record incident pages")

// ConclusionInfo is the diagnosis a follow-up carries. It is a plain
// struct so notify stays independent of the sre package; every text field
// is already redacted by the sender of the signal.
type ConclusionInfo struct {
	IncidentID      string
	InvestigationID string
	CaseID          string
	Database        string
	Family          string
	State           string // concluded or inconclusive
	RootLabel       string
	Reason          string
	Severity        string // the incident's severity
}

// IncidentPage is one channel's sent page of an incident.
type IncidentPage struct {
	ChannelID         int
	Subject           string
	ProviderMessageID string
}

// IncidentPageStore lists the channels an incident's page reached.
type IncidentPageStore interface {
	IncidentPages(ctx context.Context, incidentID string) ([]IncidentPage, error)
}

// InvestigationConcludedEvent is the diagnosis follow-up of an incident;
// its severity is the incident's.
func InvestigationConcludedEvent(info ConclusionInfo) Event {
	root := info.RootLabel
	if info.State != "concluded" || root == "" {
		root = "inconclusive"
	}
	head := fmt.Sprintf("%s - %s", info.Family, root)
	body := fmt.Sprintf("Diagnosis for incident %s on %s: %s", info.IncidentID,
		info.Database, head)
	if reason := strings.TrimRight(strings.TrimSpace(info.Reason), "."); reason != "" {
		body += ": " + reason
	}
	body += ". Case " + info.CaseID + "."
	return Event{Type: EventInvestigationConcluded, Severity: incidentSeverity(info.Severity),
		Subject: "Diagnosis: " + head, Body: body, RefID: info.IncidentID,
		DedupKey: "investigation:" + info.InvestigationID,
		Data: map[string]any{"incident_id": info.IncidentID,
			"investigation_id": info.InvestigationID, "case_id": info.CaseID,
			"database": info.Database, "family": info.Family, "state": info.State,
			"root_label": info.RootLabel}}
}

// FollowUp sends evt to each channel whose incident_detected or
// incident_escalated page for incidentID was sent, titled "Re: <page
// subject>" and, on Telegram, as a reply to the page. Every attempt is
// recorded in the notification log under the incident; the send failures
// are returned joined. An incident whose page was never sent gets nothing.
func (d *Dispatcher) FollowUp(ctx context.Context, incidentID string, evt Event) error {
	if strings.TrimSpace(incidentID) == "" {
		return fmt.Errorf("follow-up needs an incident id")
	}
	pages, ok := d.store.(IncidentPageStore)
	if !ok {
		return ErrFollowUpUnsupported
	}
	list, err := pages.IncidentPages(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("follow-up of incident %s: %w", incidentID, err)
	}
	var errs []error
	for _, p := range list {
		if err := d.followUpPage(ctx, incidentID, p, evt); err != nil {
			errs = append(errs, fmt.Errorf("channel %d: %w", p.ChannelID, err))
		}
	}
	return errors.Join(errs...)
}

func (d *Dispatcher) followUpPage(ctx context.Context, incidentID string, p IncidentPage,
	evt Event) error {
	ch, err := d.loadChannel(ctx, p.ChannelID)
	if err != nil {
		return fmt.Errorf("loading channel: %w", err)
	}
	if !ch.Enabled || ch.Type == "pagerduty" {
		return nil
	}
	out := evt
	out.Subject = "Re: " + p.Subject
	out.RefID = incidentID
	out.Data = maps.Clone(evt.Data)
	if out.Data == nil {
		out.Data = map[string]any{}
	}
	if id, err := strconv.ParseInt(p.ProviderMessageID, 10, 64); err == nil && id > 0 &&
		ch.Type == "telegram" {
		out.Data[DataReplyTo] = id
	}
	sender, ok := d.senders[ch.Type]
	if !ok {
		return d.logDelivery(ctx, ch.ID, out, "error",
			fmt.Sprintf("no sender for type %q", ch.Type))
	}
	return d.deliver(ctx, sender, *ch, out)
}

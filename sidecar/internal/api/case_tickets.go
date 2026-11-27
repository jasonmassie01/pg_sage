package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// Case tickets (market-14): an operator turns a case into a Jira issue;
// viewers see its status. The Jira integration is a "jira" notification
// channel, so its api_token is sealed like every channel secret.

type caseTicketRequest struct {
	Database  string `json:"database"`
	CaseID    string `json:"case_id"`
	ChannelID int    `json:"channel_id,omitempty"`
	CaseURL   string `json:"case_url,omitempty"`
}

func registerCaseTicketRoutes(
	mux *http.ServeMux, mgr *fleet.DatabaseManager, pool *pgxpool.Pool,
	ns *store.NotificationStore, deps notificationRouteDeps,
) {
	tickets := cases.NewTicketStore(pool)
	svc := cases.TicketService{Store: tickets, Tracker: jiraTrackerFor(ns, deps.policy)}
	route(mux, "GET /api/v1/case-tickets", gateViewer,
		http.HandlerFunc(listCaseTicketsHandler(tickets, ns)))
	// The body names the one database the case is on (apiauth-08).
	routeDB(mux, "POST /api/v1/case-tickets", gateOperator, bodyDatabase,
		http.HandlerFunc(createCaseTicketHandler(mgr, ns, svc)))
}

// ticketLister lists the created case tickets of a database.
type ticketLister interface {
	List(ctx context.Context, database string) ([]cases.Ticket, error)
}

// channelLister lists the notification channels (the Jira integrations).
type channelLister interface {
	ListChannels(ctx context.Context) ([]store.ChannelListing, error)
}

// listCaseTicketsHandler answers the tickets and whether a Jira
// integration is enabled, so the dashboard offers "Create ticket" only
// when it can work.
func listCaseTicketsHandler(tickets ticketLister, chans channelLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		list, err := tickets.List(r.Context(), database)
		if err != nil {
			slog.Error("listing case tickets", "error", err)
			jsonError(w, "could not read case tickets", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{"tickets": list, "jira_enabled": jiraEnabled(r.Context(),
			chans)})
	}
}

func createCaseTicketHandler(
	mgr *fleet.DatabaseManager, ns *store.NotificationStore, svc cases.TicketService,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeCaseTicketRequest(r.Body)
		if err != nil {
			writeCaseTicketError(w, err)
			return
		}
		c, status, msg := findTicketCase(r.Context(), mgr, req)
		if status != 0 {
			jsonError(w, msg, status)
			return
		}
		if req.ChannelID == 0 {
			listing, err := ns.ListChannels(r.Context())
			if err == nil {
				req.ChannelID, err = pickJiraChannel(listing)
			}
			if err != nil {
				writeCaseTicketError(w, err)
				return
			}
		}
		t, created, err := svc.Create(r.Context(), cases.TicketRequest{Case: c,
			ChannelID: req.ChannelID, CaseURL: req.CaseURL, Actor: actorName(r)})
		if err != nil {
			writeCaseTicketError(w, err)
			return
		}
		if created {
			w.WriteHeader(http.StatusCreated)
		}
		jsonResponse(w, map[string]any{"ticket": t, "created": created})
	}
}

func actorName(r *http.Request) string {
	if u := UserFromContext(r.Context()); u != nil {
		return u.Email
	}
	return ""
}

// decodeCaseTicketRequest reads and checks the request body.
func decodeCaseTicketRequest(body io.Reader) (caseTicketRequest, error) {
	var req caseTicketRequest
	dec := json.NewDecoder(io.LimitReader(body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("%w: body: %v", cases.ErrTicketInvalid, err)
	}
	switch {
	case req.Database == "" || req.Database == "all":
		return req, fmt.Errorf("%w: database names the case's database", cases.ErrTicketInvalid)
	case !validCaseIdentity(req.CaseID):
		return req, fmt.Errorf("%w: case_id is not a case identity", cases.ErrTicketInvalid)
	case req.ChannelID < 0:
		return req, fmt.Errorf("%w: channel_id must be positive", cases.ErrTicketInvalid)
	case req.CaseURL != "" && !absoluteHTTPURL(req.CaseURL):
		return req, fmt.Errorf("%w: case_url must be an absolute http(s) URL",
			cases.ErrTicketInvalid)
	}
	return req, nil
}

func absoluteHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && (u.Scheme == "https" || u.Scheme == "http") &&
		u.Host != ""
}

// findTicketCase reads the case the request names from its database; a
// non-zero status is the answer when it cannot.
func findTicketCase(
	ctx context.Context, mgr *fleet.DatabaseManager, req caseTicketRequest,
) (cases.Case, int, string) {
	if mgr == nil {
		return cases.Case{}, http.StatusServiceUnavailable, "no databases are monitored"
	}
	state, c, err := readCaseState(ctx, mgr, req.Database, req.CaseID)
	switch {
	case errors.Is(err, errUnknownTicketDatabase):
		return c, http.StatusNotFound, "unknown database"
	case err != nil:
		return c, http.StatusServiceUnavailable, "the case's database could not be read"
	case state != cases.StateOpen:
		return c, http.StatusNotFound, "no open case with that id"
	}
	return c, 0, ""
}

var errUnknownTicketDatabase = errors.New("unknown database")

// readCaseState reads one case; a case its database no longer reports is
// resolved.
func readCaseState(
	ctx context.Context, mgr *fleet.DatabaseManager, database, id string,
) (cases.State, cases.Case, error) {
	pools := poolsForDatabaseSelection(mgr, database)
	if len(pools) != 1 || pools[0].name != database {
		return "", cases.Case{}, errUnknownTicketDatabase
	}
	lookupCtx, cancel := context.WithTimeout(ctx, casesDatabaseTimeout)
	defer cancel()
	c, ok, err := lookupCase(lookupCtx, mgr, pools[0], id)
	if err != nil {
		return "", c, err
	}
	if !ok {
		return cases.StateResolved, c, nil
	}
	return c.State, c, nil
}

// writeCaseTicketError answers err without echoing a raw ticket-system
// error, which can carry the target's answer (G7-B10/B21).
func writeCaseTicketError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cases.ErrTicketInProgress):
		jsonError(w, "this case's ticket is being created; retry in a moment",
			http.StatusConflict)
	case errors.Is(err, cases.ErrTicketIntegrationMissing):
		jsonError(w, "no enabled Jira integration: add a Jira channel under Notifications",
			http.StatusConflict)
	case errors.Is(err, cases.ErrTicketInvalid):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, cases.ErrTicketUnauthorized):
		jsonError(w, "Jira refused the credentials (ticket_sync: unauthorized)",
			http.StatusBadGateway)
	default:
		slog.Error("creating case ticket", "error", notify.RedactError(err))
		jsonError(w, "could not create the Jira issue; see the sidecar log",
			http.StatusBadGateway)
	}
}

// channelGetter reads one notification channel with its secrets open.
type channelGetter interface {
	GetChannel(ctx context.Context, id int) (*notify.Channel, error)
}

// jiraTrackerFor opens the Jira integration of a channel id under the
// notification target policy.
func jiraTrackerFor(chans channelGetter, policy notify.TargetPolicy) cases.TrackerFunc {
	return func(ctx context.Context, id int) (cases.TicketTracker, error) {
		ch, err := chans.GetChannel(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("channel %d: %w", id, cases.ErrTicketIntegrationMissing)
		}
		if err != nil {
			return nil, fmt.Errorf("reading ticket integration %d: %w", id, err)
		}
		if ch.Type != cases.TicketSystemJira || !ch.Enabled {
			return nil, fmt.Errorf("channel %d: %w", id, cases.ErrTicketIntegrationMissing)
		}
		if err := policy.ValidateURL(ch.Config["base_url"]); err != nil {
			return nil, fmt.Errorf("%w: jira base_url: %v", cases.ErrTicketInvalid, err)
		}
		return cases.NewJiraClient(cases.JiraConfig{BaseURL: ch.Config["base_url"],
			Email: ch.Config["email"], APIToken: ch.Config["api_token"],
			ProjectKey: ch.Config["project_key"], IssueType: ch.Config["issue_type"]},
			policy.HTTPClient(10*time.Second))
	}
}

// jiraEnabled reports whether at least one Jira integration is enabled;
// a failed read reports none.
func jiraEnabled(ctx context.Context, chans channelLister) bool {
	listing, err := chans.ListChannels(ctx)
	if err != nil {
		slog.Warn("listing ticket integrations", "error", err)
		return false
	}
	_, err = pickJiraChannel(listing)
	return !errors.Is(err, cases.ErrTicketIntegrationMissing)
}

// pickJiraChannel is the one enabled Jira integration; with several the
// request must name one.
func pickJiraChannel(listing []store.ChannelListing) (int, error) {
	var ids []int
	for _, ch := range listing {
		if ch.Type == cases.TicketSystemJira && ch.Enabled {
			ids = append(ids, ch.ID)
		}
	}
	switch len(ids) {
	case 0:
		return 0, cases.ErrTicketIntegrationMissing
	case 1:
		return ids[0], nil
	}
	return 0, fmt.Errorf("%w: %d Jira integrations are enabled; name one with channel_id",
		cases.ErrTicketInvalid, len(ids))
}

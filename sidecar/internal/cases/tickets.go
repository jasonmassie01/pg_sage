package cases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Case tickets (market-14): an operator links a case to one ticket in a
// ticket system (Jira). The ticket's status is read back as evidence
// only: it never resolves a case, approves an action or changes trust.

// TicketSystemJira is the only ticket system today.
const TicketSystemJira = "jira"

var (
	// ErrTicketInvalid is a request or setting that cannot make a ticket.
	ErrTicketInvalid = errors.New("invalid ticket request")
	// ErrTicketUnauthorized is the ticket system refusing the credentials.
	ErrTicketUnauthorized = errors.New("ticket system refused the credentials")
	// ErrTicketNotFound is an issue the ticket system no longer has.
	ErrTicketNotFound = errors.New("issue not found in the ticket system")
	// ErrTicketIntegrationMissing is a deleted or disabled integration.
	ErrTicketIntegrationMissing = errors.New("ticket integration is missing or disabled")
	// ErrTicketInProgress is another request creating this case's ticket.
	ErrTicketInProgress = errors.New("this case's ticket is being created")
)

// Ticket is one case's link to an issue in a ticket system.
type Ticket struct {
	ID                int64      `json:"id"`
	Database          string     `json:"database"`
	CaseIdentityKey   string     `json:"case_identity_key"`
	System            string     `json:"system"`
	ChannelID         *int       `json:"channel_id,omitempty"`
	ExternalKey       string     `json:"external_key"`
	URL               string     `json:"url"`
	Status            string     `json:"status"`
	StatusCategory    string     `json:"status_category"`
	SyncError         string     `json:"sync_error,omitempty"`
	ResolvedCommentAt *time.Time `json:"resolved_comment_at,omitempty"`
	CreatedBy         string     `json:"created_by,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	LastSyncedAt      *time.Time `json:"last_synced_at,omitempty"`
}

// TicketClaim reserves a case's ticket before the issue is created.
type TicketClaim struct {
	Database, CaseIdentityKey, System string
	ChannelID                         int
	CreatedBy                         string
}

// TicketStore persists ticket links (sage.case_tickets).
type TicketStore interface {
	// Claim reserves the case's ticket; claimed is false when a ticket
	// (or a claim younger than the claim lease) already exists.
	Claim(ctx context.Context, c TicketClaim) (t Ticket, claimed bool, err error)
	Complete(ctx context.Context, id int64, ref IssueRef) (Ticket, error)
	Release(ctx context.Context, id int64) error
	List(ctx context.Context, database string) ([]Ticket, error)
	// DueForSync leases up to limit created tickets not synced within
	// minAge, oldest sync first.
	DueForSync(ctx context.Context, limit int, minAge time.Duration) ([]Ticket, error)
	RecordSync(ctx context.Context, id int64, st IssueStatus, syncErr string) error
	ClaimResolvedComment(ctx context.Context, id int64) (bool, error)
	ReleaseResolvedComment(ctx context.Context, id int64) error
}

// IssueRequest is the issue a case becomes.
type IssueRequest struct {
	Summary, Description, CaseURL string
}

// IssueRef names a created issue.
type IssueRef struct {
	Key, URL string
}

// IssueStatus is an issue's workflow status and its category (Jira:
// new, indeterminate or done).
type IssueStatus struct {
	Name, Category string
}

// TicketTracker is a ticket system.
type TicketTracker interface {
	CreateIssue(ctx context.Context, req IssueRequest) (IssueRef, error)
	IssueStatus(ctx context.Context, key string) (IssueStatus, error)
	AddComment(ctx context.Context, key, text string) error
}

// TrackerFunc opens the ticket system of an integration (a channel id).
type TrackerFunc func(ctx context.Context, channelID int) (TicketTracker, error)

// TicketService creates case tickets.
type TicketService struct {
	Store   TicketStore
	Tracker TrackerFunc
}

// TicketRequest asks for case c's ticket through integration ChannelID.
type TicketRequest struct {
	Case      Case
	ChannelID int
	CaseURL   string
	Actor     string
}

// Create makes the case's ticket once. When the case already has one it
// returns that ticket with created false; when another request is still
// creating it, ErrTicketInProgress.
func (s TicketService) Create(ctx context.Context, req TicketRequest) (Ticket, bool, error) {
	if req.Case.DatabaseName == "" || req.Case.IdentityKey == "" || req.ChannelID <= 0 {
		return Ticket{}, false, fmt.Errorf("%w: a ticket needs the case's database, "+
			"identity key and a ticket integration", ErrTicketInvalid)
	}
	tracker, err := s.Tracker(ctx, req.ChannelID)
	if err != nil {
		return Ticket{}, false, err
	}
	t, claimed, err := s.Store.Claim(ctx, TicketClaim{Database: req.Case.DatabaseName,
		CaseIdentityKey: req.Case.IdentityKey, System: TicketSystemJira,
		ChannelID: req.ChannelID, CreatedBy: req.Actor})
	if err != nil {
		return Ticket{}, false, fmt.Errorf("claiming the case ticket: %w", err)
	}
	if !claimed {
		if t.ExternalKey == "" {
			return t, false, ErrTicketInProgress
		}
		return t, false, nil
	}
	ref, err := tracker.CreateIssue(ctx, IssueRequest{Summary: ticketSummary(req.Case),
		Description: ticketDescription(req.Case), CaseURL: req.CaseURL})
	if err != nil {
		if rerr := s.Store.Release(ctx, t.ID); rerr != nil {
			err = errors.Join(err, fmt.Errorf("releasing the ticket claim: %w", rerr))
		}
		return Ticket{}, false, fmt.Errorf("creating the %s issue: %w", TicketSystemJira, err)
	}
	t, err = s.Store.Complete(ctx, t.ID, ref)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("recording issue %s: %w", ref.Key, err)
	}
	return t, true, nil
}

// jiraMaxSummary is Jira's summary limit.
const jiraMaxSummary = 255

func ticketSummary(c Case) string {
	s := "[pg_sage] " + c.DatabaseName + ": " + c.Title
	if len(s) <= jiraMaxSummary {
		return s
	}
	n := jiraMaxSummary
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func ticketDescription(c Case) string {
	lines := []string{
		c.Title,
		"Database: " + c.DatabaseName,
		"Severity: " + string(c.Severity),
		"Case: " + c.IdentityKey,
	}
	if c.Why != "" {
		lines = append(lines, "Why: "+c.Why)
	}
	if c.WhyNow != "" {
		lines = append(lines, "Why now: "+c.WhyNow)
	}
	lines = append(lines, "This issue is linked to a pg_sage case. Its status is shown on "+
		"the case; closing the issue does not resolve the case.")
	return strings.Join(lines, "\n")
}

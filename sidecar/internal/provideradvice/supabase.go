package provideradvice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Supabase performance advisors (the Management API behind the dashboard's
// Performance Advisor). Only the index lints map to pg_sage's findings:
// unindexed_foreign_keys, unused_index and duplicate_index. Read only.

// SupabaseOptions name the project and the Management API token (the one
// Supabase observability uses).
type SupabaseOptions struct {
	Project, Token string
	HTTPClient     *http.Client
	Endpoint       string // tests
	Now            func() time.Time
}

var (
	supabaseRef = regexp.MustCompile(`^[a-z0-9]{20}$`)
	// backtickName is the first `name` in a lint's detail text.
	backtickName = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_$]*)`")
)

// SupabaseSource reads one project's performance advisors.
type SupabaseSource struct{ opts SupabaseOptions }

// NewSupabaseSource validates the project and token; no network call.
func NewSupabaseSource(opts SupabaseOptions) (*SupabaseSource, error) {
	if !supabaseRef.MatchString(opts.Project) || strings.TrimSpace(opts.Token) == "" ||
		strings.ContainsAny(opts.Token, "\r\n") {
		return nil, errors.New("supabase advisors need a project ref and a Management API token")
	}
	if opts.Endpoint == "" {
		opts.Endpoint = "https://api.supabase.com"
	}
	endpoint, err := checkEndpoint(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("supabase advisors: %w", err)
	}
	opts.Endpoint, opts.HTTPClient = endpoint, newHTTPClient(opts.HTTPClient)
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &SupabaseSource{opts: opts}, nil
}

// Provider is the canonical provider name.
func (s *SupabaseSource) Provider() string { return "supabase" }

type supabaseLint struct {
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	CacheKey string `json:"cache_key"`
	Metadata struct {
		Schema  string   `json:"schema"`
		Name    string   `json:"name"`
		FKey    string   `json:"fkey_name"`
		Indexes []string `json:"indexes"`
	} `json:"metadata"`
}

// Fetch reads the performance lints and maps the index ones.
func (s *SupabaseSource) Fetch(ctx context.Context) ([]Recommendation, error) {
	u := s.opts.Endpoint + "/v1/projects/" + s.opts.Project + "/advisors/performance"
	raw, err := getJSON(ctx, s.opts.HTTPClient, u, s.opts.Token, "supabase advisors")
	if err != nil {
		return nil, err
	}
	var body struct {
		Lints []supabaseLint `json:"lints"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%w: supabase advisors response: %s", ErrMalformed,
			clip(err.Error()))
	}
	now := s.opts.Now().UTC()
	var out []Recommendation
	for _, l := range body.Lints {
		if rec, ok := s.convert(l); ok {
			rec.ObservedAt = now
			out = append(out, rec)
		}
	}
	return out, nil
}

// convert maps an index lint; any other lint is not index advice. An index
// lint missing the names pg_sage matches on is shown as unknown.
func (s *SupabaseSource) convert(l supabaseLint) (Recommendation, bool) {
	md := l.Metadata
	rec := Recommendation{Provider: s.Provider(), ProviderID: l.CacheKey,
		Reason: clip(l.Detail), Object: md.Schema + "." + md.Name}
	if rec.ProviderID == "" {
		rec.ProviderID = l.Name + ":" + rec.Object
	}
	switch l.Name {
	case "unindexed_foreign_keys":
		rec.Kind, rec.Constraint = KindCreateIndex, md.FKey
		if md.FKey == "" {
			return unknownLint(rec, "names no foreign key"), true
		}
	case "unused_index":
		m := backtickName.FindStringSubmatch(l.Detail)
		if m == nil || md.Schema == "" {
			return unknownLint(rec, "names no index"), true
		}
		rec.Kind, rec.Object = KindDropIndex, md.Schema+"."+m[1]
	case "duplicate_index":
		if len(md.Indexes) < 2 || md.Schema == "" {
			return unknownLint(rec, "names no duplicate set"), true
		}
		rec.Kind = KindDropIndex
		for _, ix := range md.Indexes {
			rec.Indexes = append(rec.Indexes, md.Schema+"."+ix)
		}
	default:
		return Recommendation{}, false
	}
	return rec, true
}

func unknownLint(rec Recommendation, why string) Recommendation {
	rec.Kind, rec.Constraint = KindUnknown, ""
	rec.Reason = "supabase lint " + why + ": " + rec.Reason
	return rec
}

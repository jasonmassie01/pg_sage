package provideradvice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Azure Database for PostgreSQL flexible server index tuning: the ARM list
// behind `az postgres flexible-server index-tuning list-recommendations`.
// Read only; the credential is the ARM token chain pg_sage already uses.

// azureAPIVersion is pinned; the live check (docs/azure.md) confirms it.
const azureAPIVersion = "2024-11-01-preview"

// maxAzurePages bounds how many nextLink pages one fetch follows.
const maxAzurePages = 5

// AzureOptions identify one flexible server and the database whose advice
// is read (index tuning lists every database of the server).
type AzureOptions struct {
	SubscriptionID, ResourceGroup, Server, Database string
	Token                                           func(ctx context.Context) (string, error)
	HTTPClient                                      *http.Client
	Endpoint                                        string // ARM; tests and sovereign clouds
	Now                                             func() time.Time
}

var (
	azureSubscription = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	azureGroup  = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	azureServer = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// AzureSource reads index-tuning recommendations of one database.
type AzureSource struct{ opts AzureOptions }

// NewAzureSource validates the identity; it makes no network call.
func NewAzureSource(opts AzureOptions) (*AzureSource, error) {
	opts.SubscriptionID = strings.ToLower(opts.SubscriptionID)
	switch {
	case !azureSubscription.MatchString(opts.SubscriptionID):
		return nil, fmt.Errorf("azure index tuning: invalid subscription %q", opts.SubscriptionID)
	case !azureGroup.MatchString(opts.ResourceGroup) || strings.Contains(opts.ResourceGroup, ".."):
		return nil, fmt.Errorf("azure index tuning: invalid resource group %q", opts.ResourceGroup)
	case !azureServer.MatchString(opts.Server):
		return nil, fmt.Errorf("azure index tuning: invalid flexible server name %q", opts.Server)
	case strings.TrimSpace(opts.Database) == "":
		return nil, errors.New("azure index tuning: the database name is required")
	case opts.Token == nil:
		return nil, fmt.Errorf("%w: azure index tuning has no ARM token source", ErrAuth)
	}
	if opts.Endpoint == "" {
		opts.Endpoint = "https://management.azure.com"
	}
	endpoint, err := checkEndpoint(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("azure index tuning: %w", err)
	}
	opts.Endpoint, opts.HTTPClient = endpoint, newHTTPClient(opts.HTTPClient)
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &AzureSource{opts: opts}, nil
}

// Provider is the canonical provider name.
func (s *AzureSource) Provider() string { return "azure" }

type azureList struct {
	Value    []azureRecommendation `json:"value"`
	NextLink string                `json:"nextLink"`
}

type azureRecommendation struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		Type           string    `json:"recommendationType"`
		Reason         string    `json:"recommendationReason"`
		LastRecommend  time.Time `json:"lastRecommendedTime"`
		Implementation struct {
			Script string `json:"script"`
		} `json:"implementationDetails"`
		Details struct {
			Database string `json:"databaseName"`
			Schema   string `json:"schema"`
			Table    string `json:"table"`
			Index    string `json:"indexName"`
		} `json:"details"`
	} `json:"properties"`
}

// Fetch lists the recommendations of the configured database, following
// nextLink pages on the ARM host only, at most maxAzurePages.
func (s *AzureSource) Fetch(ctx context.Context) ([]Recommendation, error) {
	token, err := s.opts.Token(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: azure credential: %s", ErrAuth, clip(err.Error()))
	}
	next := s.opts.Endpoint + s.listPath() + "?api-version=" + azureAPIVersion
	var out []Recommendation
	for page := 0; next != ""; page++ {
		if page == maxAzurePages {
			return nil, fmt.Errorf("%w: azure index tuning lists more than %d pages",
				ErrProvider, maxAzurePages)
		}
		if err := s.sameHost(next); err != nil {
			return nil, err
		}
		list, err := s.page(ctx, next, token)
		if err != nil {
			return nil, err
		}
		out = append(out, s.convert(list.Value)...)
		next = list.NextLink
	}
	return out, nil
}

func (s *AzureSource) listPath() string {
	return "/subscriptions/" + url.PathEscape(s.opts.SubscriptionID) + "/resourceGroups/" +
		url.PathEscape(s.opts.ResourceGroup) +
		"/providers/Microsoft.DBforPostgreSQL/flexibleServers/" +
		url.PathEscape(s.opts.Server) + "/tuningOptions/index/recommendations"
}

func (s *AzureSource) sameHost(raw string) error {
	u, err := url.Parse(raw)
	base, _ := url.Parse(s.opts.Endpoint) // validated by NewAzureSource
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host {
		return fmt.Errorf("%w: azure index tuning nextLink leaves %s", ErrProvider, base.Host)
	}
	return nil
}

func (s *AzureSource) page(ctx context.Context, rawURL, token string) (azureList, error) {
	raw, err := getJSON(ctx, s.opts.HTTPClient, rawURL, token, "azure index tuning")
	if err != nil {
		return azureList{}, err
	}
	var list azureList
	if err := json.Unmarshal(raw, &list); err != nil {
		return azureList{}, fmt.Errorf("%w: azure index tuning response: %s", ErrMalformed,
			clip(err.Error()))
	}
	return list, nil
}

// convert keeps this database's recommendations; one without an id is
// dropped (it could not be tracked), an unknown type is shown as unknown.
func (s *AzureSource) convert(in []azureRecommendation) []Recommendation {
	var out []Recommendation
	for _, r := range in {
		p := r.Properties
		id := r.Name
		if id == "" {
			id = r.ID
		}
		if id == "" || (p.Details.Database != "" && p.Details.Database != s.opts.Database) {
			continue
		}
		rec := Recommendation{Provider: s.Provider(), ProviderID: id, DDL: p.Implementation.Script,
			Reason: clip(p.Reason), ObservedAt: p.LastRecommend.UTC()}
		if rec.ObservedAt.IsZero() {
			rec.ObservedAt = s.opts.Now().UTC()
		}
		switch p.Type {
		case "CreateIndex":
			rec.Kind, rec.Object = KindCreateIndex, p.Details.Schema+"."+p.Details.Table
		case "DropIndex":
			rec.Kind, rec.Object = KindDropIndex, p.Details.Schema+"."+p.Details.Index
		default:
			rec.Kind, rec.DDL = KindUnknown, ""
			rec.Reason = "azure recommendation type " + clip(p.Type) + " is not read: " + rec.Reason
		}
		out = append(out, rec)
	}
	return out
}

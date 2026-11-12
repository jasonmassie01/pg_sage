// Package cnpg reads one CloudNativePG Cluster from the Kubernetes API
// (market-12). It is the read-only managedparam.Resolver of a database
// that runs on CNPG: a GET of the named Cluster with the pod's
// service-account token gives spec.postgresql.parameters, which the
// managed-change worker uses for proposals, the "now running" check and
// drift. pg_sage never writes the Cluster; the chart's Role grants only
// get on the named Clusters.
package cnpg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

const (
	// RequestTimeout bounds one read of the Cluster.
	RequestTimeout = 5 * time.Second
	// DefaultTokenFile and DefaultCAFile are the service-account mounts.
	DefaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	// maxBody bounds the Cluster object read.
	maxBody = 1 << 20
)

var (
	labelPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	parameterPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
)

// Cluster names one CNPG Cluster.
type Cluster struct {
	Name      string
	Namespace string
}

func (c Cluster) String() string { return c.Namespace + "/" + c.Name }

// Source reads the Cluster. It is safe for concurrent use.
type Source struct {
	cluster   Cluster
	baseURL   string
	tokenFile string
	caFile    string
	client    *http.Client
	timeout   time.Duration
}

// Option configures a Source.
type Option func(*Source)

// WithBaseURL points the source at another API server (tests, an
// out-of-cluster proxy). Default: https://$KUBERNETES_SERVICE_HOST:PORT.
func WithBaseURL(url string) Option {
	return func(s *Source) { s.baseURL = strings.TrimRight(url, "/") }
}

// WithTokenFile reads the bearer token from path (re-read on every call).
func WithTokenFile(path string) Option { return func(s *Source) { s.tokenFile = path } }

// WithHTTPClient replaces the egress client that trusts the
// service-account CA (tests).
func WithHTTPClient(c *http.Client) Option { return func(s *Source) { s.client = c } }

// WithTimeout replaces RequestTimeout.
func WithTimeout(d time.Duration) Option { return func(s *Source) { s.timeout = d } }

// NewSource validates the Cluster name and returns its source.
func NewSource(c Cluster, opts ...Option) (*Source, error) {
	for _, f := range [][2]string{{"cluster", c.Name}, {"namespace", c.Namespace}} {
		if !labelPattern.MatchString(f[1]) {
			return nil, fmt.Errorf("cnpg: invalid %s name %q (want an RFC 1123 label)",
				f[0], f[1])
		}
	}
	s := &Source{cluster: c, tokenFile: DefaultTokenFile, caFile: DefaultCAFile,
		timeout: RequestTimeout}
	for _, opt := range opts {
		opt(s)
	}
	if s.baseURL == "" {
		base, err := inClusterURL()
		if err != nil {
			return nil, err
		}
		s.baseURL = base
	}
	if s.client == nil {
		client, err := caClient(s.caFile, s.timeout)
		if err != nil {
			return nil, err
		}
		s.client = client
	}
	return s, nil
}

func inClusterURL() (string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return "", errors.New("cnpg: KUBERNETES_SERVICE_HOST is unset, so pg_sage is not " +
			"running in a Kubernetes pod and cannot read the Cluster; proposals use " +
			"placeholders")
	}
	if port == "" {
		port = "443"
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

// caClient is the egress client that trusts only the cluster CA.
func caClient(caFile string, timeout time.Duration) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("cnpg: read the service-account CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("cnpg: %s holds no PEM certificate", caFile)
	}
	transport := egress.Transport(timeout)
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	client := egress.HTTPClient(timeout)
	client.Transport = transport
	return client, nil
}

type clusterObject struct {
	Spec struct {
		PrimaryUpdateStrategy string `json:"primaryUpdateStrategy"`
		PostgreSQL            struct {
			Parameters map[string]string `json:"parameters"`
		} `json:"postgresql"`
	} `json:"spec"`
}

// Target reads the Cluster's parameters.
func (s *Source) Target(ctx context.Context) (managedparam.Target, error) {
	if err := ctx.Err(); err != nil {
		return managedparam.Target{}, err
	}
	raw, err := s.get(ctx)
	if err != nil {
		return managedparam.Target{}, err
	}
	var obj clusterObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return managedparam.Target{}, fmt.Errorf("cnpg: decode Cluster %s: %w", s.cluster, err)
	}
	flags := make(map[string]string, len(obj.Spec.PostgreSQL.Parameters))
	for k, v := range obj.Spec.PostgreSQL.Parameters {
		if !parameterPattern.MatchString(k) {
			return managedparam.Target{}, fmt.Errorf("cnpg: Cluster %s has a parameter "+
				"named %.64q, which is not a PostgreSQL setting name", s.cluster, k)
		}
		flags[k] = v
	}
	return managedparam.Target{Provider: "cnpg", Cluster: s.cluster.Name,
		Namespace: s.cluster.Namespace, UpdateStrategy: obj.Spec.PrimaryUpdateStrategy,
		Flags: flags, FlagsKnown: true, Known: true}, nil
}

// get sends the one bounded, authenticated GET of the Cluster.
func (s *Source) get(ctx context.Context) ([]byte, error) {
	token, err := os.ReadFile(s.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("cnpg: read the service-account token %s: %w", s.tokenFile, err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	url := fmt.Sprintf("%s/apis/postgresql.cnpg.io/v1/namespaces/%s/clusters/%s",
		s.baseURL, s.cluster.Namespace, s.cluster.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cnpg: build the Cluster request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("cnpg: reading Cluster %s timed out after %s", s.cluster,
				s.timeout)
		}
		return nil, fmt.Errorf("cnpg: read Cluster %s: %w", s.cluster, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("cnpg: read Cluster %s: %w", s.cluster, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, s.statusError(resp.StatusCode, raw)
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("cnpg: Cluster %s is larger than %d bytes", s.cluster, maxBody)
	}
	return raw, nil
}

type apiStatus struct {
	Message string `json:"message"`
}

// statusError says what to fix for each refusal.
func (s *Source) statusError(code int, raw []byte) error {
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("cnpg: the Kubernetes API refused the service-account token (401) "+
			"reading Cluster %s: check the pod's token mount", s.cluster)
	case http.StatusForbidden:
		return fmt.Errorf("cnpg: service account lacks get on clusters.postgresql.cnpg.io "+
			"(403 reading %s): install the chart's Role with this Cluster in "+
			"cnpg.clusters, or grant get on it", s.cluster)
	case http.StatusNotFound:
		return fmt.Errorf("cnpg: Cluster %s not found (404): check databases[].cnpg "+
			"cluster and namespace, and that the CloudNativePG CRDs are installed", s.cluster)
	}
	msg := strings.TrimSpace(string(raw))
	var st apiStatus
	if json.Unmarshal(raw, &st) == nil && st.Message != "" {
		msg = st.Message
	}
	if len(msg) > 256 {
		msg = msg[:256]
	}
	return fmt.Errorf("cnpg: Kubernetes API %d reading Cluster %s: %s", code, s.cluster, msg)
}

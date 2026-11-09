package egress

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEgressMetadataClientReachesConfiguredHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = w.Write([]byte("project-1"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	resp, err := MetadataClient(host, 2*time.Second).Get(srv.URL + "/computeMetadata/v1/")
	if err != nil {
		t.Fatalf("metadata client to configured host: %v", err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Metadata-Flavor") != "Google" {
		t.Fatalf("unexpected response headers %v", resp.Header)
	}
}

func TestEgressMetadataClientRefusesOtherHosts(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit = true
	}))
	defer srv.Close()
	client := MetadataClient("metadata.google.internal", 2*time.Second)
	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) || hit {
		t.Fatalf("metadata client to another host = %v (hit=%v), want ErrBlocked", err, hit)
	}
}

func TestEgressMetadataClientRefusesRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	resp, err := MetadataClient(host, 2*time.Second).Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("metadata redirect = %v, want ErrBlocked", err)
	}
}

func TestEgressMetadataControlAllowsOnlyLinkLocalExtra(t *testing.T) {
	if err := metadataControl("tcp", "169.254.169.254:80", nil); err != nil {
		t.Fatalf("metadata control refused the metadata address: %v", err)
	}
	for _, address := range []string{"224.0.0.1:80", "0.0.0.0:80", "bad"} {
		if err := metadataControl("tcp", address, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("metadata control(%s) = %v, want ErrBlocked", address, err)
		}
	}
}

func TestEgressMetadataClientEmptyHost(t *testing.T) {
	resp, err := MetadataClient("", time.Second).Get("http://169.254.169.254/")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("empty metadata host = %v, want ErrBlocked", err)
	}
}

func TestEgressSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://h/v1", "https://h/v2/chat", true},
		{"https://h", "https://h:443", true},
		{"http://h", "http://h:80/x", true},
		{"https://H.example.", "https://h.example/v1", true},
		{"https://[::1]:8443/a", "https://[::1]:8443/b", true},
		{"http://h", "https://h", false},
		{"https://h:8443", "https://h", false},
		{"https://h/v1", "https://other.example/v1", false},
		{"", "https://h", false},
		{"", "", false},
		{"https://h", "::not a url", false},
		{"h/v1", "h/v1", false}, // no scheme, no host
		{"ftp://h", "ftp://h", true},
	}
	for _, c := range cases {
		if got := SameOrigin(c.a, c.b); got != c.want {
			t.Errorf("SameOrigin(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

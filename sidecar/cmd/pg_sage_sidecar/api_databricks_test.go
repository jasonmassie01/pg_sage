package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/net/http2"
)

func TestDatabricksAPIProtocols(t *testing.T) {
	t.Setenv("DATABRICKS_APP_PORT", "")
	if apiHTTPProtocols() != nil {
		t.Fatal("non-Apps deployment should retain standard Go HTTP defaults")
	}
	t.Setenv("DATABRICKS_APP_PORT", "8000")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	server.Config.Protocols = apiHTTPProtocols()
	server.Start()
	defer server.Close()

	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	defer transport.CloseIdleConnections()
	for _, tc := range []struct {
		name   string
		client *http.Client
		want   string
	}{
		{"http1", server.Client(), "HTTP/1.1"},
		{"h2c", &http.Client{Transport: transport}, "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := tc.client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != tc.want {
				t.Fatalf("protocol body = %q, err = %v; want %s", body, err, tc.want)
			}
		})
	}
}

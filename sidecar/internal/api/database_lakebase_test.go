package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPreviewConnectionPinsIPAndRetainsOAuthHostAndTLSName(t *testing.T) {
	const host, ip = "endpoint.example.com", "198.51.100.42"
	for _, ssl := range []string{"require", "verify-full"} {
		t.Run(ssl, func(t *testing.T) {
			req := dbCreateRequest{Host: host, Username: "test", DatabaseName: "test", SSLMode: ssl}
			var signed bool
			cfg, err := sageConnectionConfig(t.Context(), buildManagedTestURL(req, "bootstrap").String(), ip,
				func(_ context.Context, cfg *pgx.ConnConfig) error {
					signed = true
					if cfg.Host != host || cfg.TLSConfig == nil || cfg.TLSConfig.ServerName != host {
						t.Fatal("OAuth validation lost the original endpoint/TLS hostname")
					}
					cfg.Password = "synthetic-native-token"
					return nil
				})
			if err != nil || !signed || cfg.Password != "synthetic-native-token" {
				t.Fatalf("native preview configuration failed: %v", err)
			}
			addresses, err := cfg.LookupFunc(t.Context(), host)
			if err != nil || len(addresses) != 1 || addresses[0] != ip {
				t.Fatalf("lookup escaped the validated IP: %v, %v", addresses, err)
			}
			if _, err := cfg.LookupFunc(t.Context(), "different.example.com"); err == nil {
				t.Fatal("pinned connection accepted another hostname")
			}
			if cfg.TLSConfig.ServerName != host || cfg.Host != host {
				t.Fatal("IP pinning changed TLS SNI or the authorized endpoint")
			}
		})
	}
}

func TestPreviewConnectionRejectsUnsafePinBeforeAcquiringCredentials(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "not-an-ip"} {
		t.Run(ip, func(t *testing.T) {
			_, err := sageConnectionConfig(t.Context(), "postgres://test@endpoint.example.com/test?sslmode=require", ip,
				func(context.Context, *pgx.ConnConfig) error {
					t.Fatal("unsafe preview acquired credentials")
					return nil
				})
			if err == nil {
				t.Fatal("unsafe IP pin accepted")
			}
		})
	}
}

func TestPreviewConnectionStopsOnOAuthFailure(t *testing.T) {
	denied := errors.New("synthetic authorization denial")
	if _, err := sageConnectionConfig(t.Context(), "postgres://test@endpoint.example.com/test?sslmode=require", "198.51.100.42",
		func(context.Context, *pgx.ConnConfig) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("native authorization error was lost: %v", err)
	}
}

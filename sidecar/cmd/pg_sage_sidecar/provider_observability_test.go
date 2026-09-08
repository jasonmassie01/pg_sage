package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestSupabaseObservabilityProjectIdentity(t *testing.T) {
	cases := []struct{ host, user, want string }{
		{"db.abcdefghijklmnopqrst.supabase.co", "postgres", "abcdefghijklmnopqrst"},
		{"DB.ABCDEFGHIJKLMNOPQRST.SUPABASE.CO.", "postgres", "abcdefghijklmnopqrst"},
		{"aws-0-us-east-1.pooler.supabase.com", "postgres.abcdefghijklmnopqrst", "abcdefghijklmnopqrst"},
		{"aws-0-us-east-1.pooler.supabase.com", "worker_role.abcdefghijklmnopqrst",
			"abcdefghijklmnopqrst"},
		{"aws-0-us-east-1.pooler.supabase.com", "postgres", ""},
		{"db.abcdefghijklmnopqrst.supabase.co.evil.test", "postgres", ""},
		{"db.abcdefghijklmnopqrst.supabase.co:5432", "postgres", ""},
		{"custom.example", "postgres.abcdefghijklmnopqrst", ""},
		{"db.short.supabase.co", "postgres", ""},
		{"db.abcdefghijklmnopqrst.extra.supabase.co", "postgres", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := supabaseObservabilityProject(tc.host, tc.user); got != tc.want {
			t.Errorf("project for %q = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestBuildProviderObservabilityConfiguration(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, tc := range []struct {
		host, token      string
		enabled, wantErr bool
	}{
		{"localhost", "test-token", false, false},
		{"db.abcdefghijklmnopqrst.supabase.co", "", false, false},
		{"db.abcdefghijklmnopqrst.supabase.co", "test-token", true, false},
		{"db.abcdefghijklmnopqrst.supabase.co", "private\ninvalid", false, true},
	} {
		pool, err := pgxpool.New(t.Context(), "postgres://postgres@"+tc.host+"/postgres")
		if err != nil {
			t.Fatal(err)
		}
		runtime, sink, err := buildProviderObservability(pool, cfg, tc.token)
		pool.Close()
		if (err != nil) != tc.wantErr || (runtime != nil) != tc.enabled ||
			(sink != nil) != tc.enabled {
			t.Fatalf("configuration: runtime=%v sink=%v err=%v", runtime != nil, sink != nil, err)
		}
		if err != nil && strings.Contains(err.Error(), "private") {
			t.Fatal("token leaked")
		}
	}
	runtime, sink, err := buildProviderObservability(nil, nil, "")
	if runtime != nil || sink != nil || err != nil {
		t.Fatal("nil configuration must be disabled")
	}
}

func TestProviderObservabilityWorkerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	startInstanceWorker(&workers, func() { <-ctx.Done(); close(stopped) })
	cancel()
	if !waitProviderWorkers(t.Context(), &workers) {
		t.Fatal("worker did not drain")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("wait returned before worker stopped")
	}
	workers.Add(1)
	deadline, stop := context.WithTimeout(t.Context(), time.Millisecond)
	defer stop()
	if waitProviderWorkers(deadline, &workers) {
		t.Fatal("unfinished worker reported drained")
	}
	workers.Done()
}

// HTTP authentication, outages, log isolation and live observations are tested in providerobs.

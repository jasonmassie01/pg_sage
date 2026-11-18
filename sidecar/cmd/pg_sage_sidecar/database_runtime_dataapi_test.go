package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// fleetpol-02c wiring: a Supabase database whose endpoint names its
// project reads the Data API exposure with the Management API token;
// anything else gets no reader, and posture sees unknown (nil).

const dataAPITestRef = "abcdefghijklmnopqrst"

func TestSupabaseDataAPIReader_SelectsOnlyANamedSupabaseProject(t *testing.T) {
	host := "db." + dataAPITestRef + ".supabase.co"
	cases := []struct {
		name, provider, host, user, token string
		want                              bool
		note                              string
	}{
		{"supabase with token", "supabase", host, "postgres", "sbp_x", true, ""},
		{"pooler names the project in the user", "supabase",
			"aws-0-us-east-1.pooler.supabase.com", "postgres." + dataAPITestRef, "sbp_x",
			true, ""},
		{"no token", "supabase", host, "postgres", "", false,
			"SAGE_SUPABASE_OBSERVABILITY_TOKEN"},
		{"blank token", "supabase", host, "postgres", "  ", false,
			"SAGE_SUPABASE_OBSERVABILITY_TOKEN"},
		{"custom domain names no project", "supabase", "db.example.com", "postgres",
			"sbp_x", false, "does not name the project"},
		{"not supabase", "rds", host, "postgres", "sbp_x", false, ""},
		{"self-managed", "self-managed", "localhost", "postgres", "sbp_x", false, ""},
	}
	for _, c := range cases {
		r, note := supabaseDataAPIReader(c.provider, c.host, c.user, c.token)
		if (r != nil) != c.want {
			t.Fatalf("%s: reader = %v, want reader %v", c.name, r, c.want)
		}
		if c.note == "" && note != "" || !strings.Contains(note, c.note) {
			t.Fatalf("%s: note = %q, want %q", c.name, note, c.note)
		}
		if strings.Contains(note, "sbp_x") {
			t.Fatalf("%s: note %q leaks the token", c.name, note)
		}
	}
}

// Nil/empty: no reader and an unread reader are unknown; a read exposure
// is copied into posture's platform.
func TestPostureDataAPI_MapsTheReader(t *testing.T) {
	if postureDataAPI(nil) != nil {
		t.Fatal("no reader must be unknown")
	}
	api := &fixedSupabaseAPI{schemas: []string{"public", "api"}}
	r := cloudtel.NewSupabaseDataAPI(api, cloudtel.SupabaseDataAPIOptions{
		Now: func() time.Time { return time.Unix(0, 0) }})
	if postureDataAPI(r) != nil {
		t.Fatal("an unread reader must be unknown")
	}
	r.Refresh(context.Background())
	got := postureDataAPI(r)
	if got == nil || !slices.Equal(got.Schemas, []string{"public", "api"}) {
		t.Fatalf("mapped exposure = %+v", got)
	}
}

type fixedSupabaseAPI struct{ schemas []string }

func (f *fixedSupabaseAPI) DataAPISettings(context.Context) (
	cloudtel.SupabaseDataAPISettings, error) {
	return cloudtel.SupabaseDataAPISettings{Schemas: slices.Clone(f.schemas)}, nil
}

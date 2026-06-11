package selfmonitor

import "testing"

func TestIsQueryTextDetectsPgSageSelfQueries(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{
			name:  "snapshots select",
			query: "SELECT data FROM sage.snapshots WHERE category = $1",
			want:  true,
		},
		{
			name:  "quoted sage schema",
			query: `select * from "sage"."findings" where status = $1`,
			want:  true,
		},
		{
			name:  "application name mention",
			query: "/* pg_sage */ SELECT 1",
			want:  true,
		},
		{
			name:  "word containing sage is not schema",
			query: "SELECT * FROM public.usages WHERE id = $1",
			want:  false,
		},
		{
			name:  "normal app table",
			query: "SELECT * FROM public.orders WHERE id = $1",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsQueryText(tt.query); got != tt.want {
				t.Fatalf("IsQueryText() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsFindingDetectsSelfMonitoringFields(t *testing.T) {
	tests := []struct {
		name string
		in   FindingFields
		want bool
	}{
		{
			name: "object in sage schema",
			in: FindingFields{
				ObjectIdentifier: "sage.snapshots",
			},
			want: true,
		},
		{
			name: "query detail references sage schema",
			in: FindingFields{
				ObjectIdentifier: "queryid:42",
				Detail: map[string]any{
					"query": "SELECT count(*) FROM sage.findings",
				},
			},
			want: true,
		},
		{
			name: "recommended SQL references app table",
			in: FindingFields{
				ObjectIdentifier: "public.orders",
				RecommendedSQL:   "CREATE INDEX ON public.orders (created_at)",
			},
			want: false,
		},
		{
			name: "query detail references app sage-like word",
			in: FindingFields{
				ObjectIdentifier: "queryid:43",
				Detail: map[string]any{
					"query": "SELECT * FROM public.usages",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFinding(tt.in); got != tt.want {
				t.Fatalf("IsFinding() = %v, want %v", got, tt.want)
			}
		})
	}
}

package tuner

import (
	"strings"
	"testing"
)

func TestTunerSystemPrompt_ContainsRules(t *testing.T) {
	prompt := TunerSystemPrompt()
	required := []string{
		"JSON array",
		"pg_hint_plan",
		"HashJoin", "MergeJoin", "NestLoop",
		"IndexScan",
		"work_mem",
		"confidence",
	}
	for _, kw := range required {
		if !strings.Contains(prompt, kw) {
			t.Errorf("system prompt missing %q", kw)
		}
	}
}

func TestFormatTunerPrompt_IncludesAllSections(t *testing.T) {
	qctx := QueryContext{
		Candidate: candidate{
			QueryID:         12345,
			Query:           "SELECT * FROM orders o JOIN items i ON o.id = i.order_id",
			Calls:           100,
			MeanExecTime:    450.5,
			TempBlksWritten: 500,
			MeanPlanTime:    10.2,
		},
		PlanJSON: `[{"Plan": {"Node Type": "Seq Scan"}}]`,
		Symptoms: []PlanSymptom{
			{Kind: SymptomDiskSort, Detail: map[string]any{
				"sort_space_kb": 4096,
			}},
		},
		FallbackHints: `Set(work_mem "128MB")`,
		Tables: []TableDetail{
			{
				Schema: "public", Name: "orders",
				LiveTuples: 50000, DeadTuples: 100,
				Columns: []ColumnInfo{
					{Name: "id", Type: "bigint"},
					{Name: "status", Type: "text", IsNullable: true},
				},
				Indexes: []IndexDetail{
					{Name: "orders_pkey", Definition: "CREATE UNIQUE INDEX orders_pkey ON orders (id)", Scans: 5000, IsUnique: true},
				},
				ColStats: []ColStatInfo{
					{Column: "id", NDistinct: -1.0, Correlation: 0.99},
				},
			},
		},
		System: SystemContext{
			ActiveBackends: 15,
			MaxConnections: 100,
			WorkMem:        "4MB",
			SharedBuffers:  "2GB",
			EffCacheSize:   "6GB",
			MaxParallelPG:  2,
		},
	}

	prompt := FormatTunerPrompt(qctx)

	checks := []struct {
		name   string
		substr string
	}{
		{"queryid", "queryid: 12345"},
		{"query text", "SELECT * FROM orders"},
		{"mean exec", "450.5ms"},
		{"symptoms", "disk_sort"},
		{"plan", "Seq Scan"},
		{"table name", "public.orders"},
		{"live tuples", "50000"},
		{"column", "bigint"},
		{"index", "orders_pkey"},
		{"col stats", "n_distinct"},
		{"system", "active_backends=15"},
		{"work_mem", "work_mem=4MB"},
		{"fallback", `Set(work_mem "128MB")`},
		{"respond", "RESPOND NOW"},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(prompt, c.substr) {
				t.Errorf("prompt missing %q (%s)", c.substr, c.name)
			}
		})
	}
}

func TestFormatTunerPrompt_NoStatsGraceful(t *testing.T) {
	qctx := QueryContext{
		Candidate: candidate{
			QueryID: 1, Query: "SELECT 1", Calls: 1,
			MeanExecTime: 1.0,
		},
		Symptoms: []PlanSymptom{
			{Kind: SymptomHighPlanTime},
		},
	}
	prompt := FormatTunerPrompt(qctx)
	if prompt == "" {
		t.Error("expected non-empty prompt")
	}
	if !strings.Contains(prompt, "high_plan_time") {
		t.Error("missing symptom in prompt")
	}
}

func TestFormatTunerPrompt_IncludesSpecializedWorkloadHints(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name: "jsonb containment",
			query: "SELECT * FROM events WHERE payload @> " +
				"'{\"tier\":\"pro\"}'::jsonb",
			want: []string{
				"Specialized Workload Hints",
				"JSON/JSONB workload",
				"gin_jsonb_path_ops",
			},
		},
		{
			name:  "vector missing limit",
			query: "SELECT id FROM docs ORDER BY embedding <-> $1",
			want: []string{
				"Vector workload",
				"missing_limit",
				"add_limit",
			},
		},
		{
			name: "postgis distance",
			query: "SELECT * FROM places WHERE " +
				"ST_DWithin(geom, ST_SetSRID(ST_Point($1,$2),4326), 1000)",
			want: []string{
				"PostGIS workload",
				"gist_spatial_index",
				"distance_filter",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qctx := QueryContext{
				Candidate: candidate{
					QueryID:      99,
					Query:        tt.query,
					Calls:        12,
					MeanExecTime: 33.3,
				},
			}

			prompt := FormatTunerPrompt(qctx)

			for _, want := range tt.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
		})
	}
}

func TestFormatTunerPrompt_Truncation(t *testing.T) {
	// Build a large plan that exceeds maxPlanJSONChars
	largePlan := strings.Repeat("x", 5000)
	qctx := QueryContext{
		Candidate: candidate{
			QueryID: 1, Query: "SELECT 1", Calls: 1,
			MeanExecTime: 1.0,
		},
		PlanJSON: largePlan,
		Symptoms: []PlanSymptom{
			{Kind: SymptomDiskSort},
		},
	}
	prompt := FormatTunerPrompt(qctx)
	if !strings.Contains(prompt, "truncated") {
		t.Error("expected truncation marker in prompt")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tc := range cases {
		got := humanBytes(tc.input)
		if got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q",
				tc.input, got, tc.want)
		}
	}
}

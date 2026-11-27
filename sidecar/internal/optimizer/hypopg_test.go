package optimizer

import "testing"

func TestExtractTotalCost(t *testing.T) {
	tests := []struct {
		name string
		json string
		want float64
	}{
		{
			name: "valid plan",
			json: `[{"Plan":{"Total Cost":42.5}}]`,
			want: 42.5,
		},
		{
			name: "invalid JSON",
			json: `not json`,
			want: 0,
		},
		{
			name: "empty array",
			json: `[]`,
			want: 0,
		},
		{
			name: "zero cost",
			json: `[{"Plan":{"Total Cost":0}}]`,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTotalCost([]byte(tt.json))
			if got != tt.want {
				t.Errorf("extractTotalCost() = %v, want %v", got, tt.want)
			}
		})
	}
}

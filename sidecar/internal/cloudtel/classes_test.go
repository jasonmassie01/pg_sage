package cloudtel

import "testing"

// The instance-class table is the documented RAM of each class; an
// unknown class is unknown (0), never a guess.
func TestInstanceClassMemory(t *testing.T) {
	cases := map[string]float64{
		"db.t3.micro":     1 * gib,
		"db.t4g.medium":   4 * gib,
		"db.m5.large":     8 * gib,
		"db.m6g.2xlarge":  32 * gib,
		"db.m7g.16xlarge": 256 * gib,
		"db.r5.large":     16 * gib,
		"db.r6g.large":    16 * gib,
		"db.r6i.4xlarge":  128 * gib,
		"db.r7g.xlarge":   32 * gib,
		"db.x2g.large":    32 * gib,
		"DB.R6G.LARGE":    16 * gib,
	}
	for class, want := range cases {
		if got := InstanceClassMemoryBytes(class); got != want {
			t.Errorf("InstanceClassMemoryBytes(%q) = %v, want %v", class, got, want)
		}
	}
	for _, class := range []string{"", "db.serverless", "db.r6g.huge", "db.q9.large",
		"r6g.large"} {
		if got := InstanceClassMemoryBytes(class); got != 0 {
			t.Errorf("InstanceClassMemoryBytes(%q) = %v, want 0 (unknown)", class, got)
		}
	}
}

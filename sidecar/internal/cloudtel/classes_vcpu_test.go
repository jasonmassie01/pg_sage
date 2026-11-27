package cloudtel

import (
	"context"
	"testing"
)

// market-04: the instance size is kept on the sample (class, vCPUs,
// serverless) so cost attribution and right-sizing can use it. An unknown
// class has 0 vCPUs, never a guess.

func TestInstanceClassVCPUs(t *testing.T) {
	for class, want := range map[string]int{
		"db.r6g.large":    2,
		"db.r6g.xlarge":   4,
		"db.m5.2xlarge":   8,
		"db.r7g.16xlarge": 64,
		"DB.M6I.4XLARGE":  16,
		"db.t3.micro":     2,
		"db.t4g.medium":   2,
		"db.t4g.xlarge":   4,
		"db.t3.2xlarge":   8,
		"db.x2g.large":    2,
		"db.r6g.huge":     0,
		"db.unknown.huge": 0,
		"db.zz9.large":    0,
		"db.serverless":   0,
		"r6g.large":       0,
		"":                0,
		"db.r6g.0xlarge":  0,
		"db.r6g.99xlarge": 0,
	} {
		if got := InstanceClassVCPUs(class); got != want {
			t.Errorf("InstanceClassVCPUs(%q) = %d, want %d", class, got, want)
		}
	}
}

func TestTierVCPUs(t *testing.T) {
	for tier, want := range map[string]int{
		"db-custom-4-16384":    4,
		"db-custom-1-3840":     1,
		"db-n1-standard-8":     8,
		"db-n1-highmem-16":     16,
		"db-f1-micro":          0, // shared core: a fraction of a vCPU
		"db-g1-small":          0,
		"db-perf-optimized-N8": 0,
		"":                     0,
		"db-custom-x-1024":     0,
	} {
		if got := TierVCPUs(tier); got != want {
			t.Errorf("TierVCPUs(%q) = %d, want %d", tier, got, want)
		}
	}
}

func TestAWSCollectKeepsInstanceSize(t *testing.T) {
	f := rdsFixture(t, true)
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	if s.InstanceClass != "db.r6g.large" || s.VCPUs != 2 || s.Serverless {
		t.Fatalf("size = %q/%d/serverless=%v, want db.r6g.large/2/false",
			s.InstanceClass, s.VCPUs, s.Serverless)
	}
}

func TestAWSCollectUnknownClassHasNoVCPUs(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = instanceXML("orders", "db.unknown.huge", "postgres",
		"orders-pg16", "in-sync", 100, 500, true, "")
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	if s.InstanceClass != "db.unknown.huge" || s.VCPUs != 0 {
		t.Fatalf("size = %q/%d, want the class kept and 0 vCPUs", s.InstanceClass, s.VCPUs)
	}
}

func TestAWSCollectServerlessIsMarked(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = instanceXML("orders", "db.serverless", "postgres",
		"orders-pg16", "in-sync", 100, 500, true, "")
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	if !s.Serverless || s.VCPUs != 0 {
		t.Fatalf("serverless=%v vcpus=%d, want serverless with unknown vCPUs",
			s.Serverless, s.VCPUs)
	}
}

func TestGCPCollectKeepsTier(t *testing.T) {
	f := cloudSQLFixture(t, false, "0")
	s, err := f.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if s.InstanceClass != "db-custom-4-16384" || s.VCPUs != 4 || s.Serverless {
		t.Fatalf("size = %q/%d/%v, want db-custom-4-16384/4/false", s.InstanceClass,
			s.VCPUs, s.Serverless)
	}
}

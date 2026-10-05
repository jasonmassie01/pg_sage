package cloudtel

import "testing"

// ParseRDSHost names the RDS/Aurora resource an endpoint selects. Only
// AWS-issued endpoint shapes resolve; custom DNS, IPs and RDS Proxy never
// cause discovery (they report unavailable with a reason upstream).
func TestParseRDSHost(t *testing.T) {
	cases := []struct {
		name, host string
		want       RDSHost
		ok         bool
	}{
		{"instance", "orders-db.c9akciq32.us-east-1.rds.amazonaws.com",
			RDSHost{Identifier: "orders-db", Region: "us-east-1"}, true},
		{"instance upper case and trailing dot", "Orders-DB.C9AKCIQ32.EU-WEST-2.RDS.AMAZONAWS.COM.",
			RDSHost{Identifier: "orders-db", Region: "eu-west-2"}, true},
		{"aurora writer cluster endpoint", "shop.cluster-c9akciq32.us-west-2.rds.amazonaws.com",
			RDSHost{Identifier: "shop", Region: "us-west-2", Cluster: true}, true},
		{"aurora reader endpoint", "shop.cluster-ro-c9akciq32.us-west-2.rds.amazonaws.com",
			RDSHost{Identifier: "shop", Region: "us-west-2", Cluster: true, Reader: true}, true},
		{"china partition", "db1.abc123.cn-north-1.rds.amazonaws.com.cn",
			RDSHost{Identifier: "db1", Region: "cn-north-1"}, true},
		{"rds proxy is not an instance", "px.proxy-abc123.us-east-1.rds.amazonaws.com",
			RDSHost{}, false},
		{"custom endpoint is ambiguous", "c1.cluster-custom-abc123.us-east-1.rds.amazonaws.com",
			RDSHost{}, false},
		{"ip address", "10.0.0.12", RDSHost{}, false},
		{"empty", "", RDSHost{}, false},
		{"lookalike domain", "db.abc.us-east-1.rds.amazonaws.com.evil.example", RDSHost{}, false},
		{"bad identifier", "-db.abc.us-east-1.rds.amazonaws.com", RDSHost{}, false},
		{"bad region", "db.abc.useast1.rds.amazonaws.com", RDSHost{}, false},
		{"missing label", "abc.us-east-1.rds.amazonaws.com", RDSHost{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRDSHost(tc.host)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseRDSHost(%q) = %+v, %t; want %+v, %t",
					tc.host, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestValidRDSIdentifier(t *testing.T) {
	good := []string{"a", "orders-db", "db1", "a-b-c"}
	bad := []string{"", "1db", "db--x", "db-", "db_x", "db x", "db;rm",
		"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklm"}
	for _, id := range good {
		if !ValidRDSIdentifier(id) {
			t.Errorf("ValidRDSIdentifier(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if ValidRDSIdentifier(id) {
			t.Errorf("ValidRDSIdentifier(%q) = true, want false", id)
		}
	}
}

func TestValidAWSRegion(t *testing.T) {
	for _, r := range []string{"us-east-1", "eu-central-2", "ap-southeast-4", "us-gov-west-1",
		"cn-north-1"} {
		if !ValidAWSRegion(r) {
			t.Errorf("ValidAWSRegion(%q) = false", r)
		}
	}
	for _, r := range []string{"", "us-east", "useast1", "us-east-1/../x", "US-EAST-1x"} {
		if ValidAWSRegion(r) {
			t.Errorf("ValidAWSRegion(%q) = true", r)
		}
	}
}

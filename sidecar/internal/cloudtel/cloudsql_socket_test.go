package cloudtel

import "testing"

// No concurrent access tests: ParseCloudSQLSocket is a pure function.

func TestParseCloudSQLSocket(t *testing.T) {
	cases := []struct {
		host string
		want CloudSQLSocket
		ok   bool
	}{
		{"/cloudsql/proj-1:us-central1:main", CloudSQLSocket{"proj-1", "us-central1", "main"},
			true},
		{"/cloudsql/proj-1:europe-west4:db-2/", CloudSQLSocket{"proj-1", "europe-west4",
			"db-2"}, true},
		{"/tmp/sockets/proj-1:us-central1:main", CloudSQLSocket{"proj-1", "us-central1",
			"main"}, true},
		{"/cloudsql/proj-1:main", CloudSQLSocket{}, false},
		{"/cloudsql/example.com:proj-1:us-central1:main", CloudSQLSocket{}, false},
		{"/cloudsql/p:us-central1:main", CloudSQLSocket{}, false},
		{"/cloudsql/proj-1:uscentral:main", CloudSQLSocket{}, false},
		{"/cloudsql/proj-1:us-central1:Main", CloudSQLSocket{}, false},
		{"/cloudsql/proj-1:us-central1:", CloudSQLSocket{}, false},
		{"proj-1:us-central1:main", CloudSQLSocket{}, false},
		{"/var/run/postgresql", CloudSQLSocket{}, false},
		{"", CloudSQLSocket{}, false},
		{"10.0.0.5", CloudSQLSocket{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseCloudSQLSocket(tc.host)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseCloudSQLSocket(%q) = %+v, %v; want %+v, %v", tc.host, got, ok,
				tc.want, tc.ok)
		}
	}
}

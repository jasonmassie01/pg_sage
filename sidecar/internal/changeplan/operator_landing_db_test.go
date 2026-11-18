package changeplan

import (
	"strings"
	"testing"
)

// The landing read sees the server's value, not the reader's own
// transaction settings: the store's transactions SET LOCAL
// statement_timeout = '30s' and lock_timeout = '5s', which must neither
// land a step expecting those values nor hide the value the operator set.
func TestDBLandings_ReadsTheServerValueNotTheReadersOwn(t *testing.T) {
	f := newEngineFixture(t, "op_own_settings")
	d := NewDBLandings(f.pool)
	var server string
	if err := f.pool.QueryRow(f.ctx, `SELECT current_setting('statement_timeout')`).
		Scan(&server); err != nil {
		t.Fatalf("read statement_timeout: %v", err)
	}
	if server == "30s" {
		t.Fatalf("the server's statement_timeout is already 30s; the test cannot tell")
	}
	for _, c := range []struct{ setting, value string }{
		{"statement_timeout", "30s"}, {"lock_timeout", "5s"}} {
		s := operatorStep(1)
		s.Landing = &Landing{Setting: c.setting, Value: c.value}
		got, err := d.CheckLanding(f.ctx, s, f.clock.Now())
		if err != nil {
			t.Fatalf("%s: CheckLanding: %v", c.setting, err)
		}
		if got.Landed {
			t.Fatalf("%s = %s landed with nobody setting it: %+v", c.setting, c.value, got)
		}
	}
	s := operatorStep(1)
	s.Landing = &Landing{Setting: "statement_timeout", Value: server}
	got, err := d.CheckLanding(f.ctx, s, f.clock.Now())
	if err != nil || !got.Landed || !strings.Contains(got.Detail, "statement_timeout") {
		t.Fatalf("statement_timeout = %s (the server's value): %+v, %v; want landed", server,
			got, err)
	}
}

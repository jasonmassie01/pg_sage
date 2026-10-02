package replay

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The replay case format (pg_sage.sre.replay_case.v1): one JSON file per
// case, decoded strictly and validated before anything replays it. A
// case is frozen at detection time: no observation (or timestamp inside
// one) may come from after the investigation's active-time ceiling.

const detected = "2026-09-14T03:12:00Z"

// validCase is a minimal positive lock case; tests mutate its map form.
func validCase() map[string]any {
	return map[string]any{
		"schema": Schema, "id": "lock-test-01", "family": "lock_blocking",
		"class": ClassPositive, "description": "an idle-in-transaction holder",
		"provenance": "synthetic", "detected_at": detected,
		"subject":  "lock_contention on orders",
		"gold":     map[string]any{"root": "idle_in_tx_holder", "rationale": "idle head"},
		"canaries": []any{},
		"observations": []any{
			map[string]any{"probe": "lock_graph", "status": "ok", "offset_ms": 120,
				"rows": []any{map[string]any{"waiter_pid": 11, "blocker_pid": 10,
					"blocker_kind": "backend", "blocker_state": "idle in transaction",
					"lock_type": "transactionid", "requested_mode": "ShareLock",
					"blocker_xact_age_s":    340.5,
					"blocker_backend_start": "2026-09-14T02:00:00Z"}}},
			map[string]any{"probe": "prepared_xacts", "status": "empty", "offset_ms": 130},
			map[string]any{"probe": "sage_actions", "status": "empty", "offset_ms": 140},
		},
	}
}

func encode(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseValid(t *testing.T, m map[string]any) (Case, error) {
	t.Helper()
	c, err := Parse(encode(t, m))
	if err != nil {
		return c, err
	}
	return c, c.Validate(probes.Catalog())
}

func TestParse_ValidCase(t *testing.T) {
	c, err := parseValid(t, validCase())
	if err != nil {
		t.Fatalf("valid case rejected: %v", err)
	}
	if c.ID != "lock-test-01" || c.Family != "lock_blocking" || c.Class != ClassPositive ||
		c.Gold.Root != "idle_in_tx_holder" || len(c.Observations) != 3 {
		t.Fatalf("decoded %+v", c)
	}
	if !c.DetectedAt.Equal(time.Date(2026, 9, 14, 3, 12, 0, 0, time.UTC)) {
		t.Fatalf("detected_at %s", c.DetectedAt)
	}
	n, ok := c.Observations[0].Rows[0]["waiter_pid"].(json.Number)
	if !ok || n.String() != "11" {
		t.Fatalf("numbers must stay exact json.Number, got %T %v",
			c.Observations[0].Rows[0]["waiter_pid"], c.Observations[0].Rows[0]["waiter_pid"])
	}
	if c.Sufficient() != true {
		t.Fatal("a case with a gold root is sufficient")
	}
}

func TestParse_RejectsMalformedInput(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":         "",
		"not json":      "{not json",
		"array":         "[]",
		"trailing data": `{"schema":"` + Schema + `"} {}`,
		"unknown field": `{"schema":"` + Schema + `","surprise":1}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		} else if !errors.Is(err, ErrInvalidCase) {
			t.Errorf("%s: error %v does not wrap ErrInvalidCase", name, err)
		}
	}
	big := `{"schema":"` + Schema + `","description":"` + strings.Repeat("x", MaxCaseBytes) +
		`"}`
	if _, err := Parse([]byte(big)); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("oversized case: %v", err)
	}
}

func TestValidate_RejectsInvalidCases(t *testing.T) {
	obs := func(m map[string]any) map[string]any {
		return m["observations"].([]any)[0].(map[string]any)
	}
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"wrong schema", func(m map[string]any) { m["schema"] = "v0" }, "schema"},
		{"bad id", func(m map[string]any) { m["id"] = "Bad ID" }, "id"},
		{"empty family", func(m map[string]any) { m["family"] = "" }, "family"},
		{"unknown class", func(m map[string]any) { m["class"] = "noise" }, "class"},
		{"no description", func(m map[string]any) { m["description"] = " " }, "description"},
		{"no provenance", func(m map[string]any) { m["provenance"] = "" }, "provenance"},
		{"no detection time", func(m map[string]any) { delete(m, "detected_at") },
			"detected_at"},
		{"no rationale", func(m map[string]any) {
			m["gold"] = map[string]any{"root": "idle_in_tx_holder"}
		}, "rationale"},
		{"unknown root", func(m map[string]any) {
			m["gold"] = map[string]any{"root": "cosmic_rays", "rationale": "x"}
		}, "root"},
		{"root of another family", func(m map[string]any) {
			m["gold"] = map[string]any{"root": "pool_fan_out", "rationale": "x"}
		}, "family"},
		{"unknown contributing", func(m map[string]any) {
			m["gold"] = map[string]any{"root": "idle_in_tx_holder",
				"contributing": []any{"nope"}, "rationale": "x"}
		}, "contributing"},
		{"positive without root", func(m map[string]any) {
			m["gold"] = map[string]any{"rationale": "x"}
		}, "positive"},
		{"confounded with a root", func(m map[string]any) {
			m["class"] = ClassConfounded
		}, "confounded"},
		{"confounded without lookalike", func(m map[string]any) {
			m["class"] = ClassConfounded
			m["gold"] = map[string]any{"rationale": "x"}
		}, "lookalike"},
		{"no observations", func(m map[string]any) { m["observations"] = []any{} },
			"observations"},
		{"unknown probe", func(m map[string]any) { obs(m)["probe"] = "drop_table" },
			"probe"},
		{"unknown status", func(m map[string]any) { obs(m)["status"] = "fine" }, "status"},
		{"ok without rows", func(m map[string]any) { delete(obs(m), "rows") }, "rows"},
		{"empty with rows", func(m map[string]any) { obs(m)["status"] = "empty" }, "rows"},
		{"failure without reason", func(m map[string]any) {
			o := obs(m)
			o["status"] = "error"
			delete(o, "rows")
		}, "reason"},
		{"failure with rows", func(m map[string]any) {
			o := obs(m)
			o["status"], o["reason"] = "no_privilege", "insufficient_privilege"
		}, "rows"},
		{"observation after the active-time ceiling", func(m map[string]any) {
			obs(m)["offset_ms"] = MaxLookahead.Milliseconds() + 1
		}, "future"},
		{"observation older than the lookback", func(m map[string]any) {
			obs(m)["offset_ms"] = -MaxLookback.Milliseconds() - 1
		}, "lookback"},
		{"timestamp in a row from the future", func(m map[string]any) {
			row := obs(m)["rows"].([]any)[0].(map[string]any)
			row["blocker_backend_start"] = "2026-09-14T03:15:00Z"
		}, "future"},
		{"bad tag", func(m map[string]any) { m["tags"] = []any{"Bad Tag"} }, "tag"},
		{"canary too short", func(m map[string]any) { m["canaries"] = []any{"abc"} },
			"canary"},
		{"canary not in the data", func(m map[string]any) {
			m["canaries"] = []any{"CANARY-NOT-PRESENT-1"}
		}, "canary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validCase()
			c.mutate(m)
			_, err := parseValid(t, m)
			if err == nil {
				t.Fatalf("accepted")
			}
			if !errors.Is(err, ErrInvalidCase) {
				t.Fatalf("error %v does not wrap ErrInvalidCase", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestValidate_BoundariesAccepted(t *testing.T) {
	m := validCase()
	o := m["observations"].([]any)
	o[0].(map[string]any)["offset_ms"] = MaxLookahead.Milliseconds()
	o[1].(map[string]any)["offset_ms"] = -MaxLookback.Milliseconds()
	row := o[0].(map[string]any)["rows"].([]any)[0].(map[string]any)
	row["blocker_backend_start"] = "2026-09-14T03:14:00Z" // exactly +120 s
	if _, err := parseValid(t, m); err != nil {
		t.Fatalf("boundary offsets rejected: %v", err)
	}
}

func TestValidate_CanaryAndClassesThatNeedNoRoot(t *testing.T) {
	m := validCase()
	m["class"] = ClassAdversarial
	m["subject"] = "lock on orders password=CANARY-SUBJ-0001"
	m["canaries"] = []any{"CANARY-SUBJ-0001"}
	if _, err := parseValid(t, m); err != nil {
		t.Fatalf("adversarial case with a root and a canary in the subject: %v", err)
	}
	m = validCase()
	m["class"] = ClassMissingData
	m["gold"] = map[string]any{"rationale": "the lock graph is unavailable"}
	c, err := parseValid(t, m)
	if err != nil || c.Sufficient() {
		t.Fatalf("missing-data case without a root: %+v %v", c, err)
	}
	m = validCase()
	m["class"] = ClassConfounded
	m["gold"] = map[string]any{"lookalike": "hot_row_contention", "rationale": "x"}
	if _, err := parseValid(t, m); err != nil {
		t.Fatalf("confounded case: %v", err)
	}
}

func TestLoad_SortsAndRejectsDuplicates(t *testing.T) {
	a, b := validCase(), validCase()
	b["id"] = "lock-test-00"
	fsys := fstest.MapFS{
		"cases/lock_blocking/a.json": {Data: encode(t, a)},
		"cases/lock_blocking/b.json": {Data: encode(t, b)},
		"cases/README.txt":           {Data: []byte("not a case")},
	}
	cs, err := Load(fsys, probes.Catalog())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cs) != 2 || cs[0].ID != "lock-test-00" || cs[1].ID != "lock-test-01" {
		t.Fatalf("loaded %d cases, ids %v", len(cs), ids(cs))
	}
	fsys["cases/lock_blocking/c.json"] = &fstest.MapFile{Data: encode(t, a)}
	if _, err := Load(fsys, probes.Catalog()); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestLoad_NamesTheBrokenFile(t *testing.T) {
	bad := validCase()
	bad["class"] = "nope"
	fsys := fstest.MapFS{"cases/wal/broken.json": {Data: encode(t, bad)}}
	_, err := Load(fsys, probes.Catalog())
	if err == nil || !strings.Contains(err.Error(), "broken.json") ||
		!errors.Is(err, ErrInvalidCase) {
		t.Fatalf("error must name the file and wrap ErrInvalidCase: %v", err)
	}
}

func TestLoad_EmptyOrNil(t *testing.T) {
	if _, err := Load(fstest.MapFS{}, probes.Catalog()); err == nil {
		t.Fatal("an empty corpus loaded")
	}
	if _, err := Load(fstest.MapFS{"x.json": {Data: encode(t, validCase())}},
		nil); err == nil {
		t.Fatal("a nil registry validated a case")
	}
}

func ids(cs []Case) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

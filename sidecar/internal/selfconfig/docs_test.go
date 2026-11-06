package selfconfig

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// footprint-01: a derived key reported "live" is written into the runtime
// config while pg_sage runs. That is true only when the key's reader
// re-reads the config each cycle; otherwise the ledger, the Derived
// settings page and /api/v1/derived-settings report a value the process is
// not running. Every live derived key must therefore be listed here with
// the test that proves its reader picks up a change.
var liveKeyReaders = map[string]string{
	"collector.interval_seconds": "collector.TestCyclePicksUpAChangedInterval",
}

// unlistedLiveKeys is every rule key reported live with no reader test.
func unlistedLiveKeys(rules []Rule, readers map[string]string) []string {
	var out []string
	for _, r := range rules {
		if isRestart(r.Key) {
			continue
		}
		if readers[r.Key] == "" {
			out = append(out, r.Key)
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryLiveDerivedKeyHasACycleReader(t *testing.T) {
	if missing := unlistedLiveKeys(Rules(), liveKeyReaders); len(missing) != 0 {
		t.Fatalf("derived keys reported live without a reader that re-reads them each "+
			"cycle: %v; make the reader follow the runtime config and add its test to "+
			"liveKeyReaders, or keep the key restart-bound", missing)
	}
}

// The allowlist holds no stale entry: every listed key is a derived key
// and is live.
func TestLiveKeyReadersListsOnlyLiveDerivedKeys(t *testing.T) {
	for key := range liveKeyReaders {
		if _, ok := Lookup(key); !ok {
			t.Errorf("%s is listed as a live reader but is not derived", key)
			continue
		}
		if isRestart(key) {
			t.Errorf("%s is listed as a live reader but is restart-bound", key)
		}
	}
}

// CHECK-03: a new derived key marked live without a reader test is
// caught. analyzer.interval_seconds is a reconfigure (live) field that is
// not derived today.
func TestUnlistedLiveKeysCatchesANewLiveKey(t *testing.T) {
	rules := append(Rules(), Rule{Key: "analyzer.interval_seconds"})
	got := unlistedLiveKeys(rules, liveKeyReaders)
	if !reflect.DeepEqual(got, []string{"analyzer.interval_seconds"}) {
		t.Fatalf("unlisted live keys %v, want [analyzer.interval_seconds]", got)
	}
	// Restart-bound rules are never reported, whatever the allowlist says.
	if got := unlistedLiveKeys(Rules(), map[string]string{}); !reflect.DeepEqual(got,
		[]string{"collector.interval_seconds"}) {
		t.Fatalf("with an empty allowlist %v, want only the collector interval", got)
	}
}

// Nil/empty: no rules, no missing readers.
func TestUnlistedLiveKeysEmpty(t *testing.T) {
	if got := unlistedLiveKeys(nil, nil); len(got) != 0 {
		t.Fatalf("no rules gave %v", got)
	}
}

// The generated reference still reports the collector interval live.
func TestMarkdownReportsTheCollectorIntervalLive(t *testing.T) {
	if !strings.Contains(Markdown(), "| `collector.interval_seconds` | live |") {
		t.Fatal("the derived-settings reference no longer reports the collector " +
			"interval live")
	}
}

// No concurrent-access test: unlistedLiveKeys is a pure function over its
// arguments.

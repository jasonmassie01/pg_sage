package replay

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The embedded R1 corpus (AI-SRE-SPEC §12 source 1, Codex §10): 60
// redacted replay cases, 30 positive across the three R1 families, 15
// benign/confounded lookalikes and 15 missing-data/adversarial, every
// one valid and frozen at detection time.

var r1Families = []string{"lock_blocking", "connection_pressure", "wal_retention"}

// r1Plan is how often each R1 family's fixed probe plan runs each probe
// (internal/sre/plan.go). A case records every planned run, so missing
// evidence is always an explicit failure, never "not recorded".
var r1Plan = map[string]map[probes.ID]int{
	"lock_blocking": {probes.LockGraph: 1, probes.PreparedXacts: 1,
		probes.LongTransactions: 1, probes.SageActions: 1},
	"connection_pressure": {probes.ConnectionSaturation: 2, probes.LockGraph: 1,
		probes.SageActions: 1},
	"wal_retention": {probes.ReplicationSlots: 2, probes.WALCheckpoint: 2,
		probes.Archiver: 2, probes.SageActions: 1},
}

func loadCorpus(t *testing.T) []Case {
	t.Helper()
	cs, err := Corpus()
	if err != nil {
		t.Fatalf("embedded corpus: %v", err)
	}
	return cs
}

// TestCorpus_SizeAndClassMix counts the frozen R1 corpus only: cases
// added after R1 (tagged TagPostR1, e.g. the failover and pooler cases of
// the Sage SRE follow-ups) extend the corpus without changing R1's mix.
func TestCorpus_SizeAndClassMix(t *testing.T) {
	cs := loadCorpus(t)
	count := map[string]map[string]int{}
	for _, c := range cs {
		if hasTag(c, TagPostR1) {
			continue
		}
		if count[c.Family] == nil {
			count[c.Family] = map[string]int{}
		}
		count[c.Family][c.Class]++
	}
	total := map[string]int{}
	for _, fam := range r1Families {
		got := count[fam]
		if got[ClassPositive] != 10 || got[ClassConfounded] != 5 ||
			got[ClassMissingData]+got[ClassAdversarial] != 5 {
			t.Errorf("%s: classes %v, want 10 positive, 5 confounded, 5 "+
				"missing-data/adversarial", fam, got)
		}
		for class, n := range got {
			total[class] += n
		}
	}
	if total[ClassPositive] != 30 || total[ClassConfounded] != 15 ||
		total[ClassMissingData]+total[ClassAdversarial] != 15 {
		t.Errorf("R1 totals %v, want 30/15/15", total)
	}
	if total[ClassMissingData] == 0 || total[ClassAdversarial] == 0 {
		t.Errorf("both missing-data and adversarial cases are needed: %v", total)
	}
}

func TestCorpus_RecordsEveryPlannedProbe(t *testing.T) {
	for _, c := range loadCorpus(t) {
		plan, ok := r1Plan[c.Family]
		if !ok {
			continue // a later family brings its own plan check
		}
		seen := map[probes.ID]int{}
		for _, o := range c.Observations {
			seen[o.Probe]++
		}
		for id, n := range plan {
			if seen[id] < n {
				t.Errorf("%s records %s %d time(s), its plan runs it %d", c.ID, id,
					seen[id], n)
			}
		}
	}
}

func TestCorpus_InsufficientCasesAreEnough(t *testing.T) {
	insufficient := map[string]int{}
	for _, c := range loadCorpus(t) {
		if !c.Sufficient() {
			insufficient[c.Family]++
			if c.Class == ClassPositive {
				t.Errorf("%s: a positive case must have a gold root", c.ID)
			}
		}
	}
	n := 0
	for _, fam := range r1Families {
		if insufficient[fam] < 6 {
			t.Errorf("%s has %d insufficient-evidence cases, want at least 6", fam,
				insufficient[fam])
		}
		n += insufficient[fam]
	}
	if n < 20 {
		t.Errorf("%d insufficient-evidence cases in all, want at least 20", n)
	}
}

func TestCorpus_AdversarialCasesCarryAnAttack(t *testing.T) {
	attacks := 0
	for _, c := range loadCorpus(t) {
		if c.Class != ClassAdversarial {
			continue
		}
		injected := hasTag(c, TagPromptInjection)
		if !injected && len(c.Canaries) == 0 {
			t.Errorf("%s is adversarial but has neither a prompt injection nor a "+
				"canary secret", c.ID)
		}
		if injected && !strings.Contains(strings.ToLower(caseText(c)), "ignore") {
			t.Errorf("%s is tagged %s but its data carries no injected instruction", c.ID,
				TagPromptInjection)
		}
		attacks++
	}
	if attacks < 6 {
		t.Errorf("%d adversarial cases, want at least 6 (2 per family)", attacks)
	}
}

func TestCorpus_EveryCaseHasDistinctData(t *testing.T) {
	seen := map[string]string{}
	for _, c := range loadCorpus(t) {
		key := caseText(c)
		if other, dup := seen[key]; dup {
			t.Errorf("%s replays the same observations as %s", c.ID, other)
		}
		seen[key] = c.ID
	}
}

func hasTag(c Case, tag string) bool {
	for _, t := range c.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Sage SRE follow-ups B: the post-R1 cases cover a failover between the
// samples of both two-sample families (CHECK-07) and pool exhaustion at
// an external pooler (CHECK-04). A pooler case records the pooler
// telemetry at both samples, as the connection plan collects it when a
// pooler is configured.
func TestCorpus_FollowUpCases(t *testing.T) {
	want := map[string]string{"conn-failover-between-samples": ClassMissingData,
		"wal-failover-between-samples": ClassMissingData, "conn-pooler-queueing": ClassPositive,
		"conn-pooler-behind-lock-backlog": ClassPositive,
		"conn-pooler-busy-no-queue":       ClassConfounded}
	found := map[string]bool{}
	for _, c := range loadCorpus(t) {
		class, ok := want[c.ID]
		if !ok {
			continue
		}
		found[c.ID] = true
		if c.Class != class || !hasTag(c, TagPostR1) {
			t.Errorf("%s: class %s tags %v, want %s tagged %s", c.ID, c.Class, c.Tags,
				class, TagPostR1)
		}
		if !hasTag(c, "pooler") {
			continue
		}
		n := 0
		for _, o := range c.Observations {
			if o.Probe == probes.PoolerPools {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%s records pooler_pools %d time(s), want 2 (both samples)", c.ID, n)
		}
	}
	for id := range want {
		if !found[id] {
			t.Errorf("follow-up case %s is missing", id)
		}
	}
}

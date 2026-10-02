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

func TestCorpus_SizeAndClassMix(t *testing.T) {
	cs := loadCorpus(t)
	count := map[string]map[string]int{}
	for _, c := range cs {
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

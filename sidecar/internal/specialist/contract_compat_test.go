package specialist

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// v1 compatibility of the investigator and query_id additions (contract
// revision 1.1.0): a result without investigator or query data is byte for
// byte what v1.0 served (goldens generated before the change), every
// result validates against the frozen v1 baseline and the current
// document, the new fields are optional, and a v1 client that ignores
// unknown fields reads exactly the v1 fields.

func docNumbers(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	return decodeNumbers(t, raw).(map[string]any)
}

func baselineDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/contract.v1.baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	return docNumbers(t, raw)
}

func currentDoc(t *testing.T) map[string]any {
	t.Helper()
	return docNumbers(t, OpenAPIDocument())
}

func contestGoldenDetail() sre.Detail {
	d := lockDetail()
	d.Investigation.Summary.ModelContest = &sre.ModelContest{Label: sre.ModelContestLabel,
		GraphRoot: "idle_in_tx_holder", ModelRoot: "long_running_xact",
		Authority: sre.ContestAdvisory, Reason: "no root authority is configured"}
	return d
}

func goldenJSON(t *testing.T, d sre.Detail) []byte {
	t.Helper()
	r := MapResult(Snapshot{Detail: d}, MapOptions{KeepIdentifiers: true,
		Now: created.Add(5 * time.Minute)})
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestCompat_V1ResultsAreByteIdentical(t *testing.T) {
	for file, d := range map[string]sre.Detail{
		"testdata/result.v1.golden.json":         lockDetail(),
		"testdata/result.v1.contest.golden.json": contestGoldenDetail(),
	} {
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := goldenJSON(t, d); normalizedLF(got) != normalizedLF(want) {
			t.Errorf("%s: a v1 result changed:\n%s", file, got)
		}
	}
}

func TestCompat_V1GoldenValidatesAgainstBaselineAndCurrent(t *testing.T) {
	for _, file := range []string{"testdata/result.v1.golden.json",
		"testdata/result.v1.contest.golden.json"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		value := decodeNumbers(t, raw)
		for name, doc := range map[string]map[string]any{"baseline": baselineDoc(t),
			"current": currentDoc(t)} {
			if p := validateValue(doc, schemaNamed(doc, "Result"), value, "$"); len(p) > 0 {
				t.Errorf("%s against %s: %v", file, name, p)
			}
		}
	}
}

// v11Result is a result with every new field populated.
func v11Result(t *testing.T) []byte {
	t.Helper()
	d := withInvestigator(conclusion(sre.ModelAgreed, graphRootNode, graphRootNode,
		sre.ContestAdvisory, "the model agrees with the causal graph's root"))
	rec := &Record{Query: &QueryScope{QueryID: "-7212345678901234567",
		QueryHash: strings.Repeat("ab", 32), Applied: QueryAppliedEvidence}}
	r := MapResult(Snapshot{Detail: d, Record: rec}, MapOptions{KeepIdentifiers: true,
		Now: created.Add(5 * time.Minute)})
	if r.Investigator == nil || r.QueryScope == nil {
		t.Fatalf("fixture must carry the new fields: %+v", r)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCompat_V11ResultValidatesAgainstBaselineAndCurrent(t *testing.T) {
	value := decodeNumbers(t, v11Result(t))
	for name, doc := range map[string]map[string]any{"baseline": baselineDoc(t),
		"current": currentDoc(t)} {
		if p := validateValue(doc, schemaNamed(doc, "Result"), value, "$"); len(p) > 0 {
			t.Errorf("against %s: %v", name, p)
		}
	}
}

// A v1 client ignores unknown fields: what it reads of a 1.1 result with an
// agreeing investigator is exactly the v1 result.
func TestCompat_V1ClientReadsTheSameFields(t *testing.T) {
	v11 := decodeNumbers(t, v11Result(t)).(map[string]any)
	delete(v11, "investigator")
	delete(v11, "query_scope")
	v1 := decodeNumbers(t, goldenJSON(t, lockDetail())).(map[string]any)
	if !reflect.DeepEqual(v11, v1) {
		gotRaw, _ := json.Marshal(v11)
		wantRaw, _ := json.Marshal(v1)
		t.Fatalf("v1 fields differ:\n%s\n%s", gotRaw, wantRaw)
	}
}

func TestCompat_NewFieldsAreOptional(t *testing.T) {
	doc := currentDoc(t)
	for schema, fields := range map[string][]string{
		"Result": {"investigator", "query_scope"}, "OpenRequest": {"query_id", "query_hash"},
		"OpenResponse": {"query_scope"}} {
		s := schemaNamed(doc, schema)
		props := s["properties"].(map[string]any)
		req := stringSet(s["required"])
		for _, f := range fields {
			if _, ok := props[f]; !ok {
				t.Errorf("%s.%s is not documented", schema, f)
			}
			if req[f] {
				t.Errorf("%s.%s must be optional in v1", schema, f)
			}
		}
	}
}

func TestCompat_MinorRevisionBumped(t *testing.T) {
	info := func(doc map[string]any) map[string]any { return doc["info"].(map[string]any) }
	if v := info(currentDoc(t))["version"]; v != "1.1.0" {
		t.Fatalf("an additive change bumps the minor revision: %v", v)
	}
	if v := info(baselineDoc(t))["version"]; v != "1.0.0" {
		t.Fatalf("the frozen baseline stays 1.0.0: %v", v)
	}
	if info(currentDoc(t))["x-contract-version"] != ContractVersion ||
		ContractVersion != "pg_sage.specialist.v1" {
		t.Fatal("an additive change is not a new contract version")
	}
}

func TestCompat_TranscriptPathIsPublished(t *testing.T) {
	paths := currentDoc(t)["paths"].(map[string]any)
	const path = "/databases/{database}/investigations/{investigation_id}/transcript"
	op, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatal("the transcript path is not published")
	}
	get := op["get"].(map[string]any)
	responses := get["responses"].(map[string]any)
	for _, code := range []string{"200", "401", "403", "404"} {
		if _, ok := responses[code]; !ok {
			t.Errorf("transcript response %s missing", code)
		}
	}
	if get["operationId"] != "getInvestigationTranscript" {
		t.Errorf("operationId %v", get["operationId"])
	}
}

// The validator must fail broken results, or the checks above prove
// nothing.
func TestCompat_ValidatorDetectsBrokenResults(t *testing.T) {
	doc := currentDoc(t)
	mutations := map[string]func(m map[string]any){
		"missing required": func(m map[string]any) { delete(m, "outcome") },
		"wrong type":       func(m map[string]any) { m["chain_verified"] = "yes" },
		"bad enum":         func(m map[string]any) { m["outcome"] = "solved" },
		"bad nested enum": func(m map[string]any) {
			m["investigator"].(map[string]any)["verdict"] = "overrule"
		},
		"bad query id": func(m map[string]any) {
			m["query_scope"].(map[string]any)["query_id"] = "12a"
		},
		"null root object": func(m map[string]any) { m["root_cause"] = "x" },
	}
	for name, mutate := range mutations {
		value := decodeNumbers(t, v11Result(t)).(map[string]any)
		mutate(value)
		if p := validateValue(doc, schemaNamed(doc, "Result"), value, "$"); len(p) == 0 {
			t.Errorf("%s was not detected", name)
		}
	}
}

package specialist

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Contract tests: the published OpenAPI document is byte-for-byte the
// reviewed golden (any change is a deliberate, reviewed edit), it stays a
// backward-compatible superset of the frozen v1 baseline (a breaking change
// fails), and the Go types serve exactly the documented fields.

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func normalizedLF(b []byte) string {
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func TestContract_PublishedDocumentMatchesGolden(t *testing.T) {
	golden, err := os.ReadFile("testdata/openapi.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := normalizedLF(OpenAPIDocument()); got != normalizedLF(golden) {
		t.Fatalf("openapi.json changed without updating testdata/openapi.golden.json; " +
			"review the change against the v1 baseline, then update the golden")
	}
}

func TestContract_DocumentNamesTheContractVersion(t *testing.T) {
	var doc struct {
		Info struct {
			Version         string `json:"version"`
			ContractVersion string `json:"x-contract-version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(OpenAPIDocument(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info.ContractVersion != ContractVersion || ContractVersion !=
		"pg_sage.specialist.v1" || !strings.HasPrefix(doc.Info.Version, "1.") {
		t.Fatalf("contract version %q / info %+v", ContractVersion, doc.Info)
	}
}

func TestContract_IsBackwardCompatibleWithTheV1Baseline(t *testing.T) {
	baseline := readJSONFile(t, "testdata/contract.v1.baseline.json")
	var current map[string]any
	if err := json.Unmarshal(OpenAPIDocument(), &current); err != nil {
		t.Fatal(err)
	}
	if problems := compatProblems(baseline, current); len(problems) > 0 {
		t.Fatalf("breaking changes against the v1 baseline:\n%s",
			strings.Join(problems, "\n"))
	}
}

// breakingMutations are the kinds of breaking change the checker must catch.
var breakingMutations = map[string]func(doc map[string]any){
	"removed path": func(doc map[string]any) {
		delete(doc["paths"].(map[string]any), "/databases/{database}/investigations")
	},
	"removed response code": func(doc map[string]any) {
		op := pathOp(doc, "/databases/{database}/investigations/{investigation_id}/result",
			"get")
		delete(op["responses"].(map[string]any), "202")
	},
	"removed response property": func(doc map[string]any) {
		delete(schemaProps(doc, "Result"), "causal_chain")
	},
	"changed property type": func(doc map[string]any) {
		schemaProps(doc, "Citation")["numbers"] = map[string]any{"type": "array"}
	},
	"dropped guaranteed field": func(doc map[string]any) {
		s := schemaOf(doc, "RootCause")
		s["required"] = []any{"node"}
	},
	"new required request field": func(doc map[string]any) {
		s := schemaOf(doc, "OpenRequest")
		s["required"] = []any{"family"}
	},
	"removed request enum value": func(doc map[string]any) {
		p := schemaProps(doc, "OpenRequest")["family"].(map[string]any)
		p["enum"] = []any{"lock_blocking"}
	},
	"removed response enum value": func(doc map[string]any) {
		p := schemaProps(doc, "RemediationResponse")["verdict"].(map[string]any)
		p["enum"] = []any{"blocked"}
	},
	"shrunk request limit": func(doc map[string]any) {
		p := schemaProps(doc, "Symptom")["summary"].(map[string]any)
		p["maxLength"] = float64(10)
	},
	"removed schema": func(doc map[string]any) {
		delete(doc["components"].(map[string]any)["schemas"].(map[string]any),
			"Confidence")
	},
	"changed ref target": func(doc map[string]any) {
		schemaProps(doc, "Result")["confidence"] = map[string]any{
			"$ref": "#/components/schemas/Rollback"}
	},
}

// The checker must itself catch each kind of breaking change.
func TestContract_CompatCheckerDetectsBreakingChanges(t *testing.T) {
	for name, mutate := range breakingMutations {
		t.Run(name, func(t *testing.T) {
			baseline := readJSONFile(t, "testdata/contract.v1.baseline.json")
			changed := readJSONFile(t, "testdata/contract.v1.baseline.json")
			mutate(changed)
			if problems := compatProblems(baseline, changed); len(problems) == 0 {
				t.Fatalf("%s was not detected as breaking", name)
			}
		})
	}
	// Additive changes are compatible.
	baseline := readJSONFile(t, "testdata/contract.v1.baseline.json")
	added := readJSONFile(t, "testdata/contract.v1.baseline.json")
	schemaProps(added, "Result")["new_optional"] = map[string]any{"type": "string"}
	if p := compatProblems(baseline, added); len(p) != 0 {
		t.Fatalf("an added optional response field is compatible: %v", p)
	}
}

func pathOp(doc map[string]any, path, method string) map[string]any {
	return doc["paths"].(map[string]any)[path].(map[string]any)[method].(map[string]any)
}

func schemaOf(doc map[string]any, name string) map[string]any {
	return doc["components"].(map[string]any)["schemas"].(map[string]any)[name].(map[string]any)
}

func schemaProps(doc map[string]any, name string) map[string]any {
	return schemaOf(doc, name)["properties"].(map[string]any)
}

// requestSchemas are inputs: new required fields or shrunk limits break
// callers. Every other schema is an output.
var requestSchemas = map[string]bool{"OpenRequest": true, "Symptom": true, "Window": true,
	"Attach": true, "ExternalRef": true, "RemediationRequest": true, "WebhookRequest": true}

func compatProblems(baseline, current map[string]any) []string {
	var out []string
	bp, _ := baseline["paths"].(map[string]any)
	cp, _ := current["paths"].(map[string]any)
	for path, raw := range bp {
		cur, ok := cp[path].(map[string]any)
		if !ok {
			out = append(out, "path removed: "+path)
			continue
		}
		for method, op := range raw.(map[string]any) {
			cop, ok := cur[method].(map[string]any)
			if !ok {
				out = append(out, "operation removed: "+method+" "+path)
				continue
			}
			bresp, _ := op.(map[string]any)["responses"].(map[string]any)
			cresp, _ := cop["responses"].(map[string]any)
			for code := range bresp {
				if _, ok := cresp[code]; !ok {
					out = append(out, "response removed: "+method+" "+path+" "+code)
				}
			}
		}
	}
	bs := baseline["components"].(map[string]any)["schemas"].(map[string]any)
	cs, _ := current["components"].(map[string]any)["schemas"].(map[string]any)
	for name, raw := range bs {
		cur, ok := cs[name].(map[string]any)
		if !ok {
			out = append(out, "schema removed: "+name)
			continue
		}
		out = append(out, schemaProblems(name, raw.(map[string]any), cur)...)
	}
	sort.Strings(out)
	return out
}

func schemaProblems(name string, base, cur map[string]any) []string {
	var out []string
	bprops, _ := base["properties"].(map[string]any)
	cprops, _ := cur["properties"].(map[string]any)
	for prop, raw := range bprops {
		c, ok := cprops[prop].(map[string]any)
		if !ok {
			out = append(out, name+"."+prop+" removed")
			continue
		}
		out = append(out, propertyProblems(name+"."+prop, requestSchemas[name],
			raw.(map[string]any), c)...)
	}
	breq, creq := stringSet(base["required"]), stringSet(cur["required"])
	if requestSchemas[name] {
		for r := range creq {
			if !breq[r] {
				out = append(out, name+": new required input "+r)
			}
		}
		return out
	}
	for r := range breq {
		if !creq[r] {
			out = append(out, name+": guaranteed field "+r+" no longer required")
		}
	}
	return out
}

func propertyProblems(path string, request bool, base, cur map[string]any) []string {
	var out []string
	if !reflect.DeepEqual(base["type"], cur["type"]) {
		out = append(out, path+": type changed")
	}
	if !reflect.DeepEqual(base["$ref"], cur["$ref"]) {
		out = append(out, path+": reference changed")
	}
	if !reflect.DeepEqual(base["oneOf"], cur["oneOf"]) && base["oneOf"] != nil {
		out = append(out, path+": oneOf changed")
	}
	be, ce := stringSet(base["enum"]), stringSet(cur["enum"])
	for v := range be {
		if !ce[v] {
			out = append(out, path+": enum value "+v+" removed")
		}
	}
	if request {
		if bm, ok := base["maxLength"].(float64); ok {
			if cm, ok := cur["maxLength"].(float64); !ok || cm < bm {
				out = append(out, path+": maxLength shrunk")
			}
		}
	}
	return out
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	list, _ := v.([]any)
	for _, item := range list {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

// contractTypes maps each published schema to the Go type that serves it.
var contractTypes = map[string]reflect.Type{
	"ContractInfo": reflect.TypeOf(ContractInfo{}), "ErrorResponse": reflect.TypeOf(
		ErrorResponse{}), "OpenRequest": reflect.TypeOf(OpenRequest{}),
	"WebhookRequest": reflect.TypeOf(WebhookRequest{}),
	"WebhookIgnored": reflect.TypeOf(WebhookIgnored{}), "Symptom": reflect.TypeOf(
		Symptom{}), "Window": reflect.TypeOf(Window{}), "Attach": reflect.TypeOf(Attach{}),
	"ExternalRef": reflect.TypeOf(ExternalRef{}), "RemediationRequest": reflect.TypeOf(
		RemediationRequest{}), "InvestigationRef": reflect.TypeOf(InvestigationRef{}),
	"Links": reflect.TypeOf(Links{}), "OpenResponse": reflect.TypeOf(OpenResponse{}),
	"StatusResponse": reflect.TypeOf(StatusResponse{}), "Citation": reflect.TypeOf(
		Citation{}), "ChainLink": reflect.TypeOf(ChainLink{}),
	"Hypothesis": reflect.TypeOf(Hypothesis{}), "RootCause": reflect.TypeOf(RootCause{}),
	"CalibratedRate": reflect.TypeOf(CalibratedRate{}), "Confidence": reflect.TypeOf(
		Confidence{}), "MissingEvidence": reflect.TypeOf(MissingEvidence{}),
	"ModelContest": reflect.TypeOf(ModelContest{}), "PredictedEffect": reflect.TypeOf(
		PredictedEffect{}), "Rollback": reflect.TypeOf(Rollback{}),
	"GatePreview": reflect.TypeOf(GatePreview{}), "Remediation": reflect.TypeOf(
		Remediation{}), "EvidenceRef": reflect.TypeOf(EvidenceRef{}),
	"CallerSupplied": reflect.TypeOf(CallerSupplied{}), "Redaction": reflect.TypeOf(
		Redaction{}), "Result": reflect.TypeOf(Result{}),
	"RemediationResponse": reflect.TypeOf(RemediationResponse{}),
	// Contract revision 1.1.0.
	"QueryScope": reflect.TypeOf(QueryScope{}), "QueryScopeResult": reflect.TypeOf(
		QueryScopeResult{}), "InvestigatorResult": reflect.TypeOf(InvestigatorResult{}),
	"RootAdoption": reflect.TypeOf(RootAdoption{}), "InvestigatorClaim": reflect.TypeOf(
		InvestigatorClaim{}), "UnmodeledCause": reflect.TypeOf(UnmodeledCause{}),
	"InvestigatorRun": reflect.TypeOf(InvestigatorRun{}), "TranscriptLink": reflect.TypeOf(
		TranscriptLink{}), "TranscriptResponse": reflect.TypeOf(TranscriptResponse{}),
}

type jsonField struct {
	name      string
	omitempty bool
	typ       reflect.Type
}

func jsonFields(t reflect.Type) []jsonField {
	var out []jsonField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out = append(out, jsonField{name: name, omitempty: strings.Contains(opts,
			"omitempty"), typ: f.Type})
	}
	return out
}

func TestContract_GoTypesServeExactlyTheDocumentedFields(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(OpenAPIDocument(), &doc); err != nil {
		t.Fatal(err)
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if len(schemas) != len(contractTypes) {
		t.Fatalf("%d schemas, %d mapped Go types", len(schemas), len(contractTypes))
	}
	for name, typ := range contractTypes {
		s, ok := schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("schema %s missing", name)
		}
		props, _ := s["properties"].(map[string]any)
		required := stringSet(s["required"])
		fields := jsonFields(typ)
		if len(fields) != len(props) {
			t.Errorf("%s: %d Go fields, %d documented properties", name, len(fields),
				len(props))
		}
		for _, f := range fields {
			p, ok := props[f.name].(map[string]any)
			if !ok {
				t.Errorf("%s.%s is served but not documented", name, f.name)
				continue
			}
			if required[f.name] == f.omitempty {
				t.Errorf("%s.%s: required=%t but omitempty=%t", name, f.name,
					required[f.name], f.omitempty)
			}
			if !typeMatches(f.typ, p) {
				t.Errorf("%s.%s: Go type %s does not match %v", name, f.name, f.typ, p)
			}
		}
	}
}

func typeMatches(t reflect.Type, p map[string]any) bool {
	nullable := false
	if t.Kind() == reflect.Pointer {
		t, nullable = t.Elem(), true
	}
	if ref, ok := p["$ref"].(string); ok {
		return t.Kind() == reflect.Struct && strings.HasSuffix(ref, "/"+t.Name())
	}
	if oneOf, ok := p["oneOf"].([]any); ok {
		for _, alt := range oneOf {
			if ref, ok := alt.(map[string]any)["$ref"].(string); ok {
				return nullable && strings.HasSuffix(ref, "/"+t.Name())
			}
		}
		return false
	}
	types := map[string]bool{}
	switch v := p["type"].(type) {
	case string:
		types[v] = true
	case []any:
		for _, x := range v {
			types[x.(string)] = true
		}
	}
	if nullable && !types["null"] && t != reflect.TypeOf(time.Time{}) {
		return false
	}
	switch {
	case t == reflect.TypeOf(time.Time{}):
		return types["string"] && p["format"] == "date-time"
	case t.Kind() == reflect.String:
		return types["string"]
	case t.Kind() == reflect.Bool:
		return types["boolean"]
	case t.Kind() == reflect.Int, t.Kind() == reflect.Int64:
		return types["integer"]
	case t.Kind() == reflect.Float64:
		return types["number"]
	case t.Kind() == reflect.Slice:
		return types["array"]
	case t.Kind() == reflect.Map:
		return types["object"]
	}
	return false
}

package principalfile

import (
	"strings"
	"testing"
)

const validFile = `
apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata:
  name: prod-agents
spec:
  prune: true
  principals:
    - name: ci-bot
      sponsor: alice@example.com
      profile: readonly-analyst
      env_ceiling: stage
      identities:
        - {issuer: "https://idp.example.com", subject: "0oa1"}
    - name: etl-writer
      sponsor: bob@example.com
      profile: app-writer
      env_ceiling: prod
      status: frozen
`

func TestParseValidFile(t *testing.T) {
	f, err := Parse([]byte(validFile))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Metadata.Name != "prod-agents" || !f.Spec.Prune || len(f.Spec.Principals) != 2 {
		t.Fatalf("file = %+v", f)
	}
	ci := f.Spec.Principals[0]
	if ci.Name != "ci-bot" || ci.Status != "active" || ci.EnvCeiling != "stage" ||
		len(ci.Identities) != 1 || ci.Identities[0].Subject != "0oa1" {
		t.Fatalf("ci-bot = %+v (status defaults to active)", ci)
	}
	if f.Spec.Principals[1].Status != "frozen" {
		t.Fatalf("etl-writer status = %q", f.Spec.Principals[1].Status)
	}
}

// Every problem in a file is reported at once, each naming its principal.
func TestParseReportsEveryProblem(t *testing.T) {
	bad := `
apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata: {name: x}
spec:
  principals:
    - {name: Bad_Name, profile: legacy, env_ceiling: dev}
    - {name: ok-one, profile: legacy, env_ceiling: moon}
    - {name: ok-two, profile: legacy, env_ceiling: dev, status: retired}
    - name: ok-three
      profile: legacy
      env_ceiling: dev
      identities: [{issuer: "http://idp.example.com", subject: s}]
    - {name: dup, profile: legacy, env_ceiling: dev}
    - {name: dup, profile: legacy, env_ceiling: dev}
    - {name: no-profile, env_ceiling: dev}
`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatalf("bad file accepted")
	}
	for _, want := range []string{"Bad_Name", "moon", "retired", "https", "dup",
		"no-profile"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
}

func TestParseRejectsEnvelopeAndShapeErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key": `apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata: {name: x}
spec: {principals: [], surprise: 1}`,
		"version": `apiVersion: pg_sage/v9
kind: AgentPrincipals
metadata: {name: x}
spec: {principals: []}`,
		"kind": `apiVersion: pg_sage/v1
kind: Something
metadata: {name: x}
spec: {principals: []}`,
		"file name": `apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata: {name: "Bad Name"}
spec: {principals: []}`,
		"envelopes": `apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata: {name: x}
spec: {principals: [], envelopes: [{name: e}]}`,
		"profiles": `apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata: {name: x}
spec: {principals: [], profiles: {p: {classes: [read]}}}`,
		"empty":    ``,
		"two docs": "apiVersion: pg_sage/v1\n---\napiVersion: pg_sage/v1\n",
		"duplicate identity": `apiVersion: pg_sage/v1
kind: AgentPrincipals
metadata: {name: x}
spec:
  principals:
    - {name: a, profile: legacy, env_ceiling: dev,
       identities: [{issuer: "https://i", subject: s}]}
    - {name: b, profile: legacy, env_ceiling: dev,
       identities: [{issuer: "https://i", subject: s}]}`,
	}
	for name, body := range cases {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

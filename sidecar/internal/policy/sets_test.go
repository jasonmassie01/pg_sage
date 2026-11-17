package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fleetpol-01a: policy sets without a database. The DB-backed store, the
// gate read and the upgrade are in sets_db_test.go.

var testDimensions = []string{"recoverability", "security", "performance"}

func testSet(name string, priority int, selectors ...Selector) PolicySet {
	return PolicySet{Name: name, Priority: priority, Selectors: selectors}
}

func testSetDoc(set PolicySet, id, version int64, doc string) SetDocument {
	return SetDocument{Set: set, Policy: &Policy{ID: id, Version: version,
		Scope: Scope{SetName: set.Name}, Profile: "staffed", Document: json.RawMessage(doc)}}
}

func defaultSet() PolicySet { return testSet(DefaultSetName, 0, Selector{All: true}) }

func prodSet() PolicySet {
	return testSet("prod", 10, Selector{Tags: map[string]string{"tier": "prod"}})
}

func TestPolicySetSelector_MatchesTagsProviderSourceAndCreator(t *testing.T) {
	member := Member{DatabaseID: 7, Tags: map[string]string{"tier": "prod", "team": "pay"},
		Provider: "rds", ProviderProject: "acct-1", DiscoverySource: "rds-east",
		Creator: "agent:builder"}
	cases := []struct {
		name string
		sel  Selector
		want bool
	}{
		{"all", Selector{All: true}, true},
		{"one tag", Selector{Tags: map[string]string{"tier": "prod"}}, true},
		{"two tags", Selector{Tags: map[string]string{"tier": "prod", "team": "pay"}}, true},
		{"tag value differs", Selector{Tags: map[string]string{"tier": "dev"}}, false},
		{"tag absent", Selector{Tags: map[string]string{"zone": "a"}}, false},
		{"provider", Selector{Provider: "rds"}, true},
		{"provider differs", Selector{Provider: "cloud-sql"}, false},
		{"project", Selector{ProviderProject: "acct-1"}, true},
		{"source", Selector{DiscoverySource: "rds-east"}, true},
		{"creator prefix", Selector{Creator: "agent:*"}, true},
		{"creator exact", Selector{Creator: "agent:builder"}, true},
		{"creator other", Selector{Creator: "person:*"}, false},
		{"and of fields", Selector{Provider: "rds", Tags: map[string]string{"tier": "dev"}}, false},
		{"no criteria", Selector{}, false},
	}
	for _, tc := range cases {
		if got := tc.sel.Matches(member); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (Selector{Creator: "agent:*"}).Matches(Member{DatabaseID: 1}) {
		t.Error("a creator selector matched a member with no recorded creator")
	}
}

func TestPolicySet_NoSelectorsMatchesNothing(t *testing.T) {
	set := testSet("empty", 5)
	if set.Matches(Member{DatabaseID: 1, Tags: map[string]string{"tier": "prod"}}) {
		t.Fatal("a set with no selectors matched a database")
	}
	_, ok, err := MatchSet([]PolicySet{set}, Member{DatabaseID: 1})
	if err != nil || ok {
		t.Fatalf("MatchSet over a selector-less set = ok %v err %v, want no match", ok, err)
	}
}

func TestPolicySet_MatchPicksHighestPriorityAndRefusesTies(t *testing.T) {
	prodMember := Member{DatabaseID: 3, Tags: map[string]string{"tier": "prod"}}
	got, ok, err := MatchSet([]PolicySet{defaultSet(), prodSet()}, prodMember)
	if err != nil || !ok || got.Name != "prod" {
		t.Fatalf("MatchSet = %q ok %v err %v, want prod", got.Name, ok, err)
	}
	got, ok, err = MatchSet([]PolicySet{defaultSet(), prodSet()}, Member{DatabaseID: 4})
	if err != nil || !ok || got.Name != DefaultSetName {
		t.Fatalf("an untagged database matched %q (ok %v err %v), want default", got.Name, ok, err)
	}
	twin := testSet("prod-twin", 10, Selector{Tags: map[string]string{"tier": "prod"}})
	_, _, err = MatchSet([]PolicySet{defaultSet(), prodSet(), twin}, prodMember)
	if !errors.Is(err, ErrSetConflict) || !strings.Contains(err.Error(), "prod-twin") {
		t.Fatalf("a tie returned %v, want ErrSetConflict naming both sets", err)
	}
}

func TestValidateSet_RefusesBadInput(t *testing.T) {
	tooLong := strings.Repeat("a", 64)
	cases := []struct {
		name string
		set  PolicySet
		want string
	}{
		{"bad name", testSet("Prod!", 5, Selector{All: true}), "name"},
		{"long name", testSet(tooLong, 5, Selector{All: true}), "name"},
		{"negative priority", testSet("x", -1, Selector{All: true}), "priority"},
		{"priority too high", testSet("x", 1001, Selector{All: true}), "priority"},
		{"zero priority for a named set", testSet("x", 0, Selector{All: true}), "priority"},
		{"default priority", testSet(DefaultSetName, 3, Selector{All: true}), "default"},
		{"default narrowed", testSet(DefaultSetName, 0, Selector{Provider: "rds"}), "default"},
		{"empty selector", testSet("x", 5, Selector{}), "selector"},
		{"all with others", testSet("x", 5, Selector{All: true, Provider: "rds"}), "all"},
		{"empty tag key", testSet("x", 5, Selector{Tags: map[string]string{"": "v"}}), "tag"},
		{"unknown dimension", withGrades(testSet("x", 5, Selector{All: true}),
			GradeRequirement{Dimension: "vibes", MinLetter: "C", WithinHours: 24}), "vibes"},
		{"bad letter", withGrades(testSet("x", 5, Selector{All: true}),
			GradeRequirement{Dimension: "recoverability", MinLetter: "E", WithinHours: 24}),
			"letter"},
		{"zero window", withGrades(testSet("x", 5, Selector{All: true}),
			GradeRequirement{Dimension: "recoverability", MinLetter: "C"}), "within_hours"},
		{"negative ttl", withReq(testSet("x", 5, Selector{All: true}),
			SetRequirements{TTLHours: -1}), "ttl"},
		{"unknown class ceiling", withReq(testSet("x", 5, Selector{All: true}),
			SetRequirements{TrustCeiling: map[ChangeClass]string{"drop_table": "advisory"}}),
			"drop_table"},
		{"unknown trust level", withReq(testSet("x", 5, Selector{All: true}),
			SetRequirements{TrustCeiling: map[ChangeClass]string{ChangeIndex: "yolo"}}), "yolo"},
	}
	for _, tc := range cases {
		err := ValidateSet(tc.set, testDimensions)
		if !errors.Is(err, ErrInvalidSet) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ValidateSet = %v, want ErrInvalidSet mentioning %q",
				tc.name, err, tc.want)
		}
	}
}

func withGrades(set PolicySet, grades ...GradeRequirement) PolicySet {
	set.Requirements.Grades = grades
	return set
}

func withReq(set PolicySet, req SetRequirements) PolicySet {
	set.Requirements = req
	return set
}

func TestValidateSet_AcceptsSeedsAndRequirements(t *testing.T) {
	set := withReq(prodSet(), SetRequirements{
		Grades: []GradeRequirement{{Dimension: "recoverability", MinLetter: "C",
			WithinHours: 24}},
		OwnerRequired: true, TTLHours: 72, EnrollmentDeadlineHours: 24,
		TrustCeiling: map[ChangeClass]string{ChangeIndex: "advisory"},
	})
	for _, s := range []PolicySet{defaultSet(), set,
		testSet("agent-created", 20, Selector{Creator: "agent:*"})} {
		if err := ValidateSet(s, testDimensions); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
	}
}

func TestDecodeSet_RefusesUnknownSelectorKey(t *testing.T) {
	_, err := DecodeSet([]byte(`{"name":"x","priority":5,"selectors":[{"colour":"red"}]}`))
	if !errors.Is(err, ErrInvalidSet) || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("an unknown selector key decoded: %v", err)
	}
	_, err = DecodeSet([]byte(`{"name":"x","priority":5,"selectors":[],"extra":1}`))
	if !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("an unknown set field decoded: %v", err)
	}
	set, err := DecodeSet([]byte(`{"name":"prod","priority":10,
		"selectors":[{"tags":{"tier":"prod"}}],"requirements":{"owner_required":true}}`))
	if err != nil || set.Name != "prod" || set.Priority != 10 ||
		set.Selectors[0].Tags["tier"] != "prod" || !set.Requirements.OwnerRequired {
		t.Fatalf("DecodeSet = %+v, %v", set, err)
	}
	if _, err := DecodeSet(nil); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("an empty body decoded: %v", err)
	}
}

func TestCheckTies_RefusesEqualPriorityOverlapOnAMember(t *testing.T) {
	members := []Member{{DatabaseID: 1, Tags: map[string]string{"tier": "prod", "team": "a"}},
		{DatabaseID: 2}}
	teamA := testSet("team-a", 10, Selector{Tags: map[string]string{"team": "a"}})
	err := CheckTies([]PolicySet{defaultSet(), prodSet(), teamA}, members)
	if !errors.Is(err, ErrSetConflict) || !strings.Contains(err.Error(), "database 1") {
		t.Fatalf("CheckTies = %v, want a conflict on database 1", err)
	}
	teamA.Priority = 11
	if err := CheckTies([]PolicySet{defaultSet(), prodSet(), teamA}, members); err != nil {
		t.Fatalf("distinct priorities conflicted: %v", err)
	}
	everyone := testSet("everyone", 10, Selector{All: true})
	if err := CheckTies([]PolicySet{prodSet(), everyone}, nil); !errors.Is(err, ErrSetConflict) {
		t.Fatalf("an all-selector set tied with prod at priority 10 without members: %v", err)
	}
	if err := CheckTies(nil, nil); err != nil {
		t.Fatalf("no sets conflicted: %v", err)
	}
}

const docA = `{"allowed_change_classes":["index","analyze"],"serialize_mode":"park"}`
const docB = `{"allowed_change_classes":["analyze"],"serialize_mode":"park"}`

func TestResolve_SetsExceptionsAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	sets := []SetDocument{testSetDoc(defaultSet(), 10, 3, docA), testSetDoc(prodSet(), 11, 2, docB)}
	prod := Member{DatabaseID: 5, Tags: map[string]string{"tier": "prod"}}

	got, err := Resolve(sets, prod, nil, nil, now)
	if err != nil || got.Set != "prod" || got.Policy.ID != 11 || got.Policy.Version != 2 ||
		got.Exception != nil {
		t.Fatalf("prod member resolved to %+v, %v", got, err)
	}
	own := &Policy{ID: 40, Version: 4, Scope: Scope{DatabaseID: ptr64(5)}, Profile: "staffed",
		Document: json.RawMessage(docA), UpdatedBy: "dba"}
	got, err = Resolve(sets, prod, own, nil, now)
	if err != nil || got.Policy.ID != 40 || got.Exception == nil ||
		got.Exception.SetName != "prod" || got.Exception.SetBy != "dba" {
		t.Fatalf("an own document without metadata resolved to %+v, %v", got, err)
	}
	at := now
	exc := &Exception{DatabaseID: 5, SetName: "default", SetBy: "dba", ExpiresAt: &at}
	got, err = Resolve(sets, prod, own, exc, now)
	if err != nil || got.Policy.ID != 11 || got.Exception != nil || !got.ExceptionExpired {
		t.Fatalf("an exception expiring at now held: %+v, %v", got, err)
	}
	later := now.Add(time.Second)
	exc.ExpiresAt = &later
	got, err = Resolve(sets, prod, own, exc, now)
	if err != nil || got.Policy.ID != 40 || got.Exception == nil || got.ExceptionExpired {
		t.Fatalf("an exception expiring at now+1s reverted: %+v, %v", got, err)
	}
	if got.Exception.SetName != "prod" {
		t.Fatalf("the exception names %q, want the set the database matches now",
			got.Exception.SetName)
	}
}

func TestResolve_NilEmptyAndFailClosed(t *testing.T) {
	now := time.Now()
	member := Member{DatabaseID: 9}
	if _, err := Resolve(nil, member, nil, nil, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no sets and no own document = %v, want ErrNotFound (bootstrap)", err)
	}
	own := &Policy{ID: 2, Version: 1, Document: json.RawMessage(docA)}
	got, err := Resolve(nil, member, own, nil, now)
	if err != nil || got.Policy.ID != 2 || got.Set != "" {
		t.Fatalf("no sets: the database's own document is today's behaviour, got %+v, %v",
			got, err)
	}
	noDoc := []SetDocument{{Set: defaultSet()}}
	if _, err := Resolve(noDoc, member, nil, nil, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a set without a document resolved: %v", err)
	}
	twin := testSet("twin", 10, Selector{Tags: map[string]string{"tier": "prod"}})
	sets := []SetDocument{testSetDoc(prodSet(), 1, 1, docA), testSetDoc(twin, 2, 1, docA)}
	prod := Member{DatabaseID: 9, Tags: map[string]string{"tier": "prod"}}
	if _, err := Resolve(sets, prod, own, nil, now); !errors.Is(err, ErrSetConflict) {
		t.Fatalf("a tie resolved instead of failing closed: %v", err)
	}
}

// State transitions on one database: joins agent-created (tag added),
// leaves it, an exception is set, the exception expires.
func TestResolve_StateTransitions(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	agent := testSet("agent-created", 20, Selector{Tags: map[string]string{"origin": "agent"}})
	sets := []SetDocument{testSetDoc(defaultSet(), 1, 1, docA), testSetDoc(agent, 2, 1, docB)}
	member := Member{DatabaseID: 4, Tags: map[string]string{}}
	steps := []struct {
		name    string
		edit    func()
		wantSet string
		wantID  int64
	}{
		{"untagged", func() {}, DefaultSetName, 1},
		{"tag added", func() { member.Tags["origin"] = "agent" }, "agent-created", 2},
		{"tag removed", func() { delete(member.Tags, "origin") }, DefaultSetName, 1},
	}
	for _, step := range steps {
		step.edit()
		got, err := Resolve(sets, member, nil, nil, now)
		if err != nil || got.Set != step.wantSet || got.Policy.ID != step.wantID {
			t.Fatalf("%s: resolved %q/%d, %v", step.name, got.Set, got.Policy.ID, err)
		}
	}
	expires := now.Add(time.Hour)
	own := &Policy{ID: 30, Version: 1, Document: json.RawMessage(docB)}
	exc := &Exception{DatabaseID: 4, SetName: DefaultSetName, ExpiresAt: &expires}
	if got, _ := Resolve(sets, member, own, exc, now); got.Policy.ID != 30 {
		t.Fatalf("exception set: resolved %d, want the exception's 30", got.Policy.ID)
	}
	if got, _ := Resolve(sets, member, own, exc, expires); got.Policy.ID != 1 {
		t.Fatalf("exception expired: resolved %d, want default's 1", got.Policy.ID)
	}
}

func TestDocumentDiff_ListsChangedFields(t *testing.T) {
	diff, err := DocumentDiff(json.RawMessage(docA), json.RawMessage(docB))
	if err != nil || len(diff) != 1 || diff[0].Field != "allowed_change_classes" {
		t.Fatalf("DocumentDiff = %+v, %v", diff, err)
	}
	if string(diff[0].Set) != `["index","analyze"]` || string(diff[0].Exception) != `["analyze"]` {
		t.Fatalf("diff values = %s / %s", diff[0].Set, diff[0].Exception)
	}
	nested, err := DocumentDiff(json.RawMessage(`{"a":{"b":1,"c":2}}`),
		json.RawMessage(`{"a":{"b":1,"c":3},"d":true}`))
	if err != nil || len(nested) != 2 || nested[0].Field != "a.c" || nested[1].Field != "d" {
		t.Fatalf("nested diff = %+v, %v", nested, err)
	}
	same, err := DocumentDiff(json.RawMessage(docA), json.RawMessage(docA))
	if err != nil || len(same) != 0 {
		t.Fatalf("identical documents differ: %+v, %v", same, err)
	}
	if _, err := DocumentDiff(json.RawMessage(`{`), json.RawMessage(docA)); err == nil {
		t.Fatal("a malformed document diffed")
	}
}

func ptr64(v int64) *int64 { return &v }

func TestDecodeMemberTags_ObjectListAndInvalid(t *testing.T) {
	got, err := decodeMemberTags([]byte(`{"tier":"prod"}`))
	if err != nil || got["tier"] != "prod" || len(got) != 1 {
		t.Fatalf("object tags = %v, %v", got, err)
	}
	got, err = decodeMemberTags([]byte(`["tier=prod","canary"]`))
	if err != nil || got["tier"] != "prod" || got["canary"] != "" || len(got) != 2 {
		t.Fatalf("list tags = %v, %v", got, err)
	}
	if got, err := decodeMemberTags([]byte(`{}`)); err != nil || len(got) != 0 {
		t.Fatalf("empty tags = %v, %v", got, err)
	}
	if _, err := decodeMemberTags([]byte(`{"tier":7}`)); err == nil {
		t.Fatal("a non-string tag value decoded; resolution must fail closed on it")
	}
}

package classify

import (
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Spec §6.7: classes only narrow; secret is never granted; stage and prod
// grant only columns classified clean (pii with an unmask entry); branch
// and dev default to every column not classified secret. Masking is a
// convenience in branch and dev, never a control.

func TestParseClass(t *testing.T) {
	for _, s := range []string{"clean", "pii", "secret", "untrusted_input"} {
		c, err := ParseClass(s)
		if err != nil || string(c) != s {
			t.Errorf("ParseClass(%q) = %q, %v", s, c, err)
		}
	}
	for _, s := range []string{"", "PII", "public", "unclassified", "secret "} {
		if c, err := ParseClass(s); !errors.Is(err, ErrInvalidClass) || c != "" {
			t.Errorf("ParseClass(%q) = %q, %v", s, c, err)
		}
	}
	order := []Class{ClassClean, ClassUntrusted, ClassPII, ClassSecret}
	for i := 1; i < len(order); i++ {
		if order[i].Narrowness() <= order[i-1].Narrowness() {
			t.Errorf("%s must be narrower than %s", order[i], order[i-1])
		}
	}
	if Unclassified.Narrowness() >= ClassClean.Narrowness() {
		t.Error("unclassified ranks below clean")
	}
}

func cls(class Class, status Status) Classification {
	return Classification{ID: 1, Class: class, Status: status}
}

func TestClassificationEffective(t *testing.T) {
	cases := []struct {
		c    Classification
		want Class
	}{
		{cls(ClassPII, StatusConfirmed), ClassPII},
		{cls(ClassClean, StatusConfirmed), ClassClean},
		{cls(ClassSecret, StatusProposed), ClassSecret}, // proposals narrow
		{cls(ClassPII, StatusProposed), ClassPII},
		{cls(ClassUntrusted, StatusProposed), ClassUntrusted},
		{cls(ClassClean, StatusProposed), Unclassified}, // never widen
		{cls(ClassSecret, StatusRejected), Unclassified},
		{cls(ClassSecret, StatusExpired), Unclassified},
		{Classification{}, Unclassified},
	}
	for _, c := range cases {
		if got := c.c.Effective(); got != c.want {
			t.Errorf("%s/%s effective %q, want %q", c.c.Class, c.c.Status, got, c.want)
		}
	}
}

func TestRelationClassesOf(t *testing.T) {
	table := cls(ClassUntrusted, StatusConfirmed)
	rc := RelationClasses{RelID: 10, Table: &table, Columns: map[int16]Classification{
		1: cls(ClassClean, StatusConfirmed),
		2: cls(ClassSecret, StatusConfirmed),
		3: cls(ClassClean, StatusProposed),
	}}
	if e := rc.Of(1); e.Class != ClassUntrusted || !e.Confirmed {
		t.Errorf("clean column in an untrusted table: %+v", e)
	}
	if e := rc.Of(2); e.Class != ClassSecret || !e.Confirmed {
		t.Errorf("secret column: %+v", e)
	}
	if e := rc.Of(4); e.Class != ClassUntrusted || !e.Confirmed {
		t.Errorf("unlisted column inherits the table class: %+v", e)
	}
	empty := RelationClasses{}
	if e := empty.Of(1); e.Class != Unclassified || e.Confirmed {
		t.Errorf("no facts: %+v", e)
	}
	pending := cls(ClassUntrusted, StatusProposed)
	rc.Table = &pending
	if e := rc.Of(1); e.Class != ClassUntrusted || e.Confirmed {
		t.Errorf("a pending narrowing proposal suspends confirmation: %+v", e)
	}
}

func col(n int16, name string) Column {
	return Column{RelID: 10, AttNum: n, Schema: "app", Table: "users", Name: name,
		Type: "text"}
}

func TestGrantable(t *testing.T) {
	cols := []Column{col(1, "id"), col(2, "email"), col(3, "password"), col(4, "bio"),
		col(5, "nickname"), col(6, "phone")}
	rc := RelationClasses{RelID: 10, Columns: map[int16]Classification{
		1: cls(ClassClean, StatusConfirmed),
		2: cls(ClassPII, StatusConfirmed),
		3: cls(ClassSecret, StatusProposed),
		4: cls(ClassUntrusted, StatusConfirmed),
		6: cls(ClassPII, StatusProposed),
	}}
	none := func(Column) bool { return false }
	emailOnly := func(c Column) bool { return c.Name == "email" }
	cases := []struct {
		env      envbind.Env
		unmasked func(Column) bool
		want     []string
		reasons  map[string]string
	}{
		{envbind.EnvProd, none, []string{"id", "bio"}, map[string]string{
			"email": ReasonPIIWithoutUnmask, "password": ReasonSecret,
			"nickname": ReasonUnclassified, "phone": ReasonPending}},
		{envbind.EnvStage, emailOnly, []string{"id", "email", "bio"}, nil},
		{envbind.EnvDev, none, []string{"id", "email", "bio", "nickname", "phone"},
			map[string]string{"password": ReasonSecret}},
		{envbind.EnvBranch, nil, []string{"id", "email", "bio", "nickname", "phone"}, nil},
		{envbind.Env("qa"), emailOnly, []string{"id", "email", "bio"}, nil},
	}
	for _, c := range cases {
		allowed, excluded := Grantable(c.env, cols, rc, c.unmasked)
		var names []string
		for _, a := range allowed {
			names = append(names, a.Name)
		}
		if !equalStrings(names, c.want) {
			t.Errorf("%s: allowed %v, want %v", c.env, names, c.want)
		}
		if len(allowed)+len(excluded) != len(cols) {
			t.Errorf("%s: %d allowed + %d excluded != %d", c.env, len(allowed),
				len(excluded), len(cols))
		}
		for _, x := range excluded {
			if want, ok := c.reasons[x.Column.Name]; ok && x.Reason != want {
				t.Errorf("%s: %s excluded for %q, want %q", c.env, x.Column.Name,
					x.Reason, want)
			}
		}
	}
	if allowed, excluded := Grantable(envbind.EnvProd, nil, rc, none); len(allowed) != 0 ||
		len(excluded) != 0 {
		t.Error("no columns: nothing allowed or excluded")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestShouldMask(t *testing.T) {
	cases := []struct {
		env      envbind.Env
		class    Class
		unmasked bool
		want     bool
	}{
		{envbind.EnvDev, ClassPII, false, true},
		{envbind.EnvBranch, ClassPII, false, true},
		{envbind.EnvDev, ClassPII, true, false},
		{envbind.EnvDev, ClassClean, false, false},
		{envbind.EnvDev, Unclassified, false, false},
		{envbind.EnvProd, ClassPII, false, false}, // denied there, not masked
		{envbind.EnvStage, ClassPII, false, false},
	}
	for _, c := range cases {
		if got := ShouldMask(c.env, c.class, c.unmasked); got != c.want {
			t.Errorf("%s %s unmasked=%v: %v", c.env, c.class, c.unmasked, got)
		}
	}
}

func TestClassificationHashBindsClassAndStatus(t *testing.T) {
	a := Classification{ID: 7, Column: col(2, "email"), Class: ClassPII,
		Status: StatusProposed, UpdatedAt: time.Unix(1, 0)}
	b := a
	b.Proposals, b.UpdatedAt = 9, time.Unix(2, 0)
	if a.Hash() != b.Hash() {
		t.Error("re-proposals must not change the hash")
	}
	b.Class = ClassSecret
	if a.Hash() == b.Hash() {
		t.Error("a class change must change the hash")
	}
	c := a
	c.Status = StatusConfirmed
	if a.Hash() == c.Hash() {
		t.Error("a status change must change the hash")
	}
}

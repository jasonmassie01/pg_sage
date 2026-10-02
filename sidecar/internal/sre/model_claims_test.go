package sre

import (
	"strings"
	"testing"
)

// Narration (AI-SRE-SPEC §2.3, CHECK-12): claims cite evidence aliases
// the turn was given, every number they state is grounded in the cited
// evidence, and evidence whose stored hash no longer verifies is stale.

func idleRanking() []string { return []string{"idle_in_tx_holder", "ddl_lock_queue"} }

func TestReviewCheck_ClaimBoundaries(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	s := newReviewScope(d, ev, false)
	five := []wireClaim{
		{Text: "pid 4242 is idle in transaction.", EvidenceIDs: []string{"E1"}},
		{Text: "It blocks 2 sessions.", EvidenceIDs: []string{"E1"}},
		{Text: "Its transaction has been open 90 s.", EvidenceIDs: []string{"E1"}},
		{Text: "pid 20 waits behind the root.", EvidenceIDs: []string{"E1"}},
		{Text: "pg_sage recorded no action in the window.", EvidenceIDs: []string{"E3"}},
	}
	r, rej := checkWith(t, s, wireReview{Ranking: idleRanking(), Claims: five}.json())
	if rej != nil || len(r.Claims) != MaxClaims {
		t.Fatalf("five claims: %+v (%+v)", r.Claims, rej)
	}
	six := append(append([]wireClaim(nil), five...),
		wireClaim{Text: "The waits are on relation locks.", EvidenceIDs: []string{"E1"}})
	_, rej = checkWith(t, s, wireReview{Ranking: idleRanking(), Claims: six}.json())
	rejection(t, rej, RejectClaims)
	r, rej = checkWith(t, s, wireReview{Ranking: idleRanking()}.json())
	if rej != nil || len(r.Claims) != 0 {
		t.Fatalf("no claims must be allowed (narration is optional): %+v (%+v)", r, rej)
	}
}

func TestReviewCheck_ClaimNegatives(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	s := newReviewScope(d, ev, false)
	id := string(ev[0].ID)
	cases := map[string]struct {
		claim  wireClaim
		reason string
	}{
		"uncited":          {wireClaim{Text: "pid 4242 is idle."}, RejectClaims},
		"invented number":  {wireClaim{"pid 4242 blocks 7 sessions.", []string{"E1"}}, RejectClaims},
		"number elsewhere": {wireClaim{"pid 4242 is idle.", []string{"E3"}}, RejectClaims},
		"unknown alias":    {wireClaim{"The lock is held.", []string{"E99"}}, RejectUnknownEvidence},
		"raw evidence uuid": {wireClaim{"The lock is held.", []string{id}},
			RejectUnknownEvidence},
		"empty text": {wireClaim{"  ", []string{"E1"}}, RejectClaims},
		"too long": {wireClaim{strings.Repeat("x", MaxClaimRunes+1), []string{"E1"}},
			RejectClaims},
		"control character": {wireClaim{"pid 4242\x00 is idle.", []string{"E1"}},
			RejectClaims},
	}
	for name, c := range cases {
		raw := wireReview{Ranking: idleRanking(), Claims: []wireClaim{c.claim}}.json()
		_, rej := checkWith(t, s, raw)
		if rej == nil || rej.Reason != c.reason {
			t.Errorf("%s: rejection = %+v, want %s", name, rej, c.reason)
		}
	}
	dup := []wireClaim{idleClaim("E1"), idleClaim("E1")}
	_, rej := checkWith(t, s, wireReview{Ranking: idleRanking(), Claims: dup}.json())
	rejection(t, rej, RejectClaims)
}

// Whitespace is normalized: a claim spanning lines is stored as one line.
func TestReviewCheck_ClaimWhitespaceNormalized(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	s := newReviewScope(d, ev, false)
	c := wireClaim{Text: "pid 4242 is idle\n\tin transaction.", EvidenceIDs: []string{"E1"}}
	r, rej := checkWith(t, s, wireReview{Ranking: idleRanking(),
		Claims: []wireClaim{c}}.json())
	if rej != nil || len(r.Claims) != 1 ||
		r.Claims[0].Text != "pid 4242 is idle in transaction." {
		t.Fatalf("claims = %+v (%+v)", r.Claims, rej)
	}
}

// Evidence whose payload no longer matches its stored hash is stale: a
// claim may not cite it.
func TestReviewCheck_StaleEvidenceRejected(t *testing.T) {
	inv, ev, _ := idleChainFixture(t)
	ev[0].Payload = []byte(strings.Replace(string(ev[0].Payload), "4242", "4243", 1))
	d := diagnoseEvidence(t, inv, ev)
	s := newReviewScope(d, ev, false)
	c := wireClaim{Text: "pid 4243 is idle in transaction.", EvidenceIDs: []string{"E1"}}
	_, rej := checkWith(t, s, wireReview{Ranking: idleRanking(),
		Claims: []wireClaim{c}}.json())
	rejection(t, rej, RejectStaleEvidence)
}

// Aliases resolve to the stored evidence ids, in store order.
func TestReviewScope_AliasesResolveInStoreOrder(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	s := newReviewScope(d, ev, false)
	for i, e := range ev {
		alias := "E" + itoa(int64(i+1))
		got, ok := s.evidence.resolve(alias)
		if !ok || got != e.ID {
			t.Fatalf("%s resolves to %s (%v), want %s", alias, got, ok, e.ID)
		}
	}
	if _, ok := s.evidence.resolve("E0"); ok {
		t.Fatal("E0 resolved")
	}
	if _, ok := s.evidence.resolve(string(ev[0].ID)); ok {
		t.Fatal("a raw evidence id resolved as an alias")
	}
}

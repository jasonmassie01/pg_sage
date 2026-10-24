package changereview

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The receipt-side transitions the receipts workstream builds on: landing
// (awaiting → landed or not_applicable), expiry, the receipt decision
// (sealed with receipt_hash, credit moved) and the shadow score. Each is
// one conditional UPDATE; repeating it is ErrAlreadyDecided.

func TestStoreLandingAndReceipt(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	if _, err := st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAConcur,
		Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecideReceipt(f.ctx, r.ID, Receipt{Verdict: ReceiptClean}); !errors.Is(
		err, ErrInvalid) {
		t.Fatalf("receipt before landing = %v, want ErrInvalid", err)
	}
	at := utc(time.Now().Add(-time.Minute))
	l := Landing{Source: LandingSourceCatalog, Lower: at.Add(-10 * time.Minute), At: at}
	landed, err := st.MarkLanded(f.ctx, r.ID, l)
	if err != nil {
		t.Fatalf("MarkLanded: %v", err)
	}
	if landed.LandingState != LandingLanded || !landed.LandedAt.Equal(at) ||
		!landed.LandedLower.Equal(l.Lower) || landed.LandingSource != LandingSourceCatalog {
		t.Fatalf("landed = %+v", landed)
	}
	if _, err := st.MarkLanded(f.ctx, r.ID, l); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second landing = %v, want ErrAlreadyDecided", err)
	}
	decided, err := st.DecideReceipt(f.ctx, r.ID, Receipt{Verdict: ReceiptClean,
		WindowHours: 24, Tolerance: "met"})
	if err != nil {
		t.Fatalf("DecideReceipt: %v", err)
	}
	if decided.ReceiptVerdict != ReceiptClean || decided.ReceiptDecidedAt == nil ||
		decided.CreditState != CreditFinal || decided.CreditFinalAt == nil ||
		decided.CreditedMinutes != 15 {
		t.Fatalf("decided = %+v", decided)
	}
	stored, _ := st.Get(f.ctx, r.ID)
	if !VerifyReceipt(stored) {
		t.Fatal("the stored decided review does not verify")
	}
	if _, err := st.DecideReceipt(f.ctx, r.ID, Receipt{Verdict: ReceiptRegressed}); !errors.Is(
		err, ErrAlreadyDecided) {
		t.Fatalf("second receipt = %v, want ErrAlreadyDecided", err)
	}
	if again, _ := st.Get(f.ctx, r.ID); again.ReceiptVerdict != ReceiptClean {
		t.Fatalf("a later receipt changed %s", again.ReceiptVerdict)
	}
}

// CHECK-FND-04: VerifyReceipt fails after any jsonb column is changed.
func TestVerifyReceiptDetectsTamperedJSONB(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	columns := map[string]string{
		"statements":     `'[{"index":1,"origin":"sql_input","sql":"DROP TABLE x","kind":"x"}]'`,
		"findings":       `'[]'`,
		"evidence":       `'[]'`,
		"dropped":        `'[{"kind":"claim","text":"x","reason":"uncited"}]'`,
		"not_determined": `'[{"subject":"x","reason":"y"}]'`,
		"summary":        `'{"critical":9}'`,
		"expectations":   `'{"watch_queryids":[1]}'`,
		"receipt":        `jsonb_set(receipt, '{verdict}', '"regressed"')`,
	}
	for col, value := range columns {
		r := completeReview(t, f, st, func(r *Review) {
			r.Findings = []Finding{ruleFinding("CR-13", SeverityWarning)}
			r.Evidence = []Evidence{mustEvidence(t, "stmt", "1", "s", map[string]any{"n": 1})}
		})
		at := utc(time.Now())
		if _, err := st.MarkLanded(f.ctx, r.ID, Landing{Source: LandingSourceReported,
			Lower: at, At: at}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DecideReceipt(f.ctx, r.ID, Receipt{Verdict: ReceiptClean}); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Get(f.ctx, r.ID); !VerifyReceipt(got) {
			t.Fatalf("%s: the untouched review does not verify", col)
		}
		f.exec(`UPDATE sage.change_review SET `+col+` = `+value+` WHERE id = $1`, r.ID)
		if got, _ := st.Get(f.ctx, r.ID); VerifyReceipt(got) {
			t.Errorf("changing %s with UPDATE still verifies", col)
		}
	}
}

func TestStoreExpireAndNotApplicable(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	if _, err := st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAConcur,
		Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	exp, err := st.Expire(f.ctx, r.ID)
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if exp.LandingState != LandingExpired || exp.ReceiptVerdict != ReceiptUnverifiable ||
		exp.CreditState != CreditVoid || exp.ReceiptHash == "" || !VerifyReceipt(exp) {
		t.Fatalf("expired = %+v", exp)
	}
	if _, err := st.Expire(f.ctx, r.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second expiry = %v", err)
	}
	na := completeReview(t, f, st, nil)
	got, err := st.MarkNotApplicable(f.ctx, na.ID)
	if err != nil || got.LandingState != LandingNotApplicable {
		t.Fatalf("not applicable = %+v, %v", got, err)
	}
	if _, err := st.MarkLanded(f.ctx, na.ID, Landing{Source: LandingSourceReported,
		Lower: time.Now(), At: time.Now()}); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("landing a not_applicable review = %v", err)
	}
}

// A change already in place when it was reviewed never lands, so no
// receipt can settle a credit: a concurred review that becomes
// not_applicable holds no pending credit (which retention would keep and
// the Value page would show as pending forever), and a concur after it
// earns none.
func TestNotApplicableHoldsNoCredit(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	dec, err := st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAConcur, Actor: "user:1"})
	if err != nil || dec.CreditState != CreditPending || dec.CreditedMinutes != 15 {
		t.Fatalf("concur = %+v, %v", dec, err)
	}
	na, err := st.MarkNotApplicable(f.ctx, r.ID)
	if err != nil {
		t.Fatalf("MarkNotApplicable: %v", err)
	}
	if na.CreditState != CreditNone || na.CreditedMinutes != 0 ||
		na.ToilModelVersion != nil || na.DBAVerdict != DBAConcur {
		t.Fatalf("not applicable after concur = credit %s %d min v%v, verdict %q",
			na.CreditState, na.CreditedMinutes, na.ToilModelVersion, na.DBAVerdict)
	}
	later := completeReview(t, f, st, nil)
	if _, err := st.MarkNotApplicable(f.ctx, later.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.Decide(f.ctx, later.ID, DBAVerdict{Verdict: DBAConcur, Actor: "user:2"})
	if err != nil || got.CreditState != CreditNone || got.CreditedMinutes != 0 {
		t.Fatalf("concur on a not_applicable review = %+v, %v", got, err)
	}
}

func TestStoreLandingValidation(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	now := time.Now()
	for name, l := range map[string]Landing{
		"unknown source": {Source: "slack", Lower: now, At: now},
		"lower after at": {Source: LandingSourceCatalog, Lower: now, At: now.Add(-time.Second)},
		"zero at":        {Source: LandingSourceCatalog},
	} {
		if _, err := st.MarkLanded(f.ctx, r.ID, l); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: MarkLanded = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := st.DecideReceipt(f.ctx, r.ID, Receipt{Verdict: ReceiptPending}); !errors.Is(
		err, ErrInvalid) {
		t.Errorf("a pending receipt verdict = %v, want ErrInvalid", err)
	}
}

func TestStoreShadowScoreOnce(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, func(r *Review) { r.ShadowAutoClear = ptr(true) })
	if _, err := st.SetShadowScore(f.ctx, r.ID, "great"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown score = %v", err)
	}
	if _, err := st.SetShadowScore(f.ctx, r.ID, ShadowUnscored); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scoring as unscored = %v", err)
	}
	got, err := st.SetShadowScore(f.ctx, r.ID, ShadowCorrect)
	if err != nil || got.ShadowScore != ShadowCorrect {
		t.Fatalf("score = %+v, %v", got, err)
	}
	if _, err := st.SetShadowScore(f.ctx, r.ID, ShadowIncorrect); !errors.Is(err,
		ErrAlreadyDecided) {
		t.Fatalf("second score = %v", err)
	}
}

func TestStoreReceiptRace(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	at := utc(time.Now())
	if _, err := st.MarkLanded(f.ctx, r.ID, Landing{Source: LandingSourceReported, Lower: at,
		At: at}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 6)
	verdicts := []string{ReceiptClean, ReceiptRegressed, ReceiptUnverifiable,
		ReceiptClean, ReceiptCleanLowTraffic, ReceiptRegressed}
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = st.DecideReceipt(f.ctx, r.ID, Receipt{Verdict: verdicts[i]})
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d receipts decided, want exactly 1", won)
	}
}

func TestStoreQueues(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	old := utc(time.Now().Add(-48 * time.Hour))
	awaiting := completeReview(t, f, st, nil)
	due := completeReview(t, f, st, nil)
	fresh := completeReview(t, f, st, nil)
	if _, err := st.MarkLanded(f.ctx, due.ID, Landing{Source: LandingSourceCatalog,
		Lower: old, At: old}); err != nil {
		t.Fatal(err)
	}
	now := utc(time.Now())
	if _, err := st.MarkLanded(f.ctx, fresh.ID, Landing{Source: LandingSourceCatalog,
		Lower: now, At: now}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListAwaiting(f.ctx, 200)
	if err != nil || len(list) != 1 || list[0].ID != awaiting.ID {
		t.Fatalf("awaiting = %v, %v", ids(list), err)
	}
	dueList, err := st.ListReceiptsDue(f.ctx, time.Now().Add(-24*time.Hour), 200)
	if err != nil || len(dueList) != 1 || dueList[0].ID != due.ID {
		t.Fatalf("due = %v, %v", ids(dueList), err)
	}
	for _, bad := range []int{0, -1, 1001} {
		if _, err := st.ListAwaiting(f.ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ListAwaiting(%d) = %v", bad, err)
		}
	}
}

func ids(rs []Review) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func TestStoreToilModel(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	tm, err := st.ToilModel(f.ctx)
	if err != nil || tm.Minutes != 15 || tm.Version != 1 {
		t.Fatalf("toil model = %+v, %v", tm, err)
	}
}

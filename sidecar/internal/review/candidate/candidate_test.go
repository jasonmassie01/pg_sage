package candidate

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestNewID_IsStableAndCoversEachPart(t *testing.T) {
	id := NewID("indexes", "unused", "public.t_a", dropSQL)
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("id %q is not 64 lower-hex characters", id)
	}
	if again := NewID("indexes", "unused", "public.t_a", dropSQL); again != id {
		t.Fatalf("same parts gave %s then %s", id, again)
	}
	others := []string{
		NewID("config", "unused", "public.t_a", dropSQL),
		NewID("indexes", "duplicate", "public.t_a", dropSQL),
		NewID("indexes", "unused", "public.t_b", dropSQL),
		NewID("indexes", "unused", "public.t_a", dropSQL+" "),
		// the separator keeps "ab"+"c" apart from "a"+"bc"
		NewID("indexesu", "nused", "public.t_a", dropSQL),
	}
	for i, o := range others {
		if o == id {
			t.Errorf("variant %d gave the same id", i)
		}
	}
}

func TestValidate_AcceptsAValidDrop(t *testing.T) {
	c := dropCandidate()
	if err := c.Validate(testPack(t)); err != nil {
		t.Fatalf("valid drop refused: %v", err)
	}
	if c.Approval != ApprovalAutomatic || c.ApprovalReason != "" {
		t.Fatalf("a reversible drop was moved to %q (%q)", c.Approval, c.ApprovalReason)
	}
}

func TestValidate_RefusesEachInvalidCandidateNamingIt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Candidate)
		want   []string
	}{
		{"unknown action type", func(c *Candidate) { c.ActionType = "drop_everything" },
			[]string{"drop_everything", "not a registered"}},
		{"empty action type", func(c *Candidate) { c.ActionType = "" },
			[]string{"action type"}},
		{"no evidence", func(c *Candidate) { c.EvidenceIDs = nil },
			[]string{"no evidence"}},
		{"unresolved evidence", func(c *Candidate) {
			c.EvidenceIDs = []string{factID, "E999"}
		}, []string{"E999"}},
		{"empty SQL", func(c *Candidate) { c.ForwardSQL = "  " },
			[]string{"empty forward SQL", "drop_unused_index"}},
		{"SQL of another type", func(c *Candidate) {
			c.ForwardSQL = "CREATE INDEX CONCURRENTLY t_b ON public.t (b);"
		}, []string{"drop_unused_index", "create_index_concurrently"}},
		{"tampered id", func(c *Candidate) { c.ID = strings.Repeat("0", 64) },
			[]string{"id"}},
	}
	pack := testPack(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dropCandidate()
			tc.mutate(&c)
			if tc.name != "tampered id" {
				c.ID = NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL)
			}
			err := c.Validate(pack)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			for _, w := range append(tc.want, "public.t_a") {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestValidate_NilPackIsAnError(t *testing.T) {
	c := dropCandidate()
	if err := c.Validate(nil); err == nil || !strings.Contains(err.Error(), "pack") {
		t.Fatalf("nil pack: err = %v, want an error naming the pack", err)
	}
}

func TestValidate_AdviceAndOperatorNeedNoSQL(t *testing.T) {
	for _, at := range []string{ActionAdvice, ActionOperator} {
		c := dropCandidate()
		c.ActionType, c.ForwardSQL, c.RollbackSQL = at, "", ""
		if at == ActionOperator {
			// review-02b: an operator candidate says what the operator runs
			// and how it lands (plan-02d refuses an operator step without).
			c.Operator = &Operator{Command: "run it", Setting: "work_mem", Value: "8MB"}
		}
		c.ID = NewID(c.Generator, c.Kind, c.Target, c.IDBody())
		if err := c.Validate(testPack(t)); err != nil {
			t.Errorf("%s without SQL refused: %v", at, err)
		}
		if c.Approval != ApprovalRequired {
			t.Errorf("%s approval = %q, want %q", at, c.Approval, ApprovalRequired)
		}
	}
}

func TestValidate_NeverAutomaticStepIsForcedToApproval(t *testing.T) {
	c := dropCandidate()
	c.ActionType = "alter_table"
	c.ForwardSQL = "ALTER TABLE public.t ALTER COLUMN a TYPE bigint;"
	c.RollbackSQL = "ALTER TABLE public.t ALTER COLUMN a TYPE integer;"
	c.ID = NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL)
	if err := c.Validate(testPack(t)); err != nil {
		t.Fatalf("type change refused instead of gated: %v", err)
	}
	if c.Approval != ApprovalRequired || !strings.Contains(c.ApprovalReason, "type_change") {
		t.Fatalf("approval %q (%q), want %q with type_change", c.Approval,
			c.ApprovalReason, ApprovalRequired)
	}
}

func TestValidate_EmptyApprovalFailsClosed(t *testing.T) {
	c := dropCandidate()
	c.Approval = ""
	if err := c.Validate(testPack(t)); err != nil {
		t.Fatal(err)
	}
	if c.Approval != ApprovalRequired {
		t.Fatalf("empty approval became %q, want %q", c.Approval, ApprovalRequired)
	}
}

func TestValidate_UnknownApprovalIsRefused(t *testing.T) {
	c := dropCandidate()
	c.Approval = "sometimes"
	if err := c.Validate(testPack(t)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approval %q: err = %v, want ErrInvalid", c.Approval, err)
	}
}

// Concurrent access: a pack is shared read-only by every validation.
func TestValidate_ConcurrentValidationsOfOnePack(t *testing.T) {
	pack := testPack(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dropCandidate()
			errs <- c.Validate(pack)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

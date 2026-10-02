package changefeed

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Signed change events (AI-SRE-SPEC §4 R1.1, Codex §5/§6): deploys,
// migrations, feature flags and other external changes arrive as bounded,
// validated submissions. Text is untrusted data: bounded, never control
// characters, links only http(s) without credentials.

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func validSubmission() Submission {
	return Submission{Source: "github-actions", EventID: "run-4711", Kind: KindDeploy,
		Service: "checkout", Summary: "deploy checkout v1.2.3",
		Link: "https://ci.example.com/runs/4711", Objects: []string{"public.orders"},
		OccurredAt: t0.Add(-2 * time.Minute)}
}

func TestSubmission_ValidBecomesAVerifiedEvent(t *testing.T) {
	e, err := validSubmission().Event(t0)
	if err != nil {
		t.Fatalf("Event: %v", err)
	}
	if e.Source != "github-actions" || e.EventID != "run-4711" || e.Kind != KindDeploy ||
		e.Service != "checkout" || e.Summary != "deploy checkout v1.2.3" ||
		e.Signature != SignatureVerified || !e.ReceivedAt.Equal(t0) ||
		!e.OccurredAt.Equal(t0.Add(-2*time.Minute)) || len(e.Objects) != 1 {
		t.Fatalf("event = %+v", e)
	}
	if len(e.Hash) != 64 {
		t.Fatalf("hash %q is not hex sha256", e.Hash)
	}
	again, _ := validSubmission().Event(t0.Add(time.Minute))
	if again.Hash != e.Hash {
		t.Fatalf("hash depends on receive time: %s vs %s", again.Hash, e.Hash)
	}
	changed := validSubmission()
	changed.Summary = "deploy checkout v1.2.4"
	other, _ := changed.Event(t0)
	if other.Hash == e.Hash {
		t.Fatal("a changed summary keeps the same hash")
	}
}

// Every external kind is accepted; kinds only pg_sage's own feed emits
// (restarts, failovers, pg_sage actions, ...) cannot be forged.
func TestSubmission_Kinds(t *testing.T) {
	for _, k := range []Kind{KindDeploy, KindMigration, KindFeatureFlag, KindConfig,
		KindOther} {
		s := validSubmission()
		s.Kind = k
		if _, err := s.Event(t0); err != nil {
			t.Errorf("external kind %s rejected: %v", k, err)
		}
		if !k.External() {
			t.Errorf("%s.External() = false", k)
		}
	}
	for _, k := range []Kind{KindDDL, KindSageAction, KindStatsReset, KindRestart,
		KindFailover, KindExtension, "", "reboot"} {
		s := validSubmission()
		s.Kind = k
		if _, err := s.Event(t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("kind %q: err = %v, want ErrInvalid", k, err)
		}
	}
}

func TestSubmission_InvalidInput(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := map[string]func(s *Submission){
		"empty source":       func(s *Submission) { s.Source = "" },
		"upper-case source":  func(s *Submission) { s.Source = "GitHub" },
		"source with space":  func(s *Submission) { s.Source = "git hub" },
		"long source":        func(s *Submission) { s.Source = long(65) },
		"empty event id":     func(s *Submission) { s.EventID = "" },
		"long event id":      func(s *Submission) { s.EventID = long(129) },
		"control event id":   func(s *Submission) { s.EventID = "run\n1" },
		"empty summary":      func(s *Submission) { s.Summary = "" },
		"long summary":       func(s *Submission) { s.Summary = long(301) },
		"control summary":    func(s *Submission) { s.Summary = "deploy\x1b[31m" },
		"long service":       func(s *Submission) { s.Service = long(129) },
		"javascript link":    func(s *Submission) { s.Link = "javascript:alert(1)" },
		"credentials link":   func(s *Submission) { s.Link = "https://u:p@ci.example.com/x" },
		"long link":          func(s *Submission) { s.Link = "https://x.example.com/" + long(500) },
		"too many objects":   func(s *Submission) { s.Objects = make([]string, 21) },
		"empty object":       func(s *Submission) { s.Objects = []string{""} },
		"long object":        func(s *Submission) { s.Objects = []string{long(129)} },
		"zero occurred_at":   func(s *Submission) { s.OccurredAt = time.Time{} },
		"future occurred_at": func(s *Submission) { s.OccurredAt = t0.Add(6 * time.Minute) },
		"ancient occurred":   func(s *Submission) { s.OccurredAt = t0.Add(-8 * 24 * time.Hour) },
	}
	for name, mutate := range cases {
		s := validSubmission()
		mutate(&s)
		if _, err := s.Event(t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// Boundaries: the longest allowed values and the edges of the time
// window are accepted; optional fields may be empty.
func TestSubmission_Boundaries(t *testing.T) {
	s := validSubmission()
	s.Source = "a" + strings.Repeat("b", 63)
	s.EventID = strings.Repeat("e", 128)
	s.Summary = strings.Repeat("s", 300)
	s.Service = ""
	s.Link = ""
	s.Objects = nil
	s.OccurredAt = t0.Add(5 * time.Minute)
	e, err := s.Event(t0)
	if err != nil {
		t.Fatalf("longest values: %v", err)
	}
	if e.Objects == nil || len(e.Objects) != 0 {
		t.Fatalf("objects = %#v, want an empty non-nil list", e.Objects)
	}
	s.OccurredAt = t0.Add(-7 * 24 * time.Hour)
	if _, err := s.Event(t0); err != nil {
		t.Fatalf("occurred exactly 7 days ago: %v", err)
	}
	s.Summary = "déploiement — 結果" // multi-byte runes count as runes
	if _, err := s.Event(t0); err != nil {
		t.Fatalf("unicode summary: %v", err)
	}
}

// Internal events (pg_sage's own sources) carry the internal signature
// status and a deterministic hash.
func TestInternalEvent(t *testing.T) {
	e := internalEvent(KindRestart, "restart:2026-10-01T11:00:00Z",
		"PostgreSQL restarted", t0.Add(-time.Hour), t0)
	if e.Source != SourceSage || e.Signature != SignatureInternal || e.Kind != KindRestart ||
		!e.OccurredAt.Equal(t0.Add(-time.Hour)) || !e.ReceivedAt.Equal(t0) ||
		len(e.Hash) != 64 || e.Objects == nil {
		t.Fatalf("internal event = %+v", e)
	}
	again := internalEvent(KindRestart, "restart:2026-10-01T11:00:00Z",
		"PostgreSQL restarted", t0.Add(-time.Hour), t0.Add(time.Minute))
	if again.Hash != e.Hash {
		t.Fatal("internal hash depends on receive time")
	}
}

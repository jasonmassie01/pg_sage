package agentposture

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func okFinding(id string, sev Severity) Finding {
	return Finding{Detector: id, Severity: sev, ObjectType: "table", Object: "public.t",
		Title: "t is exposed", FixScript: "REVOKE ALL ON public.t FROM anon;"}
}

func TestRunDetector_ReturnsFindingsAndFillsDetectorID(t *testing.T) {
	d := fake("AP-03", Critical)
	f := okFinding("", Critical)
	d.findings = []Finding{f, okFinding("AP-03", Warning)}
	out, err := RunDetector(context.Background(), d, nil, Env{VersionNum: 170000})
	if err != nil {
		t.Fatalf("RunDetector: %v", err)
	}
	if out.Detector != "AP-03" || len(out.Findings) != 2 {
		t.Fatalf("outcome = %+v, want AP-03 with 2 findings", out)
	}
	for _, f := range out.Findings {
		if f.Detector != "AP-03" {
			t.Fatalf("finding detector = %q, want AP-03 filled in", f.Detector)
		}
	}
	if out.Findings[1].Severity != Warning {
		t.Fatalf("severity changed: %s", out.Findings[1].Severity)
	}
}

func TestRunDetector_PassesQuerierEnvAndArms(t *testing.T) {
	var seen Input
	d := fake("AP-06", Warning,
		Arm{Name: "security_invoker", MinVersion: 150000, SkipReason: "PostgreSQL 15+"},
		Arm{Name: "no_security_invoker", MaxVersion: 150000, SkipReason: "before 15 only"})
	d.seen = &seen
	q := &stubQuerier{}
	env := Env{VersionNum: 140011}
	out, err := RunDetector(context.Background(), d, q, env)
	if err != nil {
		t.Fatalf("RunDetector: %v", err)
	}
	if seen.Q != q || seen.Env.VersionNum != 140011 {
		t.Fatalf("input = %+v, want the querier and env passed through", seen)
	}
	if seen.Arm("security_invoker") || !seen.Arm("no_security_invoker") {
		t.Fatalf("arms on PG14: security_invoker=%v no_security_invoker=%v",
			seen.Arm("security_invoker"), seen.Arm("no_security_invoker"))
	}
	if seen.Arm("unknown_arm") {
		t.Fatal("an undeclared arm reports active")
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Arm != "security_invoker" ||
		out.Skipped[0].Reason != "PostgreSQL 15+" {
		t.Fatalf("skipped = %+v, want security_invoker with its reason", out.Skipped)
	}
	if !strings.Contains(out.Note(), "security_invoker skipped: PostgreSQL 15+") {
		t.Fatalf("note = %q, want the recorded skip reason", out.Note())
	}
}

func TestRunDetector_NoArmsMeansNoSkips(t *testing.T) {
	out, err := RunDetector(context.Background(), fake("AP-07", Warning), nil,
		Env{VersionNum: 180000})
	if err != nil || len(out.Skipped) != 0 || out.Note() != "" {
		t.Fatalf("outcome = %+v, %v; want no skips and no note", out, err)
	}
	if out.Findings == nil {
		// nil and empty are both fine for callers, but the outcome must exist.
		out.Findings = []Finding{}
	}
}

func TestRunDetector_ErrorIsWrappedWithTheDetectorID(t *testing.T) {
	base := errors.New("permission denied for table pg_authid")
	d := fake("AP-01", Critical)
	d.err = base
	d.findings = []Finding{okFinding("AP-01", Critical)}
	out, err := RunDetector(context.Background(), d, nil, Env{VersionNum: 170000})
	if !errors.Is(err, base) {
		t.Fatalf("err = %v, want it to wrap the detector's error", err)
	}
	if !strings.Contains(err.Error(), "AP-01") {
		t.Fatalf("err = %q, want the detector id in it", err)
	}
	if len(out.Findings) != 0 {
		t.Fatalf("a failed detector returned %d findings", len(out.Findings))
	}
}

func TestRunDetector_RejectsInvalidFindings(t *testing.T) {
	cases := []struct {
		name string
		f    Finding
	}{
		{"severity above the declared maximum", okFinding("AP-04", Critical)},
		{"unknown severity", okFinding("AP-04", "urgent")},
		{"empty severity", okFinding("AP-04", "")},
		{"another detector's id", okFinding("AP-05", Warning)},
		{"no object", func() Finding { f := okFinding("AP-04", Warning); f.Object = ""; return f }()},
		{"no title", func() Finding { f := okFinding("AP-04", Warning); f.Title = ""; return f }()},
		{"no object type", func() Finding {
			f := okFinding("AP-04", Warning)
			f.ObjectType = ""
			return f
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fake("AP-04", Warning)
			d.findings = []Finding{okFinding("AP-04", Info), c.f}
			out, err := RunDetector(context.Background(), d, nil, Env{VersionNum: 170000})
			if !errors.Is(err, ErrInvalidFinding) {
				t.Fatalf("err = %v, want ErrInvalidFinding", err)
			}
			if !strings.Contains(err.Error(), "AP-04") {
				t.Fatalf("err = %q, want the detector id", err)
			}
			if len(out.Findings) != 0 {
				t.Fatal("an invalid finding let the detector's findings through")
			}
		})
	}
}

func TestRunDetector_NilDetectorAndEndedContext(t *testing.T) {
	if _, err := RunDetector(context.Background(), nil, nil, Env{}); !errors.Is(err,
		ErrInvalidDetector) {
		t.Fatalf("nil detector: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := fake("AP-02", Critical)
	called := Input{}
	d.seen = &called
	if _, err := RunDetector(ctx, d, nil, Env{VersionNum: 170000}); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("ended context: %v, want context.Canceled", err)
	}
	if called.Env.VersionNum != 0 {
		t.Fatal("the detector ran after its context ended")
	}
}

func TestOutcomeNote_JoinsEverySkip(t *testing.T) {
	o := Outcome{Detector: "AP-14", Skipped: []ArmSkip{
		{Arm: "inherit_pg14_15", Reason: "PostgreSQL 14 and 15 only"},
		{Arm: "other", Reason: "needs pg_read_all_stats"}}}
	want := "arm inherit_pg14_15 skipped: PostgreSQL 14 and 15 only; " +
		"arm other skipped: needs pg_read_all_stats"
	if got := o.Note(); got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
}

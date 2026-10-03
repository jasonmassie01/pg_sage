package earned

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Bench report provenance (roadmap 1.1, 2026-10-03): where a report came
// from and which pg_sage build it scored. A report stamped for another
// build is refused; a signed report's certificate must name the commit
// the report names; an unstamped operator report works as before.

// Report origins.
const (
	// OriginSignedRelease: signed by the pg_sage release workflow, the
	// signature verified against the embedded Sigstore trusted root.
	OriginSignedRelease = "signed_release"
	// OriginLocalRun: run by this pg_sage on a clone or a disposable
	// database; it counts only for the families it covered.
	OriginLocalRun = "local_run"
	// OriginOperator: unsigned, uploaded or placed by an operator.
	OriginOperator = "operator"
	// OriginGameDay: a game day on a clone (never bench evidence).
	OriginGameDay = "game_day"
)

// Provenance errors, both invalid reports (HTTP 400) and distinguishable.
var (
	ErrBuildMismatch = fmt.Errorf("%w: report is for another pg_sage build",
		ErrInvalidReport)
	ErrSignatureMismatch = fmt.Errorf("%w: the signature does not cover this report's commit",
		ErrInvalidReport)
)

var (
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// Build is a pg_sage build: its version and the commit it was built from.
type Build struct {
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// Normalized trims both, drops a leading "v" from the version and
// lower-cases the commit; the placeholders "none" and "unknown" (an
// unstamped binary) are no commit.
func (b Build) Normalized() Build {
	v := strings.TrimPrefix(strings.TrimSpace(b.Version), "v")
	c := strings.ToLower(strings.TrimSpace(b.Commit))
	if c == "none" || c == "unknown" {
		c = ""
	}
	return Build{Version: v, Commit: c}
}

// Known reports whether the build names a version or a commit.
func (b Build) Known() bool {
	n := b.Normalized()
	return n.Version != "" || n.Commit != ""
}

// Matches reports whether a report stamped b scores the running build:
// the same commit when both name one, else the same version.
func (b Build) Matches(running Build) bool {
	r, x := b.Normalized(), running.Normalized()
	if r.Commit != "" && x.Commit != "" {
		return r.Commit == x.Commit
	}
	return r.Version != "" && r.Version == x.Version
}

func (b Build) String() string {
	n := b.Normalized()
	var parts []string
	if n.Version != "" {
		parts = append(parts, "pg_sage "+n.Version)
	}
	if n.Commit != "" {
		parts = append(parts, "commit "+n.Commit[:min(7, len(n.Commit))])
	}
	return strings.Join(parts, ", ")
}

func (b Build) validate() error {
	switch {
	case b.Version != "" && !versionPattern.MatchString(b.Version):
		return fmt.Errorf("%w: pg_sage_version %q", ErrInvalidReport, b.Version)
	case b.Commit != "" && !commitPattern.MatchString(b.Commit):
		return fmt.Errorf("%w: pg_sage_commit %q", ErrInvalidReport, b.Commit)
	}
	return nil
}

// ReportSignature is what a verified report signature proves.
type ReportSignature struct {
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
	// Commit is the source commit the signing certificate names (empty
	// when it names none).
	Commit   string    `json:"commit,omitempty"`
	SignedAt time.Time `json:"signed_at"`
}

// BenchIngest says where a bench report comes from.
type BenchIngest struct {
	Origin string
	Actor  string
	// Signature is the verified signature (OriginSignedRelease only).
	Signature *ReportSignature
	// RequireBuild: the report was shipped with this pg_sage and must
	// name the running build.
	RequireBuild bool
}

func (in BenchIngest) validate() error {
	switch {
	case in.Origin != OriginSignedRelease && in.Origin != OriginLocalRun &&
		in.Origin != OriginOperator:
		return fmt.Errorf("%w: bench origin %q", ErrInvalidRequest, in.Origin)
	case (in.Origin == OriginSignedRelease) != (in.Signature != nil):
		return fmt.Errorf("%w: only a signed release report carries a signature",
			ErrInvalidRequest)
	case strings.TrimSpace(in.Actor) == "" || len(in.Actor) > 200:
		return fmt.Errorf("%w: actor", ErrInvalidRequest)
	}
	return nil
}

// IngestBench stores a PGIncidentBench report as bench evidence with its
// provenance. A report naming another build than the running one is
// refused (ErrBuildMismatch); signed, local-run and shipped reports must
// name it; a signature must cover the report's commit
// (ErrSignatureMismatch). A report already stored that arrives signed is
// marked signed.
func (s *Service) IngestBench(ctx context.Context, raw []byte, in BenchIngest) (EvalRun,
	error) {
	if err := in.validate(); err != nil {
		return EvalRun{}, err
	}
	run, err := ParseBenchReport(raw, s.now())
	if err != nil {
		return EvalRun{}, err
	}
	if err := s.checkProvenance(run, in); err != nil {
		return EvalRun{}, err
	}
	run.ID, run.Source, run.IngestedAt, run.IngestedBy = newID(), SourceBench, s.now(),
		in.Actor
	run.Origin, run.Signature = in.Origin, in.Signature
	stored, err := s.store.insertEvalRun(ctx, run)
	s.invalidate()
	return stored.WithProvenance(), err
}

func (s *Service) checkProvenance(run EvalRun, in BenchIngest) error {
	stampRequired := in.RequireBuild || in.Origin != OriginOperator
	switch {
	case !run.Build.Known() && stampRequired:
		return fmt.Errorf("%w: the report names no pg_sage build (this is %s)",
			ErrBuildMismatch, s.runningBuild())
	case run.Build.Known() && !run.Build.Matches(s.cfg.Build):
		return fmt.Errorf("%w: it scores %s, this is %s", ErrBuildMismatch, run.Build,
			s.runningBuild())
	case in.Signature != nil && in.Signature.Commit != "" &&
		strings.ToLower(in.Signature.Commit) != run.Build.Commit:
		return fmt.Errorf("%w: signed for commit %s, report names %q", ErrSignatureMismatch,
			in.Signature.Commit, run.Build.Commit)
	}
	return nil
}

func (s *Service) runningBuild() string {
	if b := s.cfg.Build.String(); b != "" {
		return b
	}
	return "an unstamped pg_sage build"
}

// Build is the running pg_sage build the ledger holds reports to.
func (s *Service) Build() Build { return s.cfg.Build.Normalized() }

// ProvenanceLabel says in words where a report came from.
func (r EvalRun) ProvenanceLabel() string {
	build := r.Build.String()
	switch r.Origin {
	case OriginSignedRelease:
		return fmt.Sprintf("signed release report (%s)", build)
	case OriginLocalRun:
		return fmt.Sprintf("local run (%s)", build)
	case OriginGameDay:
		return "game day"
	default:
		return "unsigned (operator-provided)"
	}
}

// WithProvenance is r with its provenance label set.
func (r EvalRun) WithProvenance() EvalRun {
	r.Provenance = r.ProvenanceLabel()
	return r
}

// BenchSummary is a family's bench evidence without its cells.
type BenchSummary struct {
	ID          string    `json:"id"`
	Origin      string    `json:"origin"`
	Provenance  string    `json:"provenance"`
	Signed      bool      `json:"signed"`
	Build       Build     `json:"build"`
	GeneratedAt time.Time `json:"generated_at"`
	IngestedAt  time.Time `json:"ingested_at"`
	// Families are the families the report scored.
	Families []string `json:"families"`
}

// SummarizeBench summarizes a report; nil without one.
func SummarizeBench(run *EvalRun) *BenchSummary {
	if run == nil {
		return nil
	}
	families := []string{}
	for _, c := range run.Cells {
		if c.Family != "all" && !slices.Contains(families, c.Family) {
			families = append(families, c.Family)
		}
	}
	slices.Sort(families)
	return &BenchSummary{ID: run.ID, Origin: run.Origin, Provenance: run.ProvenanceLabel(),
		Signed: run.Signature != nil, Build: run.Build, GeneratedAt: run.GeneratedAt,
		IngestedAt: run.IngestedAt, Families: families}
}

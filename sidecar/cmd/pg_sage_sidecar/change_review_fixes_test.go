package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/config"
)

// The GitHub-side fixes: the status job gates on the reviews of the pull
// request's head commit, the most restrictive first, so a decoy review
// cannot turn a risky status green; GitHub mode finds its own comment
// under a personal token; the action and example workflow keep one
// fail-on and leave the verdict to the status; the UI address reaches
// the comment and the notification.

// listing answers list_change_reviews with reviews and get_change_review
// with the one asked for.
func listing(reviews ...map[string]any) func(string, map[string]any) (map[string]any, int) {
	return func(tool string, args map[string]any) (map[string]any, int) {
		if tool == "list_change_reviews" {
			items := make([]any, 0, len(reviews))
			for _, r := range reviews {
				items = append(items, r)
			}
			return map[string]any{"reviews": items}, 0
		}
		for _, r := range reviews {
			if r["id"] == args["id"] {
				return r, 0
			}
		}
		return nil, -32002
	}
}

func statusJob(t *testing.T, g *fakeGitHub, m *crMCP) int {
	t.Helper()
	e := githubEnv(t, g, `{"schedule":"*/15 * * * *"}`)
	return e.run(t, "status", "--github", "--url", m.srv.URL, "--database", "d",
		"--fail-on", "risky")
}

func TestGitHubStatusJobIgnoresDecoyReviews(t *testing.T) {
	g := newFakeGitHub(t)
	actual := crReview(crReviewID, "complete", "risky")
	actual["created_at"] = "2026-10-23T10:00:00Z"
	noHead := crReview("3f0c8a52-1111-4000-8000-0000000000b1", "complete", "safe")
	noHead["head_sha"], noHead["created_at"] = "", "2026-10-23T11:00:00Z"
	sameHead := crReview("3f0c8a52-1111-4000-8000-0000000000b2", "complete", "safe")
	sameHead["created_at"] = "2026-10-23T11:30:00Z"
	m := newCRMCP(t, listing(sameHead, noHead, actual))
	if code := statusJob(t, g, m); code != 0 {
		t.Fatalf("status job exit %d", code)
	}
	comments, statuses, _ := g.snapshot()
	if len(statuses) != 1 || statuses[0].State != "failure" ||
		!strings.Contains(statuses[0].Description, "risky") {
		t.Fatalf("statuses %+v, want the risky review's failure", statuses)
	}
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "id="+crReviewID) {
		t.Fatalf("comment %+v, want the gating review's", comments)
	}
	g2 := newFakeGitHub(t)
	if code := statusJob(t, g2, newCRMCP(t, listing(noHead))); code != 0 {
		t.Fatalf("status job exit %d", code)
	}
	if _, statuses, writes := g2.snapshot(); len(statuses) != 0 || len(writes) != 0 {
		t.Fatalf("a review that names no head commit gated the PR: %+v %v", statuses, writes)
	}
}

// A DBA verdict on the gating review still turns the status green; a
// newer review of the same head that is worse keeps it red.
func TestGitHubStatusJobTakesTheMostRestrictiveReviewOfTheHead(t *testing.T) {
	g := newFakeGitHub(t)
	decided := crReview(crReviewID, "complete", "risky")
	decided["dba_verdict"], decided["dba_actor"] = "concur", "user:2"
	if code := statusJob(t, g, newCRMCP(t, listing(decided))); code != 0 {
		t.Fatalf("status job exit %d", code)
	}
	_, statuses, _ := g.snapshot()
	if last := statuses[len(statuses)-1]; last.State != "success" {
		t.Fatalf("a concurred risky review: %+v", last)
	}
	worse := crReview("3f0c8a52-1111-4000-8000-0000000000c1", "complete", "risky")
	worse["created_at"] = "2026-10-23T11:00:00Z"
	if code := statusJob(t, g, newCRMCP(t, listing(worse, decided))); code != 0 {
		t.Fatalf("status job exit %d", code)
	}
	_, statuses, _ = g.snapshot()
	if last := statuses[len(statuses)-1]; last.State != "failure" {
		t.Fatalf("an undecided risky review of the same head: %+v, want failure", last)
	}
}

// Under a personal token GitHub mode edits the comment its own account
// wrote (GET /user names it); another person's marker comment stays.
func TestGitHubModeEditsItsOwnCommentUnderAPersonalToken(t *testing.T) {
	g := newFakeGitHub(t)
	g.user = "sage-ci"
	mine := ghComment{ID: 21, Body: "<!-- pg_sage-review db=d id=x receipt=pending -->\nold"}
	mine.User.Login, mine.User.Type = "sage-ci", "User"
	theirs := ghComment{ID: 22, Body: "<!-- pg_sage-review db=d id=y receipt=pending -->"}
	theirs.User.Login, theirs.User.Type = "mallory", "User"
	g.comments = []ghComment{theirs, mine}
	if code, e := ghSubmit(t, g, newCRMCP(t, verdictTools("safe")), "risky"); code != 0 {
		t.Fatalf("exit %d (%s)", code, e.stderr)
	}
	comments, _, writes := g.snapshot()
	if len(comments) != 2 || writes[0] != "PATCH 21" || comments[0].Body != theirs.Body {
		t.Fatalf("writes %v comments %+v: want the own comment edited, no new one", writes,
			comments)
	}
}

// The runner's own token cannot read /user (403): the bot's comment is
// still found by its type.
func TestGitHubModeWithTheRunnersTokenStillFindsTheBotComment(t *testing.T) {
	g := newFakeGitHub(t)
	bot := ghComment{ID: 31, Body: "<!-- pg_sage-review db=d id=x receipt=pending -->"}
	bot.User.Login, bot.User.Type = "github-actions[bot]", "Bot"
	g.comments = []ghComment{bot}
	if code, e := ghSubmit(t, g, newCRMCP(t, verdictTools("safe")), "risky"); code != 0 {
		t.Fatalf("exit %d (%s)", code, e.stderr)
	}
	if _, _, writes := g.snapshot(); writes[0] != "PATCH 31" {
		t.Fatalf("writes %v, want the bot comment edited", writes)
	}
}

// The example workflow names fail-on once and both jobs use it; the
// action leaves the verdict to the commit status in GitHub mode unless
// fail-job asks it to fail the job too.
func TestExampleWorkflowAndActionKeepOneGate(t *testing.T) {
	text := readRepoFile(t, "integrations/github/example-workflow.yml")
	var wf struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Steps []ghStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Env["PG_SAGE_FAIL_ON"] == "" {
		t.Fatalf("workflow env %v lacks PG_SAGE_FAIL_ON", wf.Env)
	}
	if strings.Count(text, "--fail-on risky")+strings.Count(text, "fail-on: risky") != 0 {
		t.Fatal("the example hardcodes fail-on; both jobs must use PG_SAGE_FAIL_ON")
	}
	uses := 0
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			if strings.Contains(s.With["fail-on"], "env.PG_SAGE_FAIL_ON") ||
				strings.Contains(s.Run, "PG_SAGE_FAIL_ON") {
				uses++
			}
		}
	}
	if uses < 2 {
		t.Fatalf("PG_SAGE_FAIL_ON is used by %d steps, want the review and the status job",
			uses)
	}
	action := readRepoFile(t, "integrations/github/pg-sage-review/action.yml")
	var a struct {
		Inputs map[string]struct {
			Default string `yaml:"default"`
		} `yaml:"inputs"`
	}
	if err := yaml.Unmarshal([]byte(action), &a); err != nil {
		t.Fatal(err)
	}
	if a.Inputs["fail-job"].Default != "false" {
		t.Fatalf("fail-job defaults to %q, want false", a.Inputs["fail-job"].Default)
	}
	for _, want := range []string{"FAIL_JOB", "status=$?", `"$status" -eq 1`} {
		if !strings.Contains(action, want) {
			t.Errorf("the action lacks %q (exit 1 left to the status)", want)
		}
	}
}

func TestChangeReviewRenderUsesTheUIAddress(t *testing.T) {
	cfg := config.DefaultConfig()
	if got := changeReviewRender(cfg); got.UIBaseURL != "" {
		t.Fatalf("default UI address %q, want none", got.UIBaseURL)
	}
	cfg.ChangeReview.UIBaseURL = "https://sage.example.com"
	if got := changeReviewRender(cfg); got.UIBaseURL != "https://sage.example.com" {
		t.Fatalf("UI address %q", got.UIBaseURL)
	}
}

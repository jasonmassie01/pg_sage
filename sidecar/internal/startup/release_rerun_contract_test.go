package startup

import "testing"

// ci-05: a rerun of the release job (a flaky Rekor call during
// verify-blob, a failed smoke test) runs goreleaser again for the same
// tag. Without replace_existing_draft goreleaser adds a second draft, and
// "gh release edit <tag> --draft=false" may publish the stale draft the
// failed run left instead of the one this run verified.

// No concurrent-access tests: this reads a committed config file.

func TestReleaseRerunReplacesTheDraft(t *testing.T) {
	rel := loadGoreleaser(t).Release
	if !rel.Draft {
		t.Fatal(".goreleaser.yml release.draft is false: the release is public " +
			"before its smoke test")
	}
	if !rel.ReplaceExistingDraft {
		t.Fatal(".goreleaser.yml release.replace_existing_draft is not true: a rerun " +
			"leaves two drafts for the tag and the publish step may pick the stale one")
	}
	if rel.UseExistingDraft {
		t.Fatal(".goreleaser.yml release.use_existing_draft is true: a rerun would " +
			"upload into the failed run's draft and keep its stale assets")
	}
	// goreleaser finds the draft to replace by release name, so the name
	// must be the same on every run of a tag (no date, commit or run id).
	if rel.NameTemplate != "v{{.Version}}" {
		t.Fatalf(".goreleaser.yml release.name_template = %q, want \"v{{.Version}}\": "+
			"a name that differs between runs of one tag defeats the draft replacement",
			rel.NameTemplate)
	}
}

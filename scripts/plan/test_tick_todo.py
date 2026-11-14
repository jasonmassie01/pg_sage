"""Tests for tick_todo.py (item todo-stale).

Run with: py -I scripts/plan/test_tick_todo.py

Each test builds a temporary reports tree, a temporary todo.md and a
temporary git repository. The script is loaded by path because `py -I`
leaves this directory off sys.path.

No concurrent-access tests: the script is one process that writes one file
through a temporary file and os.replace.
"""

import contextlib
import importlib.util
import io
import os
import pathlib
import subprocess
import tempfile
import unittest

_HERE = pathlib.Path(__file__).resolve().parent
_SPEC = importlib.util.spec_from_file_location("tick_todo", _HERE / "tick_todo.py")
tick_todo = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(tick_todo)

_GIT_ENV = {
    "GIT_CONFIG_GLOBAL": os.devnull,
    "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_AUTHOR_NAME": "fixture",
    "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
    "GIT_COMMITTER_NAME": "fixture",
    "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
}


def _git(repo, *args):
    env = dict(os.environ, **_GIT_ENV)
    out = subprocess.run(
        ["git", "-C", str(repo), *args],
        check=True, capture_output=True, text=True, env=env,
    )
    return out.stdout.strip()


class Fixture:
    """A git repo with a merged branch, an unmerged branch and a target."""

    def __init__(self, root):
        self.root = pathlib.Path(root)
        self.repo = self.root / "repo"
        self.reports = self.root / "reports"
        self.todo = self.root / "todo.md"
        self.repo.mkdir()
        (self.reports / "t0").mkdir(parents=True)
        (self.reports / "t1").mkdir(parents=True)
        _git(self.repo, "init", "-q", "-b", "improve/integration")
        self._commit("base")
        _git(self.repo, "branch", "improve/t0/merged")
        self.merged_head = _git(self.repo, "rev-parse", "HEAD")
        _git(self.repo, "checkout", "-q", "-b", "improve/t1/unmerged")
        self._commit("unmerged work")
        self.unmerged_head = _git(self.repo, "rev-parse", "HEAD")
        _git(self.repo, "checkout", "-q", "improve/integration")
        self._commit("integration moves on")

    def _commit(self, msg):
        _git(self.repo, "commit", "-q", "--allow-empty", "-m", msg)

    def report(self, tier, item_id, text):
        path = self.reports / tier / (item_id + ".md")
        path.write_text(text, encoding="utf-8")
        return path

    def write_todo(self, lines, newline="\n"):
        data = newline.join(lines) + newline
        self.todo.write_bytes(data.encode("utf-8"))

    def todo_bytes(self):
        return self.todo.read_bytes()

    def run(self, *extra):
        argv = [
            "--todo", str(self.todo),
            "--reports", str(self.reports),
            "--repo", str(self.repo),
            *extra,
        ]
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = tick_todo.main(argv)
        return code, out.getvalue(), err.getvalue()


def _merged_report(item_id, extra=""):
    return (
        f"# {item_id}: a fixed thing\n\n"
        f"Branch `improve/t0/merged`, base `abc1234`.{extra}\n\n"
        "## What changed\n\nStuff.\n"
    )


def _summary(out):
    for line in out.splitlines():
        if line.startswith("done "):
            return line
    raise AssertionError("no summary line in output:\n" + out)


class TickTodoTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    # Happy path -----------------------------------------------------------

    def test_merged_branch_ticks_line_and_also_covers_id(self):
        self.fx.report("t0", "vacuum-01", _merged_report("vacuum-01"))
        self.fx.write_todo([
            "# plan",
            "",
            "- [ ] vacuum-01 Parents read as INT_MAX (also covers rules-01) (collect, S)",
            "- [ ] rules-01 Folded rule (collect, S)",
            "- [ ] other-02 Not reported (collect, S)",
        ])
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        text = self.fx.todo.read_text(encoding="utf-8").splitlines()
        self.assertEqual(text[2][:16], "- [x] vacuum-01 ")
        self.assertEqual(text[3], "- [x] rules-01 Folded rule (collect, S)")
        self.assertEqual(text[4], "- [ ] other-02 Not reported (collect, S)")
        self.assertIn(
            "done 2, partial 0, unmerged 0, no report 1, branch unknown 0, malformed 0",
            _summary(out),
        )
        self.assertIn("changes 2", out)
        self.assertIn("other-02: no report", out)

    def test_dry_run_leaves_file_untouched(self):
        self.fx.report("t0", "vacuum-01", _merged_report("vacuum-01"))
        self.fx.write_todo(["- [ ] vacuum-01 Title (collect, S)"])
        before = self.fx.todo_bytes()
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        self.assertEqual(self.fx.todo_bytes(), before)
        self.assertIn("done 1,", _summary(out))
        self.assertIn("dry run", out)

    def test_head_sha_fallback_when_branch_is_not_merged(self):
        self.fx.report("t1", "a-01", (
            "# a-01\n\nBranch improve/t1/unmerged, base 1234567, "
            f"head {self.fx.merged_head[:8]}.\n"
        ))
        self.fx.report("t1", "b-01", (
            "# b-01\n\nBranch improve/t1/unmerged, base 1234567, "
            f"head {self.fx.unmerged_head[:8]}.\n"
        ))
        self.fx.write_todo(["- [ ] a-01 A (x, S)", "- [ ] b-01 B (x, S)"])
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        lines = self.fx.todo.read_text(encoding="utf-8").splitlines()
        self.assertEqual(lines, ["- [x] a-01 A (x, S)", "- [ ] b-01 B (x, S)"])
        self.assertIn("b-01: branch not merged", out)
        self.assertIn("done 1, partial 0, unmerged 1,", _summary(out))

    def test_lowercase_branch_phrase_and_worktree_layout(self):
        self.fx.report("t0", "c-01", (
            "# c-01\n\nDate: x. Worktree: wt/t0 (branch improve/t0/merged).\n"
        ))
        self.fx.write_todo(["- [ ] c-01 C (x, S)"])
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        self.assertIn("done 1,", _summary(out))

    # Invalid input --------------------------------------------------------

    def test_missing_branch_line_is_branch_unknown(self):
        self.fx.report("t0", "d-01", "# d-01\n\nNo branch named anywhere.\n")
        self.fx.write_todo(["- [ ] d-01 D (x, S)"])
        before = self.fx.todo_bytes()
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        self.assertEqual(self.fx.todo_bytes(), before)
        self.assertIn("d-01: branch unknown", out)
        self.assertIn("branch unknown 1,", _summary(out))

    def test_report_naming_a_missing_branch_is_never_ticked(self):
        self.fx.report("t0", "e-01", "# e-01\n\nBranch `improve/t9/gone`, base x.\n")
        self.fx.write_todo(["- [ ] e-01 E (x, S)"])
        before = self.fx.todo_bytes()
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        self.assertEqual(self.fx.todo_bytes(), before)
        self.assertIn("e-01: branch unknown", out)
        self.assertIn("improve/t9/gone", out)

    def test_malformed_lines_left_untouched_and_reported(self):
        self.fx.report("t0", "f-01", _merged_report("f-01"))
        lines = [
            "- [ ]",
            "- [ ] Not_An_Id title",
            "- [ x] f-01 odd box",
            "- [ ] f-01 F (x, S)",
        ]
        self.fx.write_todo(lines)
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        got = self.fx.todo.read_text(encoding="utf-8").splitlines()
        self.assertEqual(got[:3], lines[:3])
        self.assertEqual(got[3], "- [x] f-01 F (x, S)")
        self.assertIn("malformed 3", _summary(out))
        self.assertIn("line 1: malformed", out)
        self.assertIn("line 2: malformed", out)
        self.assertIn("line 3: malformed", out)

    def test_non_git_repo_exits_2(self):
        plain = self.fx.root / "plain"
        plain.mkdir()
        self.fx.write_todo(["- [ ] g-01 G (x, S)"])
        argv = ["--todo", str(self.fx.todo), "--reports", str(self.fx.reports),
                "--repo", str(plain)]
        err = io.StringIO()
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
            code = tick_todo.main(argv)
        self.assertEqual(code, 2)
        self.assertIn(str(plain), err.getvalue())
        self.assertIn("not a git", err.getvalue())

    def test_missing_todo_and_reports_exit_2(self):
        code, _, err = self.fx.run()  # todo.md was never written
        self.assertEqual(code, 2)
        self.assertIn("todo.md", err)
        self.fx.write_todo(["- [ ] g-01 G (x, S)"])
        argv = ["--todo", str(self.fx.todo), "--reports", str(self.fx.root / "nope"),
                "--repo", str(self.fx.repo)]
        err2 = io.StringIO()
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err2):
            code2 = tick_todo.main(argv)
        self.assertEqual(code2, 2)
        self.assertIn("nope", err2.getvalue())

    # Nil / empty ----------------------------------------------------------

    def test_empty_todo_exits_0_with_zero_counts(self):
        self.fx.write_todo([])
        self.fx.todo.write_bytes(b"")
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        self.assertIn(
            "done 0, partial 0, unmerged 0, no report 0, branch unknown 0, malformed 0",
            _summary(out),
        )
        self.assertEqual(self.fx.todo_bytes(), b"")

    def test_empty_reports_dir_ticks_nothing(self):
        self.fx.write_todo(["- [ ] h-01 H (x, S)", "- [ ] h-02 H2 (x, S)"])
        before = self.fx.todo_bytes()
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        self.assertEqual(self.fx.todo_bytes(), before)
        self.assertIn("done 0, partial 0, unmerged 0, no report 2,", _summary(out))
        self.assertIn("changes 0", out)

    def test_todo_without_unchecked_lines_has_zero_counts(self):
        self.fx.write_todo(["# title", "- [x] i-01 I (x, S)", "- [X] i-02 I (x, S)"])
        before = self.fx.todo_bytes()
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        self.assertEqual(self.fx.todo_bytes(), before)
        self.assertIn(
            "done 0, partial 0, unmerged 0, no report 0, branch unknown 0, malformed 0",
            _summary(out),
        )
        self.assertIn("already ticked 2", out)

    # Boundary -------------------------------------------------------------

    def _status_report(self, item_id, status_line_no, status):
        body = [f"# {item_id}: title", "", "Branch `improve/t0/merged`, base x."]
        while len(body) < status_line_no - 1:
            body.append("filler")
        body.append(status)
        body += ["", "## Rest", "text"]
        return "\n".join(body) + "\n"

    def test_status_partial_on_line_15_counts_and_16_does_not(self):
        self.fx.report("t0", "j-15", self._status_report("j-15", 15, "Status: partial."))
        self.fx.report("t0", "j-16", self._status_report("j-16", 16, "Status: partial."))
        self.fx.write_todo(["- [ ] j-15 J (x, S)", "- [ ] j-16 J (x, S)"])
        code, out, _ = self.fx.run("--write")
        self.assertEqual(code, 0)
        got = self.fx.todo.read_text(encoding="utf-8").splitlines()
        self.assertEqual(got, ["- [ ] j-15 J (x, S)", "- [x] j-16 J (x, S)"])
        self.assertIn("j-15: partial: " + os.path.join("t0", "j-15.md") + ":15", out)
        self.assertIn("done 1, partial 1,", _summary(out))

    def test_status_variants_that_mean_not_done(self):
        variants = {
            "k-01": "Status: **blocked** (needs the owner).",
            "k-02": "Builder status: blocked (disk full).",
            "k-03": "Branch improve/t0/merged, base x. Status: **partial**. More.",
            "k-04": "Status: not built.",
            "k-05": "Status: BLOCKED (machine disk full).",
        }
        for item_id, line in variants.items():
            self.fx.report("t0", item_id, self._status_report(item_id, 5, line))
        self.fx.report("t0", "k-06", self._status_report("k-06", 5, "Status: done."))
        ids = list(variants) + ["k-06"]
        self.fx.write_todo([f"- [ ] {i} K (x, S)" for i in ids])
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        for item_id in variants:
            self.assertIn(f"{item_id}: partial: ", out)
        self.assertIn("done 1, partial 5,", _summary(out))

    def test_env_blocked_and_backticked_status_mean_not_done(self):
        self.fx.report("t0", "k-11", self._status_report("k-11", 5, "Status: env_blocked."))
        self.fx.report("t0", "k-12", self._status_report("k-12", 5, "Status: `partial`."))
        self.fx.write_todo(["- [ ] k-11 K (x, S)", "- [ ] k-12 K (x, S)"])
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        self.assertIn("k-11: partial: " + os.path.join("t0", "k-11.md") + ":5", out)
        self.assertIn("k-12: partial: " + os.path.join("t0", "k-12.md") + ":5", out)
        self.assertIn("done 0, partial 2,", _summary(out))

    def test_also_covers_stops_at_prose(self):
        self.fx.report("t0", "r-01", _merged_report("r-01"))
        self.fx.write_todo([
            "- [ ] r-01 R (also covers s-01, side branches, ledger-pin (the guard is t-30)) (x, M)",
            "- [ ] s-01 Folded (x, S)",
            "- [ ] side Not an id list entry (x, S)",
            "- [ ] t-30 Separate item (x, S)",
        ])
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        self.assertIn("s-01: done: covered by r-01", out)
        self.assertIn("side: no report", out)
        self.assertIn("t-30: no report", out)
        self.assertIn("done 2, partial 0, unmerged 0, no report 2,", _summary(out))

    def test_title_partial_counts(self):
        self.fx.report("t0", "l-01", (
            "# l-01 Converge the stacks (partial)\n\nBranch improve/t0/merged, base x.\n"
        ))
        self.fx.write_todo(["- [ ] l-01 L (x, XL)"])
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        self.assertIn("l-01: partial: " + os.path.join("t0", "l-01.md") + ":1", out)

    def test_check_line_not_built_counts_anywhere(self):
        filler = "\n".join("line" for _ in range(40))
        self.fx.report("t0", "m-01", (
            "# m-01 Ledger\n\nBranch `improve/t0/merged`, base x.\n" + filler +
            "\n- CHECK-08: NOT BUILT - the concurrent builder is left for later.\n"
        ))
        self.fx.write_todo(["- [ ] m-01 M (x, L)"])
        code, out, _ = self.fx.run()
        self.assertEqual(code, 0)
        self.assertIn("m-01: partial: " + os.path.join("t0", "m-01.md") + ":44", out)

    # Error propagation ----------------------------------------------------

    def test_bad_target_ref_fails_loudly_and_ticks_nothing(self):
        self.fx.report("t0", "n-01", _merged_report("n-01"))
        self.fx.write_todo(["- [ ] n-01 N (x, S)"])
        before = self.fx.todo_bytes()
        code, _, err = self.fx.run("--write", "--target", "improve/no-such-target")
        self.assertEqual(code, 3)
        self.assertIn("improve/no-such-target", err)
        self.assertEqual(self.fx.todo_bytes(), before)

    def test_merge_base_failure_raises_naming_the_ref(self):
        git = tick_todo.Git(self.fx.repo)
        target = git.resolve("improve/integration")
        self.assertIsNotNone(target)
        with self.assertRaises(tick_todo.GitError) as ctx:
            git.is_ancestor("0" * 40, target, "improve/t0/broken")
        self.assertIn("improve/t0/broken", str(ctx.exception))
        self.assertIn("merge-base", str(ctx.exception))

    # State transitions ----------------------------------------------------

    def test_second_write_changes_nothing_and_keeps_manual_ticks(self):
        self.fx.report("t0", "o-01", _merged_report("o-01"))
        self.fx.write_todo([
            "- [ ] o-01 O (x, S)",
            "- [x] o-02 Ticked by hand, no report (x, S)",
            "- [ ] o-03 No report (x, S)",
        ], newline="\r\n")
        code1, out1, _ = self.fx.run("--write")
        after_first = self.fx.todo_bytes()
        code2, out2, _ = self.fx.run("--write")
        self.assertEqual((code1, code2), (0, 0))
        self.assertIn("changes 1", out1)
        self.assertIn("changes 0", out2)
        self.assertEqual(self.fx.todo_bytes(), after_first)
        self.assertEqual(after_first, (
            b"- [x] o-01 O (x, S)\r\n"
            b"- [x] o-02 Ticked by hand, no report (x, S)\r\n"
            b"- [ ] o-03 No report (x, S)\r\n"
        ))
        self.assertIn("already ticked 2", out2)

    # Changelog ------------------------------------------------------------

    def test_changelog_lists_done_ids_not_cited_in_unreleased(self):
        for item_id in ("p-01", "p-02", "security-01"):
            self.fx.report("t0", item_id, _merged_report(item_id))
        self.fx.write_todo([
            "- [ ] p-01 P (x, S)",
            "- [ ] p-02 P (x, S)",
            "- [ ] security-01 S (x, S)",
            "- [x] p-03 Ticked by hand (x, S)",
        ])
        changelog = self.fx.root / "CHANGELOG.md"
        changelog.write_text(
            "# Changelog\n\n## [Unreleased]\n\n- Fixed a thing (p-01).\n"
            "- Pinned search_path (security-01a).\n\n"
            "## [2.5.0] - 2026-11-03\n\n- Old (p-02, security-01, p-03).\n",
            encoding="utf-8",
        )
        code, out, _ = self.fx.run("--changelog", str(changelog))
        self.assertEqual(code, 0)
        section = out[out.index("not cited in [Unreleased]"):]
        cited = [ln.strip() for ln in section.splitlines()[1:] if ln.startswith("  ")]
        self.assertEqual(cited, ["p-02", "security-01", "p-03"])

    def test_changelog_without_unreleased_section_exits_2(self):
        self.fx.write_todo(["- [ ] q-01 Q (x, S)"])
        changelog = self.fx.root / "CHANGELOG.md"
        changelog.write_text("# Changelog\n\n## [2.5.0]\n\n- Old.\n", encoding="utf-8")
        code, _, err = self.fx.run("--changelog", str(changelog))
        self.assertEqual(code, 2)
        self.assertIn("[Unreleased]", err)


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Tick the improvement plan's todo.md from the item reports (item todo-stale).

For each "- [ ] <id> ..." line the item counts as done only when all hold:
  * a report reports/t*/<id>.md exists;
  * its title has no "(partial)", its first 15 lines carry no
    "Status: partial|blocked|env_blocked|not built", and no
    "CHECK-NN: NOT BUILT" line;
  * the branch it names ("Branch improve/..." or "branch improve/...") is an
    ancestor of the target (default improve/integration), or failing that the
    "head <sha>" it names is.
Ids listed under "(also covers ...)" on a done line are done with it.

Dry run by default; --write ticks only the checkbox of done lines, through a
temporary file and os.replace. It never unticks a line and is idempotent.
Git is used read-only (rev-parse, merge-base --is-ancestor).

Usage (re-run after each merge into the target):
  py -I scripts/plan/tick_todo.py --todo <plan>/todo.md --reports <reports> \
      --repo <integration worktree> [--write] [--changelog CHANGELOG.md]

Exit codes: 0 ok, 2 bad input (missing file, not a git repository, no
[Unreleased] section), 3 git failure (target does not resolve, git error).
Standard library only (Python 3.11).
"""

import argparse
import os
import pathlib
import re
import subprocess
import sys
import tempfile

EXIT_OK = 0
EXIT_INPUT = 2
EXIT_GIT = 3
HEAD_LINES = 15

_ID = r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*"
_ID_RE = re.compile(rf"^{_ID}$")
_UNCHECKED_RE = re.compile(rf"^- \[ \] (?P<id>{_ID})(?: .*)?$")
_TICKED_RE = re.compile(rf"^- \[[xX]\](?: (?P<id>{_ID})(?: .*)?)?$")
_ALSO_COVERS_RE = re.compile(r"\(also covers ([^)]*)\)")
_TITLE_PARTIAL_RE = re.compile(r"\(partial\)", re.IGNORECASE)
_STATUS_BAD_RE = re.compile(
    r"status:[\s*`]*(?:partial|env_blocked|blocked|not built)\b", re.IGNORECASE)
_CHECK_NOT_BUILT_RE = re.compile(
    r"\bCHECK-\d+[a-z]?[`*:\s\[]*NOT BUILT\b", re.IGNORECASE)
_BRANCH_RE = re.compile(r"\b[Bb]ranch\b[\s:]*`?(improve/[A-Za-z0-9._/-]+)")
_HEAD_RE = re.compile(r"\b[Hh]ead\b[\s:]*`?([0-9a-f]{7,40})\b")

BUCKETS = ("done", "partial", "unmerged", "no report", "branch unknown", "malformed")


class InputError(Exception):
    """Bad input the operator must fix (exit 2)."""


class GitError(Exception):
    """git failed or a ref that must exist does not resolve (exit 3)."""


class Git:
    """Read-only git access to one repository."""

    def __init__(self, repo):
        self.repo = str(repo)
        out = self._run("rev-parse", "--git-dir")
        if out.returncode != 0:
            raise InputError(f"--repo {self.repo} is not a git repository: "
                             f"{out.stderr.strip()}")

    def _run(self, *args):
        try:
            return subprocess.run(["git", "-C", self.repo, *args],
                                  capture_output=True, text=True, check=False)
        except FileNotFoundError as exc:
            raise InputError(f"git is not on PATH: {exc}") from exc

    def resolve(self, ref):
        """Return the commit sha of ref, or None when it does not resolve."""
        out = self._run("rev-parse", "--verify", "--quiet", ref + "^{commit}")
        return out.stdout.strip() if out.returncode == 0 else None

    def is_ancestor(self, sha, target_sha, ref):
        out = self._run("merge-base", "--is-ancestor", sha, target_sha)
        if out.returncode in (0, 1):
            return out.returncode == 0
        raise GitError(f"git merge-base --is-ancestor {ref} failed in {self.repo} "
                       f"(exit {out.returncode}): {out.stderr.strip()}")


class Report:
    """What one report says about its item."""

    def __init__(self, path, rel):
        self.rel = rel
        try:
            self.lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
        except OSError as exc:
            raise InputError(f"cannot read report {path}: {exc}") from exc

    def partial_line(self):
        """1-based line number of the first not-done marker, or None."""
        for no, line in enumerate(self.lines, start=1):
            if line.startswith("# "):
                if _TITLE_PARTIAL_RE.search(line):
                    return no
                break
        for no, line in enumerate(self.lines[:HEAD_LINES], start=1):
            if _STATUS_BAD_RE.search(line):
                return no
        for no, line in enumerate(self.lines, start=1):
            if _CHECK_NOT_BUILT_RE.search(line):
                return no
        return None

    def _first(self, pattern):
        for line in self.lines:
            match = pattern.search(line)
            if match:
                return match.group(1)
        return None

    def branch(self):
        name = self._first(_BRANCH_RE)
        return name.rstrip(".-/") if name else None

    def head(self):
        return self._first(_HEAD_RE)


def find_report(reports, item_id):
    matches = sorted(reports.glob(f"t*/{item_id}.md"))
    return matches[-1] if matches else None


def classify(item_id, reports, git, target_sha):
    """Return (bucket, reason) for one unchecked item."""
    path = find_report(reports, item_id)
    if path is None:
        return "no report", "no report"
    report = Report(path, os.path.relpath(path, reports))
    line = report.partial_line()
    if line is not None:
        return "partial", f"partial: {report.rel}:{line}"
    refs = [r for r in (report.branch(), report.head()) if r]
    if not refs:
        return "branch unknown", f"branch unknown: no branch or head in {report.rel}"
    known = [(ref, sha) for ref in refs if (sha := git.resolve(ref))]
    if not known:
        return "branch unknown", (f"branch unknown: {', '.join(refs)} not found "
                                  f"in the repository ({report.rel})")
    for ref, sha in known:
        if git.is_ancestor(sha, target_sha, ref):
            return "done", f"done: {report.rel} via {ref}"
    names = ", ".join(ref for ref, _ in known)
    return "unmerged", f"branch not merged: {names} ({report.rel})"


def also_covers(text):
    """Ids in "(also covers a-01, b-02)". The list ends at the first piece that
    is not a bare id, so prose such as "(the guard is ci-30)" ticks nothing."""
    ids = []
    for group in _ALSO_COVERS_RE.findall(text):
        for piece in (p.strip() for p in group.split(",")):
            if not _ID_RE.match(piece):
                break
            ids.append(piece)
    return ids


class Todo:
    """todo.md as lines, keeping every byte of what is not ticked."""

    def __init__(self, path):
        self.path = path
        try:
            data = path.read_bytes().decode("utf-8")
        except FileNotFoundError as exc:
            raise InputError(f"--todo {path} does not exist") from exc
        except (OSError, UnicodeDecodeError) as exc:
            raise InputError(f"cannot read --todo {path}: {exc}") from exc
        self.lines = data.split("\n")

    @staticmethod
    def text(raw):
        return raw[:-1] if raw.endswith("\r") else raw

    def tick(self, index):
        raw = self.lines[index]
        if not raw.startswith("- [ ]"):
            return False
        self.lines[index] = "- [x]" + raw[5:]
        return True

    def save(self):
        data = "\n".join(self.lines).encode("utf-8")
        fd, tmp = tempfile.mkstemp(dir=self.path.parent, prefix=".todo-", suffix=".tmp")
        try:
            with os.fdopen(fd, "wb") as handle:
                handle.write(data)
            os.replace(tmp, self.path)
        except OSError as exc:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise InputError(f"cannot write --todo {self.path}: {exc}") from exc


class Outcome:
    """Per-line decisions for one run."""

    def __init__(self):
        self.items = []  # (index, id, bucket, reason) in todo order
        self.malformed = []  # (line number, text)
        self.ticked_ids = []  # ids already ticked, in todo order
        self.already_ticked = 0

    def counts(self):
        counts = dict.fromkeys(BUCKETS, 0)
        for _, _, bucket, _ in self.items:
            counts[bucket] += 1
        counts["malformed"] = len(self.malformed)
        return counts

    def done_ids(self):
        done = [item_id for _, item_id, bucket, _ in self.items if bucket == "done"]
        return done + self.ticked_ids


def scan_lines(todo, outcome):
    """Sort lines into unchecked items, ticked lines and malformed lines."""
    pending = []
    for index, raw in enumerate(todo.lines):
        text = todo.text(raw)
        if not text.startswith("- ["):
            continue
        ticked = _TICKED_RE.match(text)
        if ticked:
            outcome.already_ticked += 1
            if ticked.group("id"):
                outcome.ticked_ids.append(ticked.group("id"))
            continue
        unchecked = _UNCHECKED_RE.match(text)
        if unchecked:
            pending.append((index, unchecked.group("id"), text))
        else:
            outcome.malformed.append((index + 1, text))
    return pending


def decide(todo, reports, git, target_sha):
    outcome = Outcome()
    pending = scan_lines(todo, outcome)
    decided = [(i, item_id, *classify(item_id, reports, git, target_sha), text)
               for i, item_id, text in pending]
    covered = {}
    for _, item_id, bucket, _, text in decided:
        if bucket == "done":
            for cid in also_covers(text):
                covered.setdefault(cid, item_id)
    for index, item_id, bucket, reason, _ in decided:
        if bucket != "done" and item_id in covered:
            bucket, reason = "done", f"done: covered by {covered[item_id]}"
        outcome.items.append((index, item_id, bucket, reason))
    return outcome


def unreleased_section(path):
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError) as exc:
        raise InputError(f"cannot read --changelog {path}: {exc}") from exc
    start = next((n for n, line in enumerate(lines)
                  if re.match(r"^## \[Unreleased\]", line, re.IGNORECASE)), None)
    if start is None:
        raise InputError(f"--changelog {path} has no '## [Unreleased]' section")
    end = next((n for n in range(start + 1, len(lines))
                if lines[n].startswith("## ")), len(lines))
    return "\n".join(lines[start:end])


def uncited(ids, section):
    def cited(item_id):
        pattern = rf"(?<![A-Za-z0-9-]){re.escape(item_id)}(?![A-Za-z0-9]|-[A-Za-z0-9])"
        return re.search(pattern, section) is not None
    return [item_id for item_id in ids if not cited(item_id)]


def report_outcome(outcome, changes, write):
    for line_no, text in outcome.malformed:
        print(f"  line {line_no}: malformed: {text!r}")
    for _, item_id, _, reason in outcome.items:
        print(f"  {item_id}: {reason}")
    counts = outcome.counts()
    print(", ".join(f"{name} {counts[name]}" for name in BUCKETS))
    print(f"already ticked {outcome.already_ticked}")
    suffix = "" if write else " (dry run: pass --write to apply)"
    print(f"changes {changes}{suffix}")


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--todo", required=True, type=pathlib.Path)
    parser.add_argument("--reports", required=True, type=pathlib.Path)
    parser.add_argument("--repo", required=True, type=pathlib.Path,
                        help="the integration worktree (read-only)")
    parser.add_argument("--target", default="improve/integration",
                        help="branch that done work must be merged into")
    parser.add_argument("--write", action="store_true",
                        help="tick done lines (default: dry run)")
    parser.add_argument("--changelog", type=pathlib.Path,
                        help="list done ids not cited in its [Unreleased] section")
    return parser.parse_args(argv)


def run(args):
    todo = Todo(args.todo)
    if not args.reports.is_dir():
        raise InputError(f"--reports {args.reports} is not a directory")
    git = Git(args.repo)
    target_sha = git.resolve(args.target)
    if target_sha is None:
        raise GitError(f"--target {args.target} does not resolve to a commit "
                       f"in {args.repo}")
    section = unreleased_section(args.changelog) if args.changelog else None
    outcome = decide(todo, args.reports, git, target_sha)
    changes = sum(1 for index, _, bucket, _ in outcome.items
                  if bucket == "done" and todo.tick(index))
    if args.write and changes:
        todo.save()
    report_outcome(outcome, changes, args.write)
    if section is not None:
        missing = uncited(outcome.done_ids(), section)
        print(f"done ids not cited in [Unreleased]: {len(missing)}")
        for item_id in missing:
            print(f"  {item_id}")
    return EXIT_OK


def main(argv=None):
    args = parse_args(argv)
    try:
        return run(args)
    except InputError as exc:
        print(f"tick_todo: {exc}", file=sys.stderr)
        return EXIT_INPUT
    except GitError as exc:
        print(f"tick_todo: {exc}", file=sys.stderr)
        return EXIT_GIT


if __name__ == "__main__":
    sys.exit(main())

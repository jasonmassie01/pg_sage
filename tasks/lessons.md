
## Lesson: Commit with explicit paths when tests are pre-staged
- **Date**: 2026-09-26
- **Mistake**: Phase-1 tests for one change were staged early; a later `git commit -m` for an unrelated fix swept them into the wrong commit.
- **Rule**: When the index holds staged work for another change, commit with explicit paths (`git commit -- <paths>`) or check `git diff --cached --stat` first.
- **Context**: Two-phase testing (stage failing tests first) while interleaving small fixes.

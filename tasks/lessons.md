
## Lesson: Commit with explicit paths when tests are pre-staged
- **Date**: 2026-09-26
- **Mistake**: Phase-1 tests for one change were staged early; a later `git commit -m` for an unrelated fix swept them into the wrong commit.
- **Rule**: When the index holds staged work for another change, commit with explicit paths (`git commit -- <paths>`) or check `git diff --cached --stat` first.
- **Context**: Two-phase testing (stage failing tests first) while interleaving small fixes.

## Lesson: Product principle, not process
- **Date**: 2026-09-27
- **Mistake**: I read "understand the project, gain trust and then become autonomous" as a
  trust ramp for Claude, with memos for jmass to ratify. It was the product's own
  principle: pg_sage gains trust, then becomes autonomous.
- **Rule**: When an instruction echoes the product's vision, apply it to the product's
  decisions and act on them. Do not build approval process around myself unless asked.
- **Context**: Deferred product decisions and roadmap calls on pg_sage.

## Lesson: pg_sage is an AI DBA — LLM features default on
- **Date**: 2026-10-01
- **Mistake**: Made the Sage SRE model turn off by default "until the bench gates pass",
  treating the LLM as an optional add-on.
- **Rule**: LLM-backed features are the product and default to ON whenever an LLM is
  configured. Earn trust through validation, cited evidence, deterministic fallback and
  budgets — not by shipping the AI disabled. Without an LLM, degrade to deterministic with
  one clear log line.
- **Context**: Any pg_sage config default or product decision involving LLM features.

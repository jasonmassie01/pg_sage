# Claude Code Setup for pg_sage (Go Sidecar)

## What's Included

```
CLAUDE.md                                → Project root
.claude/
├── settings.json                        → Hooks + permissions
└── skills/
    ├── tdd-workflow.md                  → Test-first workflow with testify patterns
    ├── go-pgx-patterns.md              → pgx, pgxpool, LISTEN/NOTIFY, goroutine lifecycle
    └── llm-provider.md                 → Pluggable LLM interface design + testing patterns
```

## Installation

### Option A: Starting the go-sidecar branch (recommended)

```bash
cd /path/to/pg_sage
git checkout -b go-sidecar

# Remove C extension files (or move to legacy/)
mkdir -p legacy
git mv src/ include/ sql/ pg_sage.control Makefile legacy/
git mv docker-entrypoint-initdb.d/ legacy/

# Copy in Claude Code config
cp /path/to/this/CLAUDE.md .
cp -r /path/to/this/.claude .

# Initialize Go module
go mod init github.com/jasonmassie01/pg_sage
go mod tidy

# Scaffold the directory structure
mkdir -p cmd/pg_sage
mkdir -p internal/{collector,analyzer/rules,executor/actions,briefing,llm,circuit,db,config,models}
mkdir -p migrations

# Commit the foundation
git add -A
git commit -m "Initialize Go sidecar with Claude Code config"
```

### Option B: If you already have the branch

```bash
cd /path/to/pg_sage
git checkout go-sidecar
cp /path/to/this/CLAUDE.md .
cp -r /path/to/this/.claude .
git add CLAUDE.md .claude/
git commit -m "Add Claude Code config (CLAUDE.md, skills, hooks)"
```

## What Each Piece Does

### CLAUDE.md
Loaded at session start. Tells Claude Code:
- This is a Go sidecar (not a C extension) using pgx
- The full project structure with package responsibilities
- Build, test, and lint commands
- Code style rules (error wrapping, context passing, slog, interfaces)
- What NOT to do (no database/sql, no global state, no panic for recoverable errors)

### Hooks (settings.json)

| Hook | Trigger | What It Does |
|------|---------|--------------|
| PostToolUse (Edit/Write) | Any .go file edit | Runs `go vet` automatically to catch issues early |
| PreToolUse (Bash) | Destructive SQL/shell | Blocks DROP CASCADE, rm -rf /, TRUNCATE sage.* |
| PreToolUse (Edit/Write) | Migration file edits | Warns: never edit existing migrations |
| Stop | End of every turn | Reminds to run tests + lint if Go files changed |

### Skills

**tdd-workflow.md** — Test-first patterns specific to pg_sage:
- testify assert/require/mock patterns
- Unit vs integration test separation (build tags)
- What to test per feature type (rules, actions, LLM providers, SQL queries)
- Mocking database access via interfaces
- Red flags that mean stop and reassess

**go-pgx-patterns.md** — pgx and Go patterns for the sidecar:
- pgxpool setup with sidecar-appropriate settings
- System catalog queries (pg_stat_statements, etc.)
- PG version detection and query branching for 14+ compatibility
- LISTEN/NOTIFY for control commands (emergency_stop, resume)
- errgroup goroutine lifecycle management
- Schema migration pattern
- Error handling with sentinel errors and wrapping
- Interface design principles

**llm-provider.md** — Pluggable LLM architecture:
- Provider interface (Complete, Name, Available)
- OpenAI-compatible, Anthropic, and Ollama implementations
- Circuit breaker wrapping
- Prompt patterns for briefing, diagnose (ReAct), explain narrative
- Testing via httptest mock servers

## Verification

After installation, start Claude Code:

```bash
cd /path/to/pg_sage
claude
```

Test these:

1. "What database driver does this project use?" → Should answer pgx without reading any files
2. "How do I add a new Tier 1 rule for connection pool monitoring?" → Should reference TDD skill, tell you to write the test first
3. "Show me how to query pg_stat_user_tables" → Should use pgx native interface with parameterized queries, not database/sql
4. "I want to add Groq as an LLM provider" → Should reference the OpenAI-compatible provider pattern from the LLM skill

## Adding New Skills Over Time

Create `.md` files in `.claude/skills/` as patterns stabilize:

- `trust-model.md` — trust level transitions, day counting, action gating rules
- `docker-dev.md` — compose setup, container lifecycle, volume mounts, test database seeding
- `rules-catalog.md` — reference of all Tier 1 rules with thresholds, severities, and remediation actions

## Updating CLAUDE.md

When Claude Code makes a mistake, add the lesson to "What NOT To Do" in CLAUDE.md. When you establish a new pattern worth preserving, add it to code style or create a new skill. This is how the project's institutional memory grows.

# OpenAI compatibility for the LLM client, and the first live OpenAI runs (2026-10-02)

Branch `claude/llm-openai-compat`, from master `fce3674` (v1.8.0-to-be). Rules:
`~/.claude/tasks/overnight-agent-rules.md`. Not pushed.

## The bug

Checked by hand against api.openai.com with `gpt-6-luna`:

1. `max_tokens` is rejected with HTTP 400. The error says "Unsupported parameter: 'max_tokens' ...
   Use 'max_completion_tokens' instead". pg_sage sent `max_tokens` from both `Chat` and
   `ChatWithTools`.
2. With `max_completion_tokens`, tool calls are still rejected with 400. The error says
   "Function tools with reasoning_effort are not supported ... set reasoning_effort to 'none'".

So every LLM feature fell back to its deterministic path with current OpenAI models. Each
rejection also counted as a provider failure, so three of them opened the circuit breaker.

## The fix

**`internal/llm/wire_compat.go` (new).** The request shape now adapts to each endpoint and
model:

- In `auto` mode every request starts in the classic shape: `max_tokens`, and no
  `reasoning_effort`.
- If the provider returns a 400 asking for a different shape, the request is re-sent once in
  that shape. Two cases are recognised:
  - The 400 asks for `max_completion_tokens`. The message must mention both
    `max_completion_tokens` and `max_tokens`, or the body must carry
    `param=max_tokens, code=unsupported_parameter`.
  - The 400 is on a request with tools and asks for `reasoning_effort` `none`.
- Matching ignores case. It reads `error.message` from an OpenAI-style JSON body, or the raw
  text when the body is not JSON.
- **The adaptation is remembered process-wide**, per (normalized chat endpoint, model). Fleet
  mode builds one client per database and purpose, and a fresh client for the same model
  starts already adapted. Access is under a mutex and is race-free.
- **Each adaptation is logged once per process.** The log names the model and the setting, never
  the endpoint or key.
- **The re-send loop always ends.** A re-send only happens for an adaptation the request's
  shape does not have yet, and every re-send adds one. So a request makes at most 3 attempts
  for a tool call and at most 2 for chat.
- **What a rejection does not do:**
  - It is not a breaker failure: `statusError` returns an `adaptError` and does not call
    `recordFailure`.
  - It never touches the budgets. The daily, per-database and per-call budgets are reserved
    once before the first attempt. They are reconciled once, to the usage of the reply that
    succeeds. A 400 carries no usage, and OpenAI does not bill it.
  - It does not change the throttle key or the cooldown.
- **What is not re-sent:**
  - a 400 that does not ask for one of the two shapes;
  - a 429 (still `ErrRateLimited` for tools, and the existing retry ladder for chat);
  - a 5xx;
  - a 422;
  - an error body that is empty or malformed.

  Each of these is recorded once, as before.
- `reasoning_effort` is only ever sent with tools. A plain `Chat` call to an adapted model
  uses `max_completion_tokens` and nothing else.

**Config.** New file `internal/config/llm_wire.go`, plus two fields in `LLMConfig`:

| Key | Values | Default |
|---|---|---|
| `llm.token_parameter` | `auto`, `max_tokens`, `max_completion_tokens` | `auto` |
| `llm.tool_reasoning_effort` | `auto`, `none`, `low`, `medium`, `high`, `omit` | `auto` |

- An empty value means `auto`. Any other value is rejected by `Config.validate`, at load and
  at reload, with an error that names the key.
- Both fields have doc tags, and `config_meta.json` and `docs/generated/config-lifecycles.md`
  were regenerated. `config.example.yaml` documents them.
- Lifecycle: `reconfigure`, through the `llm` owner (`lifecycle.go`).
- They are YAML-only: they are in the store's exclusion registry, not API overrides.
- Explicit values are honoured as given and never adapted. `omit` never sends
  `reasoning_effort`.
- The optimizer client inherits both from `llm.*`, as it does `json_mode`: in
  `llm.NewOptimizerClient` and in `optimizerLLMConfig` in `cmd/pg_sage_sidecar`.

**Thinking models (`repair.go`).** `gpt-5` and later count as thinking models:
`^gpt-([5-9]|[1-9][0-9])([.-]|$)` after the provider prefix. The `-chat` aliases do not. The
o-series already did.

**Live tests.** `internal/llm`, `internal/rca` and the e2e LLM tests now read
`SAGE_LLM_ENDPOINT`, `SAGE_LLM_MODEL` and `SAGE_LLM_API_KEY`. Gemini stays the default. The
unit live tests fall back to `GEMINI_API_KEY`. Two live tests were added to `internal/llm`: a
second tool turn that answers from the tool result, and plain chat in JSON mode.

## Product decisions

- **Adapt instead of keeping a model list.** A list of model names goes stale with every
  release. The provider's own 400 is the source of truth, and the default stays the classic
  shape, so Gemini, Groq, Ollama and the existing callers send byte-identical bodies. Tests
  assert the exact key sets.
- **Process-wide memory, keyed by endpoint and model.** One extra 400 per model per process,
  not one per client. The same model behind another endpoint (a proxy, or Azure) learns on its
  own.
- **Tools get `reasoning_effort: none` rather than a move to `/v1/responses`.** This keeps a
  single wire protocol for every provider. The investigator's tool turns are short, structured
  turns that do not need hidden reasoning, and the graph stays the authority.
- **gpt-5 and later are thinking models for budgeting.**
  - They reason in plain chat, and their reasoning counts against `max_completion_tokens`.
    Without the reserve, small caps would truncate.
  - On tool turns with effort `none` they report 0 reasoning tokens.
  - `sre.splitUsage` records such a turn as answer tokens only (tested). Reservations are
    reconciled to actual use, so the reserve costs nothing.
- **The new keys are YAML-only.** `auto` needs no operator action. An explicit pin is a
  provider-compatibility attestation, not a runtime knob.

Autonomy is unaffected: no action path, gate or trust level changed.

## Spec CHECKs

- **CHECK-10** (prompt injection cannot expand tools), measured live for the first time. The
  replay corpus's adversarial and missing-data cases passed `R1-ADVERSARIAL` with
  `gpt-6-luna`:
  - 15 runs in each replay run;
  - three prompt-injection cases: relation name, `application_name`, slot name;
  - three secret-in-data cases;
  - nine missing-data cases.

  Across those runs: 0 forbidden tool calls, 0 mutations, 0 canary leaks and 0 out-of-scope
  references. The model ranked the injected cases' true root first every time.
- **CHECK-11/12 (claims).** `R1-CLAIM-REFS` passed: 100% of narrated claims cite verifying
  evidence. That was 229/229 in the standalone replay and 230/230 in the bench replay, with
  0 unresolved claims across the 105 claims of the scenario runs.
- **CHECK-36-REPLAY and CHECK-42** pass for the causal graph, unchanged.
- **M3-LLM-ROOT** passed: 0 conclusive roots changed by the live model.

## Test Results

**Commands:**
- `go test -count=1 -cover ./...` on PG17 :55471, with the repo root mounted.
- Touched packages with `-race -cover` on PG14 :55414 and PG18 :55418.
- `golangci-lint run ./...`.

**Totals:**
- **Full suite on PG17:** 80 packages, 79 ok and 1 FAIL.
  - The failure was `internal/value` `TestFleetServiceConcurrentReadersAgree`: a dial timeout
    to `host.docker.internal:55471` while creating the fixture database. Another agent was
    loading the Docker host at the time.
  - A rerun of `./internal/value/` passed (95.3%). It does not import `llm`.
- **Touched packages on PG17, `-v`:** 2002 passed, 0 failed, 4 skipped.
- **Race runs:** `-race` on PG14 and on PG18: every touched package ok, no data races.
- **Lint:** 0 issues.

**Coverage of touched packages:**

| Package | Coverage | Floor |
|---|---|---|
| `internal/llm` | 93.1% | 70% |
| `internal/config` | 89.2% | 70% |
| `internal/sre` | 87.3% | 70% |
| `internal/rca` | 95.8% | 70% |
| `internal/store` | 74.5% | 70% |
| `cmd/pg_sage_sidecar` | 72.5% | 70% |
| `sre-bench` | 62.2% | 50% (harness) |

All touched packages meet their thresholds. `internal/api` is at 76.0% and was not touched.

**Skipped tests (all justified):**
- `TestChatWithToolsLive_RealProvider`, `TestChatLive_RealProvider` and
  `TestTier2Live_RealGemini`: live provider, opt-in through `PG_SAGE_LIVE_LLM=1`. They were run
  live separately; see below.
- `TestRCAChildProcessFixture`: a helper that only runs as a child process.

**Failures:**
- `TestChat_TokenParameterErrorVariants/empty_body` failed on the first run. This was a test
  logic error: the fake treats an empty `maxTokensErr` as "accept", so that case never sent a
  400. It was fixed in its own commit (`020c417`), which explains the change. The assertions
  were not weakened.

**New tests:** 32, plus 2 live tests.
- `openai_compat_{chat,tools,state,optimizer}_test.go`, `thinking_openai_test.go`,
  `config/llm_wire_test.go`, `sre/openai_usage_test.go` and `cmd/.../llm_wire_inherit_test.go`.
- They run against an OpenAI-compatible httptest fake that returns the live 400 bodies.
- **Happy path:** both adaptations, memory across calls and across clients.
- **Invalid input:** 10 error-body variants, and bad config values (case, spacing, unknown
  efforts).
- **Nil/empty:** empty and malformed bodies, `""` treated as `auto`, tools without tools.
- **Error propagation:**
  - a second, different 400;
  - the same adaptation error repeated (no loop);
  - a 429 during adaptation is `ErrRateLimited`;
  - explicit settings surface the provider's message.
- **Boundaries:** the reasoning allowance after a rename, the thinking cap, the 500 and 422
  statuses.
- **Concurrency:** two clients adapt the same model at the same moment, held by a barrier in
  the fake, under `-race`.
- **State:**
  - adaptation scoped to endpoint and model;
  - a breaker one failure from opening stays closed;
  - the per-database budget is charged once;
  - the cooldown is kept.
- **Integration:** the existing DB suites, plus the live runs.

**Mutation testing:** 15 mutants of the key logic, all killed:

- the status check;
- a breaker failure recorded on adaptation;
- no memory;
- explicit settings ignored;
- the loop guard removed (killed by a timeout);
- effort adapted without tools;
- `omit` ignored;
- logging every time;
- the param-only branch;
- keying by model only;
- `max_tokens` and `max_completion_tokens` both sent, in chat and in tools;
- gpt-5+ not thinking;
- `-chat` aliases treated as thinking;
- no optimizer inheritance.

The first status mutant broke the build (an unused import). It was redone so that it kept 500
and 422, and that version was killed by the variants test.

**Post-test audit:**

- **Untested inputs:** a provider that wants `max_completion_tokens` and later goes back to
  `max_tokens`. The adaptation is one-way until a restart. This is acceptable: no known
  provider does it.
- **Assertions that pass when broken:** none found. Every test asserts request bodies, counts,
  usage, budgets or breaker state, and the mutation run confirms it.
- **Fakes that hide real failures:** the fake reproduces OpenAI's exact bodies. The real API
  was exercised by every live run below, and the live log lines show both adaptations firing.

## Live results (`gpt-6-luna`, api.openai.com, PG17)

Prices: $0.10/M input, $0.50/M output. OpenAI's `completion_tokens` includes reasoning tokens,
so cost is prompt × input price + completion × output price. Reasoning tokens were 0 on every
tool turn, because effort was `none`.

| Test | Result | Calls | Prompt / completion tokens | Cost |
|---|---|---|---|---|
| `internal/llm` `TestChatWithToolsLive_RealProvider` (2 tool turns) | PASS. Both adaptations logged, and the answer uses the tool result | 2 ok + 2 adapting 400s | 342 / 39, reasoning 0 | $0.00005 |
| `internal/llm` `TestChatLive_RealProvider` (JSON chat) | PASS | 1 | 47 total | <$0.0001 |
| `internal/rca` `TestTier2Live_RealGemini` (Tier 2, run on OpenAI) | PASS. Valid incident, 3-link chain, read-only SQL | 1 + 1 adapting 400 | not instrumented (about 1.5k) | about $0.0003 |
| e2e `TestLLM*` (7 tests, including the circuit-breaker test) | 7/7 PASS | 6 live | 2,697 total | ≤ $0.0013 |
| e2e `TestTunerLLM_*` (6) | 6/6 PASS | 6 | 10,116 total | ≤ $0.0051 |
| e2e `TestOptimizerMultiQueryConsolidation` | PASS | 1 | 3,180 total | ≤ $0.0016 |
| `TestReplayCorpus` (60 cases × 2 arms) | PASS, all gates | 203 + 120 adapting 400s | 78,753 / 11,937 | $0.0138 |
| `TestPGIncidentBench` core shard (lock, connection, wal, plan) | PASS | 100 + 60 adapting 400s | 36,283 / 5,209 | $0.0062 |
| …its attached replay (second live replay) | PASS, all gates | 206 + 120 adapting 400s | 81,187 / 12,268 | $0.0143 |
| **Total** | | | | **about $0.04** (budget $3) |

The e2e tests report only total tokens, so their cost is shown at the output price as an
upper bound.

**Per family, LLM-on arm (`causal-graph+llm`).** Every run of the causal-graph arm is identical
in Safe Pass and top-1.

| Source | Family | Runs | Safe pass | Top-1 | Turns | Reviewed | model_rejected | model_disagreed | Claims cited | Prompt / completion | Cost |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Bench | connection_pressure | 8 | 8/8 | 5/5 | 11 | 8 | 0 | 0 | 27/27 | 9,895 / 1,512 | $0.0017 |
| Bench | lock_blocking | 10 | 10/10 | 7/7 | 13 | 10 | 0 | 0 | 39/39 | 12,049 / 1,712 | $0.0021 |
| Bench | plan_regression | 5 | 5/5 | 3/3 | 7 | 5 | 0 | 0 | 8/8 | 5,665 / 587 | $0.0009 |
| Bench | wal_retention | 7 | 7/7 | 5/5 | 9 | 7 | 0 | 0 | 31/31 | 8,674 / 1,398 | $0.0016 |
| Replay #1 | connection_pressure | 20 | 20/20 | 13/13 | 27 | 20 | 0 | 0 | 70/70 | 24,591 / 3,644 | $0.0043 |
| Replay #1 | lock_blocking | 20 | 20/20 | 12/12 | 28 | 20 | 0 | 0 | 78/78 | 26,424 / 3,987 | $0.0046 |
| Replay #1 | wal_retention | 20 | 20/20 | 12/12 | 28 | 19 | 0 | 1 | 81/81 | 27,738 / 4,306 | $0.0049 |
| Replay #2 | connection_pressure | 20 | 20/20 | 13/13 | 30 | 19 | 1 | 0 | 64/64 | 27,312 / 4,021 | $0.0047 |
| Replay #2 | lock_blocking | 20 | 20/20 | 12/12 | 28 | 20 | 0 | 0 | 79/79 | 26,424 / 4,048 | $0.0047 |
| Replay #2 | wal_retention | 20 | 20/20 | 12/12 | 28 | 20 | 0 | 0 | 87/87 | 27,451 / 4,199 | $0.0048 |

Gates:
- **Replay:** `R1-TOP1`, `R1-ABSTAIN`, `R1-FORBIDDEN`, `R1-ADVERSARIAL`, `R1-PACKET-P95`
  (LLM arm p95 10.0–10.5 s, against a 2-minute limit), `R1-CLAIM-REFS` and `M3-LLM-ROOT` all
  pass on both live replays.
- **Not evaluated, by design:**
  - `M3-LLM-PARITY` is pre-registered for the fake model only.
  - `R1-FACTUAL-PRECISION` needs human reviewers.
- **Baselines:** `always-escalate` and `rules-only` fail their gates as expected. They are not
  gated.

## Findings from the live runs

1. **No product correctness bugs surfaced.** Each investigation cost about $0.0002. Both live
   runs of the replay corpus gave the same Safe Pass and top-1.
2. **The bench's model tap gets a new URL for every run, so each run learns the adaptation
   again.** That adds 2 free 400s per run, which show up as `http_errors`: 300 across the three
   bench runs, and every one of them is an adaptation. They also consume `PG_SAGE_BENCH_LLM_RPM`
   pacing. Production endpoints are stable, so this is a bench artefact. **Coordinator
   decision:**
   - (a) have the tap's client inherit the learned shape (for example, key the bench by the
     upstream URL);
   - (b) report adaptation 400s separately from `http_errors`; or
   - (c) leave it, as documented here.
3. **The bench cannot diagnose a model rejection.** One `model_rejected` (replay #2,
   `replay/conn-pool-warmup`, a decoy) and one `model_disagreed` (replay #1,
   `replay/wal-slow-consumer-with-surge`) were handled safely: the decoy stayed inconclusive
   and the root stayed `slow_consumer`. But the report does not record why the reply was
   rejected or what was disputed, so the cause cannot be found without rerunning. Recording the
   rejection reason in `ModelStats` is a suggested follow-up.
4. **The live tests were Gemini-only.** The `internal/llm` and `internal/rca` live tests and
   the e2e LLM tests hard-coded the Gemini endpoint and model. They are now configurable; see
   above.
5. **Pre-existing, not caused by this branch:** `internal/rca` fails rather than skips when
   `SAGE_TEST_DATABASE_URL` is unset. `TestPreflightRCA*` dials `127.0.0.1:1`. With a database
   it passes.
6. **`TestTier2Live_RealGemini` keeps its name** so that existing references stay valid. It is
   now provider-neutral.

## Commits

- `7d1cc4d` test(llm): cover OpenAI max_completion_tokens and tool reasoning_effort adaptation
- `020c417` test(llm): make non-adapting error variants reject every request
- `8c0688b` fix(llm): adapt request shape for current OpenAI models
- `a4858c9` refactor(llm): keep exchangeTools within the 50-line limit
- this report and the CHANGELOG bullet

## What is left

- The coordinator should decide on finding 2 (bench re-learning) and finding 3 (rejection
  reasons in the bench report).
- `/v1/responses` support for reasoning with tools, if reasoning on tool turns is ever wanted.
- The reactive and runway bench shards were not run live (out of scope; core only).

# CLAUDE.md — pg_sage C Extension (v0.6.0-rc2 → v0.6.0-rc3)

## Mission

Fix the 3 remaining bugs (1 CRITICAL segfault, 2 medium), resolve API key persistence, verify the "queries" collector fix, and run Phase 15/16 test data with MCP prompt testing. This is the iteration that makes pg_sage stable enough to leave running.

---

## Current State (v0.6.0-rc2)

### Fixed and Verified (DO NOT TOUCH)
- BUG-1: Advisory lock key = 710190109
- BUG-2: API key hidden (PGC_SUSET + GUC_NO_SHOW_ALL)
- BUG-4: Empty trust_level rejected, case-insensitive
- BUG-5: trust_ramp_override_days GUC works
- BUG-6: PG_TRY/PG_CATCH in executor
- BUG-7: PK/unique indexes excluded
- BUG-8: Failed actions cooldown
- BUG-9: sage schema excluded from vacuum_bloat
- BUG-10/11: Zero adversarial crashes
- BUG-12: ReAct function blocklist (22 functions)
- DDL worker: 4th background worker with libpq
- Compiler warnings: 0
- Security: quote_identifier() on dynamic SQL
- 48 GUCs total

### Remaining Bugs
| # | Severity | Bug |
|---|----------|-----|
| BUG-13 | CRITICAL | Analyzer segfault (signal 11) every ~60s after first cycle |
| BUG-14 | MEDIUM | analyzer_interval GUC ignored — always 60s |
| BUG-15 | LOW | diagnose() response truncation ~150 chars |
| NEW | MEDIUM | API key ALTER SYSTEM doesn't persist after reload |
| BUG-3 | MEDIUM | "queries" snapshot fix applied but not verified |

---

## Fix 1: Analyzer Segfault (BUG-13 — CRITICAL)

Fix this BEFORE anything else. The analyzer crashes after cycle 1 — nothing downstream works.

### Get a stack trace:
```bash
docker exec pg_sage_test bash -c "echo '/tmp/core.%p' > /proc/sys/kernel/core_pattern"
docker exec pg_sage_test bash -c "ulimit -c unlimited"
docker exec pg_sage_test pg_ctl -D /var/lib/postgresql/data restart
sleep 120
docker exec pg_sage_test gdb /usr/lib/postgresql/17/bin/postgres /tmp/core.* -batch -ex "bt full"
```

### Most likely cause: use-after-free in SPI context

The analyzer reads snapshots via SPI, runs rules, then on cycle 2 dereferences pointers from cycle 1 that were freed when SPI_finish() was called.

```bash
grep -rn "pfree\|SPI_freetuptable\|MemoryContextReset\|MemoryContextDelete" src/analyzer*.c
grep -rn "static.*snapshot\|previous.*snapshot\|last_snapshot\|cached" src/analyzer*.c
```

Fix pattern — copy data to TopMemoryContext BEFORE SPI_finish:
```c
MemoryContext old_ctx = MemoryContextSwitchTo(TopMemoryContext);
for (int i = 0; i < finding_count; i++) {
    if (findings[i].object_identifier)
        findings[i].object_identifier = pstrdup(findings[i].object_identifier);
    if (findings[i].detail)
        findings[i].detail = pstrdup(findings[i].detail);
}
MemoryContextSwitchTo(old_ctx);
SPI_finish();  /* now safe */
```

### Other possible causes:
- NULL from SPI_getvalue not checked (check every call site)
- Shared memory concurrent access between workers
- Stack overflow from recursion in rules

### Verify: 5+ consecutive cycles without crash
```bash
docker exec pg_sage_test pg_ctl -D /var/lib/postgresql/data restart
sleep 360  # 6 minutes
docker logs pg_sage_test 2>&1 | grep -c "analyzer.*cycle\|analyzer.*completed"
# Must be >= 5
docker logs pg_sage_test 2>&1 | grep "signal 11\|SIGSEGV\|terminated"
# Must be 0
```

Commit: `fix: analyzer segfault (BUG-13)`

---

## Fix 2: analyzer_interval GUC Ignored (BUG-14)

```bash
grep -rn "analyzer_interval\|WaitLatch\|sleep" src/analyzer*.c
```

Worker must re-read GUC each cycle, not cache at startup:
```c
while (!shutdown) {
    do_analysis();
    int current_interval = sage_analyzer_interval;  /* re-read each cycle */
    WaitLatch(MyLatch, WL_LATCH_SET | WL_TIMEOUT,
              current_interval * 1000L, PG_WAIT_EXTENSION);
    ResetLatch(MyLatch);
    CHECK_FOR_INTERRUPTS();
}
```

Verify: set to 120s via ALTER SYSTEM, check cycle timing in logs.

Commit: `fix: analyzer_interval GUC respected (BUG-14)`

---

## Fix 3: API Key File-Based Loading

```c
/* New GUC: sage.llm_api_key_file (PGC_SIGHUP — filepath is safe to expose) */
/* Load key from file at _PG_init() and on SIGHUP */
/* File: one line, stripped of trailing whitespace */
/* Key stored in TopMemoryContext — survives transactions */
/* Filepath visible to all users, key value only to superusers */
```

Usage:
```bash
echo "AIzaSy..." > /var/lib/postgresql/gemini_key
chmod 600 /var/lib/postgresql/gemini_key
```
```sql
ALTER SYSTEM SET sage.llm_api_key_file = '/var/lib/postgresql/gemini_key';
SELECT pg_reload_conf();  -- key loads from file, persists
```

Commit: `feat: sage.llm_api_key_file for persistent key loading`

---

## Fix 4: diagnose() Truncation (BUG-15)

```bash
grep -rn "diagnose\|diag.*buf\|maxlen\|max_tokens\|result.*256\|result.*512" src/briefing.c src/llm.c
```

Likely a fixed-size buffer or low max_tokens. Replace with StringInfo (growable) and set max_tokens to 2000+.

Verify: `SELECT length(sage.diagnose('health check'));` > 500 chars.

Commit: `fix: diagnose() truncation (BUG-15)`

---

## Fix 5: Verify BUG-3 with Phase 15 Data

After loading Phase 15 test data (see below), verify:
```sql
SELECT category, count(*) FROM sage.snapshots GROUP BY category ORDER BY category;
-- "queries" MUST be present with count > 0
```

If still missing: debug with `docker logs | grep "collector.*queries"` and fix.

---

## Phase 15: Test Data

Load the full schema from the extension CLAUDE.md / Cloud SQL test plan:
- 50K customers, 5K products, 500K orders, 1M line_items, 500K order_events, 100K audit_log, 200K events
- Duplicate indexes, unused index, exhausted sequence, dead tuples
- 20 iterations of 8 slow query patterns

Expected: 15+ findings after analyzer runs.

---

## Phase 16: MCP Good-Path Prompts

After findings are generated, test these via Claude Desktop → MCP:

1. "What tables are in my database?" → sizes and row counts
2. "What are my slowest queries?" → real execution times
3. "Show duplicate indexes" → idx_li_order, idx_li_product_dup
4. "What indexes should I add?" → CREATE INDEX CONCURRENTLY on orders(customer_id)
5. "Health check" → dead tuples, sequence, missing FKs
6. "Why is my app slow?" → structured investigation with specific tables

Each response must contain specific data, not generic advice.

---

## Definition of Done

### Critical
- [ ] BUG-13: analyzer runs 5+ cycles without segfault
- [ ] BUG-13: stack trace obtained and root cause documented

### Medium
- [ ] BUG-14: analyzer_interval GUC respected
- [ ] API key file: sage.llm_api_key_file works, persists across reloads
- [ ] BUG-3: "queries" category verified present with Phase 15 data

### Low
- [ ] BUG-15: diagnose() returns > 500 chars

### Testing
- [ ] Phase 15 data loaded, 15+ findings generated
- [ ] Phase 16 prompts answered with specific data
- [ ] Tier 3 actions execute against Phase 15 data (duplicates dropped, VACUUM run)

### Build
- [ ] Zero compiler warnings
- [ ] 4 workers running
- [ ] Tagged v0.6.0-rc3

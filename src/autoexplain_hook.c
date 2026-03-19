/*
 * autoexplain_hook.c — Passive EXPLAIN plan capture via ExecutorEnd hook
 *
 * Installs an ExecutorEnd_hook that fires in every backend after every
 * query.  When sage.autoexplain_enabled is true, it checks whether the
 * query exceeded sage.autoexplain_min_duration_ms and, if so, applies
 * the sage.autoexplain_sample_rate sampling rate.  Qualifying queries
 * have their (queryid, duration_ms) deposited into a lock-free ring
 * buffer in shared memory.  The collector background worker drains
 * this buffer and performs the actual EXPLAIN capture asynchronously
 * via SPI, keeping the hot path in user backends extremely lightweight.
 *
 * Design constraints:
 *   - The hook runs in EVERY backend for EVERY query.  It must do
 *     almost no work on the fast path (feature disabled or query too
 *     fast).
 *   - No SPI, no palloc, no lwlock on the fast path.
 *   - The ring buffer uses pg_atomic_uint32 for head/tail so
 *     concurrent producers never corrupt the buffer.
 *   - If the buffer is full, the entry is silently dropped — this
 *     is acceptable because the collector will catch up on the next
 *     cycle and the lost entries are low-value duplicates.
 *   - The previous ExecutorEnd_hook is always saved and restored.
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include "executor/executor.h"
#include "executor/instrument.h"
#include "utils/timestamp.h"

#if PG_VERSION_NUM >= 150000
#include "common/pg_prng.h"
#else
#include <stdlib.h>
#endif

#include <math.h>

/* Previous hook in the chain */
static ExecutorEnd_hook_type prev_ExecutorEnd_hook = NULL;

/* ----------------------------------------------------------------
 * sage_autoexplain_enqueue
 *
 * Lock-free enqueue into the shared-memory ring buffer.
 * Uses compare-and-swap on the head index.  If the buffer is full,
 * the entry is silently dropped.
 * ---------------------------------------------------------------- */
static void
sage_autoexplain_enqueue(int64 queryid, double duration_ms)
{
    uint32 head;
    uint32 next;
    uint32 tail;

    if (sage_state == NULL)
        return;

    for (;;)
    {
        head = pg_atomic_read_u32(&sage_state->explain_queue_head);
        next = (head + 1) & (SAGE_EXPLAIN_QUEUE_SIZE - 1);
        tail = pg_atomic_read_u32(&sage_state->explain_queue_tail);

        /* Buffer full — drop silently */
        if (next == tail)
            return;

        /* Try to claim this slot */
        if (pg_atomic_compare_exchange_u32(&sage_state->explain_queue_head,
                                           &head, next))
        {
            /* We own slot 'head' — fill it in */
            sage_state->explain_queue[head].queryid     = queryid;
            sage_state->explain_queue[head].duration_ms = duration_ms;
            sage_state->explain_queue[head].enqueued_at = GetCurrentTimestamp();
            return;
        }

        /* CAS failed — another backend won the race; retry */
    }
}

/* ----------------------------------------------------------------
 * sage_ExecutorEnd_hook
 *
 * The actual hook function installed in ExecutorEnd_hook.
 * This runs in every backend after every query execution.
 *
 * Fast path (most queries): ~3 boolean/pointer checks, then
 * falls through to the previous hook.
 * ---------------------------------------------------------------- */
static void
sage_ExecutorEnd_hook(QueryDesc *queryDesc)
{
    /*
     * We must always call the previous hook (or standard_ExecutorEnd)
     * to avoid breaking the executor chain.  We do our work first so
     * the instrumentation data is still available.
     */
    if (sage_autoexplain_enabled &&
        sage_state != NULL &&
        !sage_state->emergency_stopped &&
        queryDesc->totaltime != NULL)
    {
        double      duration_ms;
        uint64      queryid;

        /* Read the total execution time from the instrumentation */
        InstrEndLoop(queryDesc->totaltime);
        duration_ms = queryDesc->totaltime->total * 1000.0;

        /* Check minimum duration threshold */
        if (duration_ms >= (double) sage_autoexplain_min_duration_ms)
        {
            /* Extract queryid from the plan's query identifier.
             * In PG14+ the queryId is available on the PlannedStmt. */
            queryid = queryDesc->plannedstmt->queryId;

            if (queryid != 0)
            {
                /* Apply sampling: use a cheap random check.
                 * We use pg_prng_double (PG15+) or random() for older. */
                double sample_roll;

                if (sage_autoexplain_sample_rate >= 1.0)
                {
                    /* Always capture */
                    sage_autoexplain_enqueue((int64) queryid, duration_ms);
                }
                else if (sage_autoexplain_sample_rate > 0.0)
                {
#if PG_VERSION_NUM >= 150000
                    sample_roll = pg_prng_double(&pg_global_prng_state);
#else
                    sample_roll = (double) random() / (double) MAX_RANDOM_VALUE;
#endif
                    if (sample_roll < sage_autoexplain_sample_rate)
                        sage_autoexplain_enqueue((int64) queryid, duration_ms);
                }
                /* else sample_rate == 0.0: capture nothing */
            }
        }
    }

    /* Chain to the previous hook, or call standard_ExecutorEnd */
    if (prev_ExecutorEnd_hook)
        prev_ExecutorEnd_hook(queryDesc);
    else
        standard_ExecutorEnd(queryDesc);
}

/* ----------------------------------------------------------------
 * sage_autoexplain_hook_init
 *
 * Called from _PG_init() to install the ExecutorEnd hook.
 * Saves and chains any previously installed hook.
 * ---------------------------------------------------------------- */
void
sage_autoexplain_hook_init(void)
{
    prev_ExecutorEnd_hook = ExecutorEnd_hook;
    ExecutorEnd_hook = sage_ExecutorEnd_hook;
}

/* ----------------------------------------------------------------
 * sage_autoexplain_drain_queue
 *
 * Called from the collector background worker to drain the
 * ring buffer and run EXPLAIN capture for each queued entry.
 *
 * This function runs inside the collector's transaction context
 * and may open SPI connections.  It processes up to
 * SAGE_EXPLAIN_QUEUE_SIZE entries per call to bound latency.
 * ---------------------------------------------------------------- */
void
sage_autoexplain_drain_queue(void)
{
    uint32  tail;
    uint32  head;
    int     processed = 0;
    int     max_per_cycle = 16;  /* Limit per drain cycle to avoid hogging */

    if (sage_state == NULL || !sage_autoexplain_enabled)
        return;

    for (;;)
    {
        SageExplainQueueEntry entry;

        if (processed >= max_per_cycle)
            break;

        tail = pg_atomic_read_u32(&sage_state->explain_queue_tail);
        head = pg_atomic_read_u32(&sage_state->explain_queue_head);

        /* Queue empty */
        if (tail == head)
            break;

        /* Read the entry at tail */
        entry = sage_state->explain_queue[tail];

        /* Advance tail (only the collector calls this, so no CAS needed) */
        pg_atomic_write_u32(&sage_state->explain_queue_tail,
                            (tail + 1) & (SAGE_EXPLAIN_QUEUE_SIZE - 1));

        /* Skip entries with zero queryid (should not happen, but guard) */
        if (entry.queryid == 0)
            continue;

        /*
         * Run EXPLAIN capture for this queryid.  Each capture runs in
         * its own sub-transaction to isolate failures.
         */
        {
            volatile bool err = false;

            SetCurrentStatementStartTimestamp();
            StartTransactionCommand();
            PushActiveSnapshot(GetTransactionSnapshot());
            SPI_connect();

            PG_TRY();
            {
                sage_spi_exec("SET LOCAL statement_timeout = '2s'", 0);
                sage_explain_capture_auto(entry.queryid);
            }
            PG_CATCH();
            {
                EmitErrorReport();
                FlushErrorState();
                err = true;
                elog(LOG, "pg_sage: autoexplain drain — failed to capture "
                     "plan for queryid " INT64_FORMAT " (duration %.2f ms)",
                     entry.queryid, entry.duration_ms);
            }
            PG_END_TRY();

            SPI_finish();
            if (err)
                AbortCurrentTransaction();
            else
            {
                PopActiveSnapshot();
                CommitTransactionCommand();
            }
        }

        processed++;
    }

    if (processed > 0)
        elog(DEBUG1, "pg_sage: autoexplain drain — captured %d plans", processed);
}

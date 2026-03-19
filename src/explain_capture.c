/*
 * explain_capture.c — EXPLAIN plan capture and narration for pg_sage
 *
 * Captures EXPLAIN plans for queries tracked by pg_stat_statements,
 * stores them in sage.explain_cache, and produces human-readable
 * (or LLM-enhanced) narrations of what the plan does.
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include "executor/spi.h"
#include "lib/stringinfo.h"
#include "utils/builtins.h"

#include <math.h>
#include <string.h>

/* SQL-callable function declaration */
PG_FUNCTION_INFO_V1(sage_explain);

/* ----------------------------------------------------------------
 * Forward declarations for static helpers
 * ---------------------------------------------------------------- */
static double extract_plan_cost(const char *plan_json);
static char  *extract_node_type(const char *plan_json);
static char  *extract_json_string(const char *json, const char *key);
static double extract_json_number(const char *json, const char *key);
static void   build_plan_issues(StringInfo buf, const char *plan_json, int depth);

/* ----------------------------------------------------------------
 * extract_plan_cost
 *
 * Parse the plan JSON string to find "Total Cost": <number>
 * at the top level.  Simple string-search approach.
 * ---------------------------------------------------------------- */
static double
extract_plan_cost(const char *plan_json)
{
	const char *p;

	if (plan_json == NULL)
		return 0.0;

	p = strstr(plan_json, "\"Total Cost\":");
	if (!p)
		return 0.0;

	p += strlen("\"Total Cost\":");
	while (*p == ' ')
		p++;

	return atof(p);
}

/* ----------------------------------------------------------------
 * extract_node_type
 *
 * Extract "Node Type": "..." from plan JSON.
 * Returns a palloc'd string, or NULL if not found.
 * ---------------------------------------------------------------- */
static char *
extract_node_type(const char *plan_json)
{
	return extract_json_string(plan_json, "Node Type");
}

/* ----------------------------------------------------------------
 * extract_json_string
 *
 * Generic helper: find "key": "value" in a JSON string and return
 * a palloc'd copy of value.  Returns NULL if not found.
 * ---------------------------------------------------------------- */
static char *
extract_json_string(const char *json, const char *key)
{
	StringInfoData search_key;
	const char *p;
	const char *start;
	const char *end;

	if (json == NULL || key == NULL)
		return NULL;

	initStringInfo(&search_key);
	appendStringInfo(&search_key, "\"%s\":", key);

	p = strstr(json, search_key.data);
	pfree(search_key.data);

	if (!p)
		return NULL;

	/* Advance past the key and colon */
	p += strlen(key) + 3;  /* skip "key": */

	/* Skip whitespace */
	while (*p == ' ' || *p == '\t')
		p++;

	/* Expect opening quote */
	if (*p != '"')
		return NULL;

	start = p + 1;
	end = start;
	while (*end != '\0' && *end != '"')
	{
		if (*end == '\\' && *(end + 1) != '\0')
			end++;  /* skip escaped character */
		end++;
	}

	if (*end != '"')
		return NULL;

	return pnstrdup(start, end - start);
}

/* ----------------------------------------------------------------
 * extract_json_number
 *
 * Generic helper: find "key": <number> in a JSON string.
 * Returns 0.0 if not found.
 * ---------------------------------------------------------------- */
static double
extract_json_number(const char *json, const char *key)
{
	StringInfoData search_key;
	const char *p;

	if (json == NULL || key == NULL)
		return 0.0;

	initStringInfo(&search_key);
	appendStringInfo(&search_key, "\"%s\":", key);

	p = strstr(json, search_key.data);
	pfree(search_key.data);

	if (!p)
		return 0.0;

	p += strlen(key) + 3;  /* skip "key": */

	while (*p == ' ' || *p == '\t')
		p++;

	return atof(p);
}

/* ----------------------------------------------------------------
 * build_plan_issues
 *
 * Walk through the plan JSON (simple string scanning) to find
 * nodes and flag potential performance issues.  Appends findings
 * to the given StringInfo buffer.
 *
 * depth limits recursion to prevent stack overflow on deeply
 * nested plans.
 * ---------------------------------------------------------------- */
static void
build_plan_issues(StringInfo buf, const char *plan_json, int depth)
{
	const char *search_pos;
	const char *node_start;

	if (plan_json == NULL || depth > 50)
		return;

	/*
	 * Scan for each "Node Type" occurrence in the JSON to find plan
	 * nodes.  For each node we extract the type, cost, and row estimate
	 * and flag issues.
	 */
	search_pos = plan_json;
	while ((node_start = strstr(search_pos, "\"Node Type\"")) != NULL)
	{
		char   *node_type;
		char   *relation_name;
		double  total_cost;
		double  plan_rows;

		/* Extract node attributes starting from this position */
		node_type = extract_json_string(node_start, "Node Type");
		if (node_type == NULL)
		{
			search_pos = node_start + 11; /* skip past "Node Type" */
			continue;
		}

		total_cost = extract_json_number(node_start, "Total Cost");
		plan_rows  = extract_json_number(node_start, "Plan Rows");
		relation_name = extract_json_string(node_start, "Relation Name");

		/* Flag sequential scans on large tables */
		if (strcmp(node_type, "Seq Scan") == 0 && plan_rows > 10000)
		{
			appendStringInfo(buf,
				"- Sequential scan on %s (~%.0f rows) — consider adding an index\n",
				relation_name ? relation_name : "(unknown table)",
				plan_rows);
		}

		/* Flag nested loops with high row estimates */
		if (strcmp(node_type, "Nested Loop") == 0 && plan_rows > 10000)
		{
			appendStringInfo(buf,
				"- Nested Loop producing ~%.0f rows — may indicate a missing join index\n",
				plan_rows);
		}

		/* Flag sort operations with high cost */
		if (strcmp(node_type, "Sort") == 0 && total_cost > 1000.0)
		{
			appendStringInfo(buf,
				"- Sort operation costing %.2f — check if an index could provide ordering\n",
				total_cost);
		}

		/* Flag hash/merge joins with large inputs */
		if ((strcmp(node_type, "Hash Join") == 0 ||
			 strcmp(node_type, "Merge Join") == 0) &&
			plan_rows > 100000)
		{
			appendStringInfo(buf,
				"- %s with ~%.0f rows — verify join keys are indexed\n",
				node_type, plan_rows);
		}

		/* Flag materialize nodes with high cost */
		if (strcmp(node_type, "Materialize") == 0 && total_cost > 5000.0)
		{
			appendStringInfo(buf,
				"- Materialize node costing %.2f — subquery may benefit from optimization\n",
				total_cost);
		}

		if (relation_name)
			pfree(relation_name);
		pfree(node_type);

		/* Advance past this "Node Type" to find the next one */
		search_pos = node_start + 11;
	}
}

/* ----------------------------------------------------------------
 * sage_explain_capture_with_source
 *
 * Capture an EXPLAIN plan for a given queryid with a specified
 * source label (e.g. 'manual', 'auto').
 *
 * 1. Look up the query text from pg_stat_statements.
 * 2. Run EXPLAIN (FORMAT JSON, COSTS, VERBOSE) on it.
 * 3. Store the result in sage.explain_cache.
 *
 * Caller must have an active SPI connection.
 * ---------------------------------------------------------------- */
static void
sage_explain_capture_with_source(int64 queryid, const char *source)
{
	StringInfoData sql;
	char   *query_text = NULL;
	char   *plan_json  = NULL;
	double  total_cost;
	int     ret;
	volatile bool step1_ok = false;
	volatile bool step2_ok = false;

	/* -------- Step 1: Look up query text from pg_stat_statements -------- */
	initStringInfo(&sql);
	appendStringInfo(&sql,
		"SELECT query FROM pg_stat_statements s "
		"JOIN pg_database d ON d.oid = s.dbid "
		"WHERE d.datname = current_database() AND s.queryid = " INT64_FORMAT " "
		"LIMIT 1",
		queryid);

	PG_TRY();
	{
		ret = SPI_execute(sql.data, true, 1);
		if (ret == SPI_OK_SELECT && SPI_processed > 0)
		{
			char *tmp = sage_spi_getval_str(0, 0);
			if (tmp != NULL && tmp[0] != '\0')
			{
				query_text = pstrdup(tmp);
				step1_ok = true;
			}
		}

		if (!step1_ok)
		{
			elog(WARNING,
				 "pg_sage: explain_capture — query text not found for "
				 "queryid " INT64_FORMAT " in pg_stat_statements",
				 queryid);
		}
	}
	PG_CATCH();
	{
		FlushErrorState();
		elog(WARNING,
			 "pg_sage: explain_capture — failed to look up queryid " INT64_FORMAT " "
			 "from pg_stat_statements",
			 queryid);
	}
	PG_END_TRY();

	if (!step1_ok)
	{
		pfree(sql.data);
		return;
	}

	/* -------- Step 2: Run EXPLAIN on the query -------- */
	resetStringInfo(&sql);
	appendStringInfo(&sql,
		"EXPLAIN (FORMAT JSON, COSTS, VERBOSE) %s",
		query_text);

	PG_TRY();
	{
		/*
		 * EXPLAIN returns SPI_OK_SELECT (not SPI_OK_UTILITY) because
		 * it produces a result set.
		 */
		ret = SPI_execute(sql.data, true, 0);
		if ((ret == SPI_OK_SELECT || ret == SPI_OK_UTILITY) && SPI_processed > 0)
		{
			char *tmp = sage_spi_getval_str(0, 0);
			if (tmp != NULL)
			{
				plan_json = pstrdup(tmp);
				step2_ok = true;
			}
		}

		if (!step2_ok)
		{
			elog(WARNING,
				 "pg_sage: explain_capture — EXPLAIN failed for queryid " INT64_FORMAT " "
				 "(SPI returned %d)",
				 queryid, ret);
		}
	}
	PG_CATCH();
	{
		FlushErrorState();
		elog(LOG,
			 "pg_sage: explain_capture — EXPLAIN could not plan queryid " INT64_FORMAT " "
			 "(may require parameter values)",
			 queryid);
	}
	PG_END_TRY();

	if (!step2_ok)
	{
		pfree(query_text);
		pfree(sql.data);
		return;
	}

	/* -------- Step 3: Extract cost and store in sage.explain_cache -------- */
	total_cost = extract_plan_cost(plan_json);

	PG_TRY();
	{
		Oid     argtypes[5] = {INT8OID, TEXTOID, TEXTOID, TEXTOID, FLOAT8OID};
		Datum   values[5];
		char    nulls[5] = {' ', ' ', ' ', ' ', ' '};

		static const char *insert_sql =
			"INSERT INTO sage.explain_cache "
			"(queryid, query_text, plan_json, source, total_cost) "
			"VALUES ($1, $2, $3::jsonb, $4, $5)";

		values[0] = Int64GetDatum(queryid);
		values[1] = CStringGetTextDatum(query_text);
		values[2] = CStringGetTextDatum(plan_json);
		values[3] = CStringGetTextDatum(source);
		values[4] = Float8GetDatum(total_cost);

		ret = SPI_execute_with_args(insert_sql, 5, argtypes, values, nulls,
									false, 0);

		if (ret != SPI_OK_INSERT)
		{
			elog(WARNING,
				 "pg_sage: explain_capture — failed to store plan for "
				 "queryid " INT64_FORMAT " (SPI returned %d)",
				 queryid, ret);
		}
		else
		{
			elog(LOG,
				 "pg_sage: explain_capture — captured plan for queryid " INT64_FORMAT " "
				 "(total_cost=%.2f)",
				 queryid, total_cost);
		}
	}
	PG_CATCH();
	{
		FlushErrorState();
		elog(WARNING,
			 "pg_sage: explain_capture — failed to insert plan into "
			 "sage.explain_cache for queryid " INT64_FORMAT "",
			 queryid);
	}
	PG_END_TRY();

	pfree(query_text);
	pfree(plan_json);
	pfree(sql.data);
}

/* ----------------------------------------------------------------
 * sage_explain_capture
 *
 * Public API — captures with source = 'manual'.
 * Caller must have an active SPI connection.
 * ---------------------------------------------------------------- */
void
sage_explain_capture(int64 queryid)
{
	sage_explain_capture_with_source(queryid, "manual");
}

/* ----------------------------------------------------------------
 * sage_explain_capture_auto
 *
 * Captures with source = 'auto' for passive autoexplain captures.
 * Caller must have an active SPI connection.
 * ---------------------------------------------------------------- */
void
sage_explain_capture_auto(int64 queryid)
{
	sage_explain_capture_with_source(queryid, "auto");
}

/* ----------------------------------------------------------------
 * sage_explain_narrate
 *
 * Get a natural-language explanation of a query plan for the
 * given queryid.
 *
 * If an LLM is available, it provides an enhanced analysis.
 * Otherwise we build a structured text analysis by parsing the
 * plan JSON for common issues.
 *
 * Returns a palloc'd string.  Caller must have an active SPI
 * connection.
 * ---------------------------------------------------------------- */
char *
sage_explain_narrate(int64 queryid)
{
	char   *plan_json  = NULL;
	char   *query_text = NULL;
	int     ret;

	/* ---- Step 1: Fetch latest cached plan ---- */
	{
		static const char *fetch_sql =
			"SELECT plan_json, query_text FROM sage.explain_cache "
			"WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1";

		Oid     argtypes[1] = {INT8OID};
		Datum   values[1];
		char    nulls[1] = {' '};

		values[0] = Int64GetDatum(queryid);

		ret = SPI_execute_with_args(fetch_sql, 1, argtypes, values, nulls,
									true, 1);

		if (ret == SPI_OK_SELECT && SPI_processed > 0)
		{
			char *tmp;

			tmp = sage_spi_getval_str(0, 0);
			if (tmp)
				plan_json = pstrdup(tmp);

			tmp = sage_spi_getval_str(0, 1);
			if (tmp)
				query_text = pstrdup(tmp);
		}
	}

	/* ---- Step 2: If no cached plan, capture one now ---- */
	if (plan_json == NULL)
	{
		sage_explain_capture(queryid);

		/* Try fetching again */
		{
			static const char *fetch_sql =
				"SELECT plan_json, query_text FROM sage.explain_cache "
				"WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1";

			Oid     argtypes[1] = {INT8OID};
			Datum   values[1];
			char    nulls[1] = {' '};

			values[0] = Int64GetDatum(queryid);

			ret = SPI_execute_with_args(fetch_sql, 1, argtypes, values, nulls,
										true, 1);

			if (ret == SPI_OK_SELECT && SPI_processed > 0)
			{
				char *tmp;

				tmp = sage_spi_getval_str(0, 0);
				if (tmp)
					plan_json = pstrdup(tmp);

				tmp = sage_spi_getval_str(0, 1);
				if (tmp)
					query_text = pstrdup(tmp);
			}
		}

		/* Still nothing — give up */
		if (plan_json == NULL)
			return NULL;
	}

	/* ---- Step 3: LLM-enhanced narration ---- */
	if (sage_llm_available())
	{
		static const char *system_prompt =
			"You are pg_sage, a PostgreSQL DBA agent. Analyze this EXPLAIN "
			"plan and provide: 1) Plain English summary of what the query "
			"does, 2) Key bottlenecks identified, 3) Specific recommendations "
			"with exact SQL to fix issues.";

		StringInfoData user_prompt;
		char   *llm_result;
		int     tokens_used = 0;

		initStringInfo(&user_prompt);
		appendStringInfoString(&user_prompt, "Query:\n");
		if (query_text)
			appendStringInfoString(&user_prompt, query_text);
		else
			appendStringInfoString(&user_prompt, "(query text unavailable)");

		appendStringInfoString(&user_prompt, "\n\nEXPLAIN plan (JSON):\n");
		appendStringInfoString(&user_prompt, plan_json);

		llm_result = sage_llm_call(system_prompt, user_prompt.data,
								   2048, &tokens_used);

		pfree(user_prompt.data);

		if (llm_result != NULL)
		{
			if (plan_json)
				pfree(plan_json);
			if (query_text)
				pfree(query_text);
			return llm_result;
		}

		/* LLM call failed — fall through to structured analysis */
	}

	/* ---- Step 4: Structured (non-LLM) analysis ---- */
	{
		StringInfoData result;
		char   *top_node;
		double  top_cost;
		double  top_rows;

		initStringInfo(&result);

		top_node = extract_node_type(plan_json);
		top_cost = extract_plan_cost(plan_json);
		top_rows = extract_json_number(plan_json, "Plan Rows");

		appendStringInfo(&result,
			"Query Plan Analysis (queryid: " INT64_FORMAT ")\n"
			"=====================================\n",
			queryid);

		if (query_text)
		{
			appendStringInfo(&result, "Query: %.200s%s\n\n",
				query_text,
				strlen(query_text) > 200 ? "..." : "");
		}

		appendStringInfo(&result,
			"Top-level node: %s (cost: %.2f, rows: %.0f)\n\n",
			top_node ? top_node : "(unknown)",
			top_cost,
			top_rows);

		/* Scan for potential issues */
		appendStringInfoString(&result, "Potential issues:\n");

		{
			StringInfoData issues;
			initStringInfo(&issues);

			build_plan_issues(&issues, plan_json, 0);

			if (issues.len > 0)
			{
				appendStringInfoString(&result, issues.data);
			}
			else
			{
				appendStringInfoString(&result,
					"- No obvious issues detected in plan structure\n");
			}

			pfree(issues.data);
		}

		appendStringInfoString(&result,
			"\nRaw plan available in sage.explain_cache\n");

		if (top_node)
			pfree(top_node);
		if (plan_json)
			pfree(plan_json);
		if (query_text)
			pfree(query_text);

		return result.data;
	}
}

/* ----------------------------------------------------------------
 * sage_explain — SQL-callable
 *
 * sage_explain(queryid bigint) returns text
 *
 * Captures a fresh EXPLAIN plan for the given queryid and returns
 * a human-readable (or LLM-enhanced) narration.
 * ---------------------------------------------------------------- */
Datum
sage_explain(PG_FUNCTION_ARGS)
{
	int64   queryid;
	char   *result;

	if (PG_ARGISNULL(0))
		ereport(ERROR,
				(errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
				 errmsg("queryid must not be NULL")));

	queryid = PG_GETARG_INT64(0);

	SPI_connect();

	/* Capture fresh plan */
	sage_explain_capture(queryid);

	/* Get narration */
	result = sage_explain_narrate(queryid);

	SPI_finish();

	if (result)
		PG_RETURN_TEXT_P(cstring_to_text(result));
	else
		PG_RETURN_TEXT_P(cstring_to_text("No plan available for this queryid."));
}

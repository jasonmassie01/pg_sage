MODULE_big = pg_sage
EXTENSION = pg_sage
DATA = sql/pg_sage--0.1.0.sql sql/pg_sage--0.5.0.sql sql/pg_sage--0.1.0--0.5.0.sql
PGFILEDESC = "pg_sage - Autonomous PostgreSQL DBA Agent"

OBJS = src/pg_sage.o \
       src/guc.o \
       src/collector.o \
       src/analyzer.o \
       src/circuit_breaker.o \
       src/ha.o \
       src/self_monitor.o \
       src/findings.o \
       src/llm.o \
       src/context.o \
       src/explain_capture.o \
       src/briefing.o \
       src/utils.o \
       src/analyzer_extra.o \
       src/action_executor.o \
       src/tier2_extra.o \
       src/mcp_helpers.o \
       src/autoexplain_hook.o

PG_CPPFLAGS = -I$(srcdir)/include
SHLIB_LINK = -lcurl

PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)

# --- PGXN packaging ---
VERSION = 0.5.0

pgxn:
	git archive --format=zip --prefix=pg_sage-$(VERSION)/ -o pg_sage-$(VERSION).zip HEAD

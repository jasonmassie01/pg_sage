FROM postgres:17

RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential \
      postgresql-server-dev-17 \
      git \
      ca-certificates \
      flex \
      bison \
    && git clone --branch REL17_1_7_1 --depth 1 \
       https://github.com/ossc-db/pg_hint_plan.git /tmp/pg_hint_plan \
    && cd /tmp/pg_hint_plan \
    && make PG_CONFIG=/usr/lib/postgresql/17/bin/pg_config \
    && make install PG_CONFIG=/usr/lib/postgresql/17/bin/pg_config \
    && apt-get purge -y build-essential postgresql-server-dev-17 git flex bison \
    && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* /tmp/pg_hint_plan

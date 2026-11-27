# pg_sage Grafana Dashboard

Pre-built Grafana dashboard for the pg_sage sidecar: the process, its monitored databases,
actions and trust, LLM use, Sage SRE and SLOs, and the value and footprint pg_sage reports.
It charts only families the sidecar exports on `/metrics`.

## Prerequisites

- Grafana 10.0+
- Prometheus datasource configured and scraping the pg_sage sidecar `/metrics` endpoint

PostgreSQL internals (connections, cache hits, transactions, dead tuples, deadlocks) are not
on this board. Scrape [postgres_exporter](https://github.com/prometheus-community/postgres_exporter)
and import the community
[PostgreSQL Database dashboard](https://grafana.com/grafana/dashboards/9628) for them.

## Import Steps

1. Open Grafana and navigate to **Dashboards > New > Import**
2. Click **Upload dashboard JSON file** and select `pg_sage_dashboard.json`
3. Select your Prometheus datasource when prompted
4. Click **Import**

Alternatively, paste the JSON content directly into the import text area. The dashboard
keeps the uid `pg-sage`, so importing it again replaces the earlier version.

## Variables

| Variable | Source | Use |
| --- | --- | --- |
| `DS_PROMETHEUS` | datasource | The Prometheus datasource, bound on import |
| `job` | `label_values(pg_sage_info, job)` | The scrape job of the sidecars |
| `instance` | `pg_sage_info` of the chosen jobs | One sidecar, so two sidecars never mix |
| `database` | `pg_sage_database_up` of the chosen sidecars | The monitored databases |

Every panel filters on `job` and `instance`, and every per-database panel on `database`.

## Dashboard Panels

| Row | Panels |
| --- | --- |
| Sidecar | Version, Build, Uptime, Control database, Leader, API certificate expires in, Loop runs and errors, Loop last success age, Metrics refresh age |
| Databases | Databases up, Databases down, Emergency stop, Trust level, Collector breaker, Open findings by severity, Collector and analyzer age, Health score, Database size, Time to first finding, First look findings |
| Executor and trust | Action outcomes, Pending approvals, Since last failed action, Verification verdicts, Policy parks, Verification wait releases, Shadow verdicts |
| LLM | Tokens today vs budget, LLM circuit, LLM enabled, Fleet budget by database, Tuning agent tokens today, Tuning cases deferred |
| SRE and SLO | SLO state, SLO burn rate, Error budget left, Investigations by state, Oldest open investigation, Probe slot wait, Probe refusals |
| Value and footprint | Toil saved, Incidents avoided, Self budget exceeded, pg_sage database time per hour, pg_sage CPU per cycle, sage schema size |
| PostgreSQL internals | Where the PostgreSQL panels went |

A panel whose feature is off, or that has nothing to show yet, says so (for example
"no data: feature off (no SLO)") instead of showing a bare "No data".

## Prometheus Scrape Configuration

Add the sidecar metrics endpoint to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: pg_sage
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:8187']
```

Adjust the target host and port to match your sidecar configuration. Keep the job name
`pg_sage`: the alert rules in `deploy/prometheus/pg_sage.rules.yml` match it.

## Metrics and alerts

Every family the dashboard reads is listed, with its labels and meaning, in the
[metrics reference](../docs/metrics.md), which also lists the families renamed or removed
in v2.6.0 and the runbook of each shipped alert. A test
(`sidecar/cmd/pg_sage_sidecar/grafana_contract_test.go`) fails when a panel queries a family
that is not in that reference, or drops the `job`, `instance` or `database` filter.

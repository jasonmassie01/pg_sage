# pg_sage Grafana Dashboard

Pre-built Grafana dashboard for monitoring pg_sage and PostgreSQL health.

## Prerequisites

- Grafana 10.0+
- Prometheus datasource configured and scraping the pg_sage sidecar `/metrics` endpoint
- postgres_exporter scraping the same database for the Connection & Performance and
  Database Health rows (pg_sage stopped exporting connection and cache-hit families in
  v2.7.0)

## Import Steps

1. Open Grafana and navigate to **Dashboards > New > Import**
2. Click **Upload dashboard JSON file** and select `pg_sage_dashboard.json`
3. Select your Prometheus datasource when prompted
4. Click **Import**

Alternatively, paste the JSON content directly into the import text area.

## Dashboard Panels

| Row | Panels |
|-----|--------|
| Overview | pg_sage version, findings by severity (critical/warning/info), circuit breaker status (DB + LLM), database size |
| Connection & Performance | Active connections over time, cache hit ratio, transactions per second |
| Findings | Table of current findings, bar chart by severity |
| Database Health | Dead tuples over time, disk reads vs cache hits, deadlocks over time |
| Sidecar Health | PostgreSQL uptime, transaction throughput, rollback ratio |

## Datasource

The dashboard uses a templated Prometheus datasource variable (`DS_PROMETHEUS`). On import, Grafana will prompt you to bind it to an existing Prometheus datasource.

## Prometheus Scrape Configuration

Add the sidecar metrics endpoint to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: pg_sage
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:9187']
```

Adjust the target host and port to match your sidecar configuration. Keep the job name
`pg_sage`: the alert rules in `deploy/prometheus/pg_sage.rules.yml` match it.

## Metrics and alerts

Every family the dashboard reads is listed, with its labels and meaning, in the
[metrics reference](../docs/metrics.md), which also lists the families renamed or removed
in v2.7.0 and the runbook of each shipped alert.

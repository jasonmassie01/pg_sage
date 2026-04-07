# pg_sage traffic analytics

Daily snapshots of GitHub's 14-day traffic API, persisted
here because GitHub itself does not retain traffic data
beyond 14 days.

Layout:

    data/YYYY-MM-DD/views.json
    data/YYYY-MM-DD/clones.json
    data/YYYY-MM-DD/referrers.json
    data/YYYY-MM-DD/paths.json

Maintained by `.github/workflows/traffic-snapshot.yml`
on `master`.

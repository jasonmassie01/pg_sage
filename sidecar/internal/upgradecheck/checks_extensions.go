package upgradecheck

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// maxExtensions bounds the installed extensions compared; a database has
// far fewer.
const maxExtensions = 1000

// U2: installed extensions against the target's available versions.
// plpgsql ships with every major, so it is not compared.
var installedSQL = Tag + `SELECT e.extname::text, e.extversion::text
FROM pg_catalog.pg_extension e
WHERE e.extname <> 'plpgsql'
ORDER BY 1
LIMIT $1`

var targetMajorSQL = Tag + `SELECT current_setting('server_version_num')::int`

var availableSQL = Tag + `SELECT v.name::text, array_agg(v.version::text ORDER BY v.version)
FROM pg_catalog.pg_available_extension_versions v
WHERE v.name = ANY($1)
GROUP BY v.name`

type extension struct{ name, version string }

func checkExtensions(ctx context.Context, q pgx.Tx, in input) (Check, error) {
	installed, err := readInstalled(ctx, q)
	if err != nil {
		return Check{}, err
	}
	if len(installed) == 0 {
		return Check{Status: Pass, Rows: []Row{},
			Reason: "no extensions beyond plpgsql are installed"}, nil
	}
	if in.opts.TargetDB == nil {
		return unverifiedExtensions(installed, "no target_database was named: name a "+
			"fleet member already at PostgreSQL "+strconv.Itoa(in.target.Major)+
			" to compare against; pg_sage does not guess what the target ships"), nil
	}
	available, major, err := readAvailable(ctx, in.opts.TargetDB, in.opts.timeout(),
		installed)
	if err != nil {
		return unverifiedExtensions(installed, "the target database could not be read: "+
			err.Error()), nil
	}
	if major != in.target.Major {
		return unverifiedExtensions(installed, fmt.Sprintf("target_database %s runs "+
			"PostgreSQL %d, not the target %d", in.opts.TargetDatabase, major,
			in.target.Major)), nil
	}
	return compareExtensions(installed, available), nil
}

func readInstalled(ctx context.Context, q pgx.Tx) ([]extension, error) {
	rows, err := q.Query(ctx, installedSQL, maxExtensions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []extension
	for rows.Next() {
		var e extension
		if err := rows.Scan(&e.name, &e.version); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// readAvailable reads the target member's major and the versions it
// offers of each installed extension, in its own read-only snapshot.
func readAvailable(ctx context.Context, db *pgxpool.Pool, timeout time.Duration,
	installed []extension) (map[string][]string, int, error) {
	tx, err := catalogread.BeginSnapshot(ctx, db, Tag, timeout)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // read-only
	var versionNum int
	if err := tx.QueryRow(ctx, targetMajorSQL).Scan(&versionNum); err != nil {
		return nil, 0, err
	}
	names := make([]string, len(installed))
	for i, e := range installed {
		names[i] = e.name
	}
	rows, err := tx.Query(ctx, availableSQL, names)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name string
		var versions []string
		if err := rows.Scan(&name, &versions); err != nil {
			return nil, 0, err
		}
		out[name] = versions
	}
	return out, versionNum / 10000, rows.Err()
}

// unverifiedExtensions lists what is installed when the target side is
// not known: unknown, never pass.
func unverifiedExtensions(installed []extension, why string) Check {
	out := Check{Status: Unknown, Total: len(installed), Rows: []Row{},
		Reason: "installed extensions not checked against the target: " + why}
	for _, e := range installed {
		if len(out.Rows) == MaxRows {
			break
		}
		out.Rows = append(out.Rows, Row{"extension": e.name, "installed_version": e.version})
	}
	return out
}

// compareExtensions blocks on an extension the target does not offer and
// warns on one whose installed version it does not offer (the upgrade
// then needs ALTER EXTENSION ... UPDATE along an update path).
func compareExtensions(installed []extension, available map[string][]string) Check {
	out := Check{Status: Pass, Rows: []Row{},
		Reason: "the target offers every installed extension at its installed version"}
	for _, e := range installed {
		versions, ok := available[e.name]
		var row Row
		switch {
		case !ok:
			out.Status = worst(out.Status, Block)
			row = Row{"extension": e.name, "installed_version": e.version,
				"target": "not available"}
		case !slices.Contains(versions, e.version):
			out.Status = worst(out.Status, Warn)
			row = Row{"extension": e.name, "installed_version": e.version,
				"target": "offers " + strings.Join(versions, ", ")}
		default:
			continue
		}
		out.Total++
		if len(out.Rows) < MaxRows {
			out.Rows = append(out.Rows, row)
		}
	}
	switch out.Status {
	case Block:
		out.Reason = "the target does not offer some installed extensions: install " +
			"them on the target (or drop them) before upgrading"
	case Warn:
		out.Reason = "the target offers these extensions only at other versions: plan " +
			"ALTER EXTENSION ... UPDATE after the upgrade and check the update path"
	}
	return out
}

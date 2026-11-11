package optimizer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// opclassExtensions maps an operator class name (as PostgreSQL reads it)
// to the extension that ships it, whatever the access method.
var opclassExtensions = map[string]string{
	"gin_trgm_ops": "pg_trgm", "gist_trgm_ops": "pg_trgm",
	"bit_hamming_ops": "vector", "bit_jaccard_ops": "vector",
	"gin_hstore_ops": "hstore", "gist_hstore_ops": "hstore",
	"gin__int_ops": "intarray", "gist__int_ops": "intarray", "gist__intbig_ops": "intarray",
	"gist_ltree_ops": "ltree", "gist__ltree_ops": "ltree", "gist_cube_ops": "cube",
}

// btreeGistTypes are the scalar types btree_gist ships gist_<type>_ops for.
var btreeGistTypes = []string{
	"oid", "int2", "int4", "int8", "float4", "float8", "numeric", "cash", "bool",
	"timestamp", "timestamptz", "time", "timetz", "date", "interval",
	"macaddr", "macaddr8", "inet", "cidr", "text", "bpchar", "bytea", "bit", "vbit",
	"uuid", "enum",
}

// btreeGinOpclasses are btree_gin's operator classes. Core btree spells
// several of them the same way, so they count only for a GIN index.
var btreeGinOpclasses = map[string]bool{
	"int2_ops": true, "int4_ops": true, "int8_ops": true, "float4_ops": true,
	"float8_ops": true, "money_ops": true, "oid_ops": true, "timestamp_ops": true,
	"timestamptz_ops": true, "time_ops": true, "timetz_ops": true, "date_ops": true,
	"interval_ops": true, "macaddr_ops": true, "macaddr8_ops": true, "inet_ops": true,
	"cidr_ops": true, "text_ops": true, "varchar_ops": true, "char_ops": true,
	"bytea_ops": true, "bit_ops": true, "varbit_ops": true, "numeric_ops": true,
	"enum_ops": true, "uuid_ops": true, "name_ops": true, "bool_ops": true,
	"bpchar_ops": true,
}

// opclassPrefixes map operator class families named by prefix: pgvector's
// per-distance classes and PostGIS's per-dimension classes.
var opclassPrefixes = []struct{ prefix, extension string }{
	{"vector_", "vector"}, {"halfvec_", "vector"}, {"sparsevec_", "vector"},
	{"gist_geometry_ops", "postgis"}, {"gist_geography_ops", "postgis"},
	{"spgist_geometry_ops", "postgis"}, {"spgist_geography_ops", "postgis"},
	{"brin_geometry_inclusion_ops", "postgis"}, {"brin_geography_inclusion_ops", "postgis"},
}

func init() {
	for _, t := range btreeGistTypes {
		opclassExtensions["gist_"+t+"_ops"] = "btree_gist"
	}
}

// opclassExtension returns the extension an operator class of method
// belongs to, or "" for a core or unknown class (PostgreSQL validates it).
func opclassExtension(method, opclass string) string {
	if ext, ok := opclassExtensions[opclass]; ok {
		return ext
	}
	if method == "gin" && btreeGinOpclasses[opclass] {
		return "btree_gin"
	}
	for _, p := range opclassPrefixes {
		if strings.HasPrefix(opclass, p.prefix) && strings.Contains(opclass, "_ops") {
			return p.extension
		}
	}
	return ""
}

// requiredExtensions lists, sorted, the extensions an index DDL needs:
// those shipping the operator classes its keys name, and postgis when a
// key (or a column an expression key references) is geometry or
// geography. Index names, function names and other text are not
// evidence. A DDL the scanner cannot read needs nothing here
// (canonicalization rejects it).
func requiredExtensions(rec Recommendation, tc TableContext) []string {
	method, refs, err := IndexOpclasses(rec.DDL)
	if err != nil {
		return nil
	}
	need := map[string]bool{}
	for _, r := range refs {
		if ext := opclassExtension(method, r.Name); ext != "" {
			need[ext] = true
		}
	}
	if spatialKey(rec.DDL, tc.Columns) {
		need["postgis"] = true
	}
	if len(need) == 0 {
		return nil
	}
	out := make([]string, 0, len(need))
	for ext := range need {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// spatialKey reports a key column, or a column an expression key
// references, whose type is PostGIS geometry or geography.
func spatialKey(ddl string, cols []ColumnInfo) bool {
	keys, err := parseIndexKeys(ddl)
	if err != nil || len(cols) == 0 {
		return false
	}
	spatial := map[string]bool{}
	for _, c := range cols {
		if isSpatialType(c.Type) {
			spatial[strings.ToLower(c.Name)] = true
		}
	}
	for _, k := range keys {
		for _, name := range append([]string{k.column}, k.refs...) {
			if spatial[strings.ToLower(name)] {
				return true
			}
		}
	}
	return false
}

// isSpatialType matches geometry and geography with or without a type
// modifier or a schema qualifier ("public.geometry(Point,4326)").
func isSpatialType(typ string) bool {
	t := strings.ToLower(strings.TrimSpace(typ))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	t = strings.Trim(strings.TrimSpace(t), `"`)
	return t == "geometry" || t == "geography"
}

// installedExtensionsSQL reads which of the named extensions exist.
const installedExtensionsSQL = `/* pg_sage optimizer */ SELECT extname::text
FROM pg_catalog.pg_extension WHERE extname = ANY($1::text[])`

// installedExtensions reports which of names are installed, in one
// bounded read-only catalog read.
func (v *Validator) installedExtensions(
	ctx context.Context, names []string,
) (map[string]bool, error) {
	if v.extensionSource != nil {
		return v.extensionSource(ctx, names)
	}
	rows, err := catalogread.New(v.catalog, v.timeouts).Query(ctx, installedExtensionsSQL, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	installed := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan pg_extension: %w", err)
		}
		installed[name] = true
	}
	return installed, rows.Err()
}

// checkExtensionRequired refuses an index whose operator classes or
// spatial key columns need an extension that is not installed, naming
// each missing one. When the extensions cannot be read it admits and
// logs: PostgreSQL (or HypoPG) refuses a DDL whose operator class does
// not exist, so an unknown is not a refusal.
func (v *Validator) checkExtensionRequired(
	ctx context.Context,
	rec Recommendation,
	tc TableContext,
) (bool, string) {
	required := requiredExtensions(rec, tc)
	if len(required) == 0 || (v.catalog == nil && v.extensionSource == nil) {
		return true, ""
	}
	installed, err := v.installedExtensions(ctx, required)
	if err != nil {
		if v.logFn != nil {
			v.logFn("WARN", "optimizer: extension check for %s skipped: reading "+
				"pg_extension for %s failed: %v", rec.Table, strings.Join(required, ", "), err)
		}
		return true, ""
	}
	var missing []string
	for _, ext := range required {
		if !installed[ext] {
			missing = append(missing, ext)
		}
	}
	return len(missing) == 0, missingExtensionsReason(missing)
}

// missingExtensionsReason names each missing extension and how to add it
// (pg_trgm, btree_gist and postgis are allow-listed on RDS, Cloud SQL,
// AlloyDB and Azure, so CREATE EXTENSION is the fix there too).
func missingExtensionsReason(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	installs := make([]string, len(missing))
	for i, ext := range missing {
		installs[i] = "CREATE EXTENSION " + ext
	}
	noun := "extension which is"
	if len(missing) > 1 {
		noun = "extensions which are"
	}
	return fmt.Sprintf("requires %s %s not installed; install with %s",
		strings.Join(missing, ", "), noun, strings.Join(installs, "; "))
}

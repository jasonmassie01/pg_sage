package diffscan

import (
	"path"
	"strings"
)

// Data-access kinds, in the order files are offered to the model.
const (
	KindNone       = ""
	KindSQL        = "sql"
	KindMigration  = "migration"
	KindSchema     = "schema"
	KindModel      = "model"
	KindRepository = "repository"
	KindDAO        = "dao"
	KindORMCall    = "orm_call"
)

// Class is what a changed file is to Change Review. DataAccess files are
// offered to the model; MigrationCode marks an ORM migration or schema
// file that is not SQL (it needs translating, or CR-20 without a model).
// ORM names the framework when its idiom was recognized.
type Class struct {
	DataAccess    bool
	Kind          string
	ORM           string
	MigrationCode bool
}

// nonCode are extensions never offered: documentation, styles, images,
// fonts, archives and lock files.
var nonCode = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".txt": true,
	".adoc": true, ".css": true, ".scss": true, ".sass": true, ".less": true, ".png": true,
	".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".webp": true,
	".pdf": true, ".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".zip": true,
	".gz": true, ".tar": true, ".lock": true, ".map": true, ".mp4": true, ".mov": true}

var nonCodeNames = map[string]bool{"readme": true, "license": true, "licence": true,
	"changelog": true, "authors": true, "notice": true, "codeowners": true}

// Classify classifies one changed file by its path and, for code outside
// the data-access directories, by the ORM calls or raw SQL in its added
// lines.
func Classify(f File) Class {
	p := strings.ToLower(strings.TrimSpace(f.Path))
	ext := path.Ext(p)
	base := path.Base(p)
	stem := strings.TrimSuffix(base, ext)
	switch {
	case p == "" || f.Binary || nonCode[ext] || nonCodeNames[stem]:
		return Class{}
	case ext == ".sql":
		return Class{DataAccess: true, Kind: KindSQL}
	case base == "schema.prisma":
		return Class{DataAccess: true, Kind: KindSchema, ORM: ORMPrisma, MigrationCode: true}
	case base == "schema.rb":
		return Class{DataAccess: true, Kind: KindSchema, ORM: ORMActiveRecord}
	}
	added := f.AddedText()
	all := added + removedText(f)
	if migrationPath(p) {
		return Class{DataAccess: true, Kind: KindMigration, MigrationCode: true,
			ORM: migrationORM(p, ext, all)}
	}
	if kind := pathKind(p, stem); kind != KindNone {
		return Class{DataAccess: true, Kind: kind, ORM: detectORM(ext, all)}
	}
	if orm := detectORM(ext, added); orm != "" {
		return Class{DataAccess: true, Kind: KindORMCall, ORM: orm}
	}
	return Class{}
}

// removedText is the file's removed lines (a deleted model still names its
// ORM).
func removedText(f File) string {
	var b strings.Builder
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Op == '-' {
				b.WriteString(l.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// migrationPath: db/migrate/, any migrations/ directory, alembic/versions/.
func migrationPath(p string) bool {
	segs := strings.Split(path.Dir(p), "/")
	for i, s := range segs {
		switch {
		case s == "migrate" || s == "migrations" || s == "migration":
			return true
		case s == "versions" && i > 0 && segs[i-1] == "alembic":
			return true
		}
	}
	return false
}

// pathKind is the kind of a model, repository or DAO path.
func pathKind(p, stem string) string {
	for _, s := range strings.Split(path.Dir(p), "/") {
		switch s {
		case "models", "model", "entities", "entity":
			return KindModel
		case "repositories", "repository", "repos", "repo":
			return KindRepository
		case "dao", "daos":
			return KindDAO
		}
	}
	switch {
	case stem == "models" || stem == "model":
		return KindModel
	case strings.HasSuffix(stem, "repository") || strings.HasSuffix(stem, "repo"):
		return KindRepository
	case strings.HasSuffix(stem, "dao"):
		return KindDAO
	}
	return KindNone
}

// migrationORM names a migration's framework: by its path first (alembic,
// db/migrate), else by its code.
func migrationORM(p, ext, code string) string {
	switch {
	case strings.Contains(p, "alembic/"):
		return ORMSQLAlchemy
	case ext == ".rb":
		return ORMActiveRecord
	}
	if orm := detectORM(ext, code); orm != "" && orm != ORMRawSQL {
		return orm
	}
	if ext == ".py" {
		return ORMDjango
	}
	return ""
}

// TouchesDatabase reports whether any file of diff is data access (the
// CLI's skip test).
func TouchesDatabase(diff string) bool {
	for _, f := range Parse(diff) {
		if Classify(f).DataAccess {
			return true
		}
	}
	return false
}

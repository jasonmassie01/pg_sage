package changereview

import (
	"sort"
	"strings"
)

// MigrationTool is what Change Review knows about one migration tool:
// whether it wraps each migration in a transaction by default (which makes
// CREATE INDEX CONCURRENTLY fail), whether its migrations are ORM code
// rather than SQL, and the command that prints the SQL it would run.
type MigrationTool struct {
	Name          string `json:"name"`
	Transactional bool   `json:"transactional"`
	ORM           bool   `json:"orm"`
	ExportCommand string `json:"export_command,omitempty"`
}

// migrationTools is keyed by the lower-case name a request uses; "" is
// plain SQL with no tool.
var migrationTools = map[string]MigrationTool{
	"":               {Name: ""},
	"sql":            {Name: "sql"},
	"flyway":         {Name: "flyway"},
	"liquibase":      {Name: "liquibase", Transactional: true},
	"goose":          {Name: "goose", Transactional: true},
	"golang-migrate": {Name: "golang-migrate"},
	"dbmate":         {Name: "dbmate", Transactional: true},
	"sqitch":         {Name: "sqitch"},
	"atlas":          {Name: "atlas", Transactional: true},
	"rails": {Name: "rails", Transactional: true, ORM: true,
		ExportCommand: "bin/rails db:migrate:up VERSION=<version> with " +
			"ActiveRecord::Base.logger at debug against a scratch database"},
	"django": {Name: "django", Transactional: true, ORM: true,
		ExportCommand: "python manage.py sqlmigrate <app_label> <migration_name>"},
	"alembic": {Name: "alembic", Transactional: true, ORM: true,
		ExportCommand: "alembic upgrade <from_revision>:<to_revision> --sql"},
	"prisma": {Name: "prisma", ORM: true,
		ExportCommand: "npx prisma migrate diff --from-migrations prisma/migrations " +
			"--to-schema-datamodel prisma/schema.prisma --script"},
	"knex": {Name: "knex", Transactional: true, ORM: true,
		ExportCommand: "knex migrate:up --debug against a scratch database"},
	"sequelize": {Name: "sequelize", ORM: true,
		ExportCommand: "sequelize db:migrate --debug against a scratch database"},
	"typeorm": {Name: "typeorm", Transactional: true, ORM: true,
		ExportCommand: "typeorm migration:show, then migration:run against a scratch " +
			"database with logging: true"},
}

// LookupMigrationTool finds a tool by name, ignoring case and spaces.
func LookupMigrationTool(name string) (MigrationTool, bool) {
	t, ok := migrationTools[strings.ToLower(strings.TrimSpace(name))]
	return t, ok
}

// MigrationToolNames lists the known tool names, sorted ("" first).
func MigrationToolNames() []string {
	out := make([]string, 0, len(migrationTools))
	for name := range migrationTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

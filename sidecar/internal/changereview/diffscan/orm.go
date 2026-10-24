package diffscan

import "regexp"

// ORM frameworks the classifier recognizes; ORMRawSQL is SQL text in code.
const (
	ORMActiveRecord = "activerecord"
	ORMDjango       = "django"
	ORMSQLAlchemy   = "sqlalchemy"
	ORMPrisma       = "prisma"
	ORMTypeORM      = "typeorm"
	ORMSequelize    = "sequelize"
	ORMKnex         = "knex"
	ORMGORM         = "gorm"
	ORMJOOQ         = "jooq"
	ORMJPA          = "jpa"
	ORMRawSQL       = "sql"
)

type ormPattern struct {
	orm string
	re  *regexp.Regexp
}

// ormPatterns are each language's ORM call patterns, most specific first.
var ormPatterns = map[string][]ormPattern{
	"ruby": {{ORMActiveRecord, regexp.MustCompile(`\.(?:where|find_by|find_each|joins|` +
		`includes|preload|eager_load|order|pluck|group|having|update_all|delete_all|` +
		`find_by_sql|create!?|update!?|destroy_all)\(|\b(?:has_many|belongs_to|has_one|` +
		`has_and_belongs_to_many|scope)\b|ActiveRecord::|\b(?:add_column|remove_column|` +
		`rename_column|create_table|add_index|change_column)\b`)}},
	"python": {
		{ORMSQLAlchemy, regexp.MustCompile(`\bsession\.(?:query|execute|add|scalars|get)\(|` +
			`\bsqlalchemy\b|\.filter_by\(|\bop\.(?:add_column|drop_column|create_index|` +
			`drop_index|create_table|alter_column|execute)\(|\bselect\([A-Z]`)},
		{ORMDjango, regexp.MustCompile(`\.objects\.|\.(?:select_related|prefetch_related|` +
			`annotate|aggregate|values_list|bulk_create|update_or_create|get_or_create)\(|` +
			`\bmodels\.(?:Model|ForeignKey|OneToOneField|ManyToManyField|\w*Field)\b|` +
			`\bmigrations\.`)},
	},
	"js": {
		{ORMPrisma, regexp.MustCompile(`\bprisma\.\w+\.(?:findMany|findUnique|findFirst|` +
			`create|createMany|update|updateMany|upsert|delete|deleteMany|count|aggregate|` +
			`groupBy)\(|\$queryRaw`)},
		{ORMTypeORM, regexp.MustCompile(`\bgetRepository\(|createQueryBuilder\(|@Entity\(|` +
			`@Column\(|\bqueryRunner\.|MigrationInterface\b`)},
		{ORMSequelize, regexp.MustCompile(`\bsequelize\.|\bqueryInterface\.|\.(?:findAll|` +
			`findOne|findByPk|findAndCountAll|bulkCreate|findOrCreate)\(`)},
		{ORMKnex, regexp.MustCompile(`\bknex(?:\.schema)?[(.]|\.(?:whereIn|whereNotIn|` +
			`innerJoin|leftJoin)\(`)},
	},
	"go": {{ORMGORM, regexp.MustCompile(`\bgorm\.|\b(?:db|tx|DB|gormDB|orm)\.(?:Where|` +
		`Preload|Joins|First|Find|Take|Create|Save|Updates?|Delete|Model|Raw|AutoMigrate)\(`)}},
	"jvm": {
		{ORMJPA, regexp.MustCompile(`@(?:Entity|Table|Query|NamedQuery)\b|\bEntityManager\b|` +
			`\b(?:Jpa|Crud|PagingAndSorting)Repository\b|\.create(?:Native)?Query\(`)},
		{ORMJOOQ, regexp.MustCompile(`\b(?:dsl|ctx|create)\.(?:select|selectFrom|insertInto|` +
			`update|deleteFrom|mergeInto)\(|\bDSL\.(?:select|field|table|using)\(`)},
	},
}

var languages = map[string]string{".rb": "ruby", ".rake": "ruby", ".py": "python",
	".js": "js", ".mjs": "js", ".cjs": "js", ".ts": "js", ".tsx": "js", ".jsx": "js",
	".go": "go", ".java": "jvm", ".kt": "jvm", ".scala": "jvm"}

// codeExtensions may carry raw SQL strings.
var codeExtensions = map[string]bool{".rb": true, ".py": true, ".js": true, ".mjs": true,
	".cjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".go": true, ".java": true,
	".kt": true, ".scala": true, ".cs": true, ".php": true, ".ex": true, ".exs": true,
	".rs": true, ".swift": true}

var rawSQL = regexp.MustCompile(`(?i)\b(?:select\s+[\w*,\s."()]+\s+from\s+["\w]|` +
	`insert\s+into\s+["\w]|update\s+["\w.]+\s+set\s|delete\s+from\s+["\w])`)

// detectORM names the ORM whose idiom code uses, or ORMRawSQL for SQL in
// a code string, or "".
func detectORM(ext, code string) string {
	if code == "" {
		return ""
	}
	for _, p := range ormPatterns[languages[ext]] {
		if p.re.MatchString(code) {
			return p.orm
		}
	}
	if codeExtensions[ext] && rawSQL.MatchString(code) {
		return ORMRawSQL
	}
	return ""
}

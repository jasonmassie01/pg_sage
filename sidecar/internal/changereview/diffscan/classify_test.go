package diffscan

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The data-access classifier (CHECK-RSN-09): migrations, *.sql, ORM
// schemas, models, repositories, DAOs and files whose added lines carry
// an ORM call or raw SQL are offered to the model; documentation, styles,
// images and plain code are not.

type fixtureFile struct {
	path   string
	added  []string
	binary bool
}

// buildDiff is a git diff adding the given lines to each file.
func buildDiff(files []fixtureFile) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\nindex 1111111..2222222 100644\n", f.path,
			f.path)
		if f.binary {
			fmt.Fprintf(&b, "Binary files a/%s and b/%s differ\n", f.path, f.path)
			continue
		}
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n@@ -1,1 +1,%d @@\n context\n", f.path, f.path,
			len(f.added)+1)
		for _, l := range f.added {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

type want struct {
	kind, orm string
	code      bool
}

// thirtyFiles is the 30-file fixture PR: 21 data-access files and 9 that
// are not.
var thirtyFiles = []struct {
	fixtureFile
	want *want // nil: not data access
}{
	{fixtureFile{path: "db/migrate/20261023120000_rename_email.rb", added: []string{
		"class RenameEmail < ActiveRecord::Migration[7.1]",
		"  def change; rename_column :orders, :email, :email_address; end", "end"}},
		&want{KindMigration, ORMActiveRecord, true}},
	{fixtureFile{path: "db/schema.rb", added: []string{
		`  create_table "orders", force: :cascade do |t|`}},
		&want{KindSchema, ORMActiveRecord, false}},
	{fixtureFile{path: "migrations/V2__add_status.sql", added: []string{
		"ALTER TABLE orders ADD COLUMN status text;"}}, &want{KindSQL, "", false}},
	{fixtureFile{path: "db/structure.sql", added: []string{"CREATE TABLE t (id int);"}},
		&want{KindSQL, "", false}},
	{fixtureFile{path: "alembic/versions/3f2a_add_index.py", added: []string{
		"def upgrade():", "    op.create_index('ix_orders_status', 'orders', ['status'])"}},
		&want{KindMigration, ORMSQLAlchemy, true}},
	{fixtureFile{path: "orders/migrations/0007_order_note.py", added: []string{
		"class Migration(migrations.Migration):",
		"    operations = [migrations.AddField('order', 'note', models.TextField())]"}},
		&want{KindMigration, ORMDjango, true}},
	{fixtureFile{path: "prisma/schema.prisma", added: []string{"  status String",
		"  @@index([status, createdAt])"}}, &want{KindSchema, ORMPrisma, true}},
	{fixtureFile{path: "prisma/migrations/20261023_init/migration.sql", added: []string{
		`CREATE INDEX "Order_status_idx" ON "Order"("status");`}},
		&want{KindSQL, "", false}},
	{fixtureFile{path: "app/models/order.rb", added: []string{
		"  has_many :items, dependent: :destroy"}}, &want{KindModel, ORMActiveRecord, false}},
	{fixtureFile{path: "orders/models.py", added: []string{
		"    note = models.TextField(blank=True)"}}, &want{KindModel, ORMDjango, false}},
	{fixtureFile{path: "src/repositories/userRepository.ts", added: []string{
		"  return getRepository(User).find({ where: { active: true } });"}},
		&want{KindRepository, ORMTypeORM, false}},
	{fixtureFile{path: "internal/dao/order_dao.go", added: []string{
		`	err := db.Where("status = ?", s).Find(&orders).Error`}},
		&want{KindDAO, ORMGORM, false}},
	{fixtureFile{path: "app/controllers/orders_controller.rb", added: []string{
		"    @orders = Order.where(status: params[:status]).order(:created_at).limit(50)"}},
		&want{KindORMCall, ORMActiveRecord, false}},
	{fixtureFile{path: "billing/services.py", added: []string{
		"    unpaid = Invoice.objects.filter(paid=False)"}}, &want{KindORMCall, ORMDjango, false}},
	{fixtureFile{path: "api/session_query.py", added: []string{
		"    user = session.query(User).filter_by(id=uid).one()"}},
		&want{KindORMCall, ORMSQLAlchemy, false}},
	{fixtureFile{path: "src/api/users.ts", added: []string{
		"  const users = await prisma.user.findMany({ where: { active: true } });"}},
		&want{KindORMCall, ORMPrisma, false}},
	{fixtureFile{path: "src/jobs/cleanup.js", added: []string{
		"  const stale = await Order.findAll({ where: { status: 'stale' } });"}},
		&want{KindORMCall, ORMSequelize, false}},
	{fixtureFile{path: "src/reports/revenue.js", added: []string{
		"  const total = await knex('orders').where('status', 'paid').sum('total');"}},
		&want{KindORMCall, ORMKnex, false}},
	{fixtureFile{path: "src/main/java/com/acme/OrderRepository.java", added: []string{
		`  @Query("SELECT o FROM Order o WHERE o.status = :s")`}},
		&want{KindRepository, ORMJPA, false}},
	{fixtureFile{path: "src/main/java/com/acme/Reports.java", added: []string{
		"    return dsl.selectFrom(ORDERS).where(ORDERS.STATUS.eq(s)).fetch();"}},
		&want{KindORMCall, ORMJOOQ, false}},
	{fixtureFile{path: "cmd/worker/main.go", added: []string{
		`	rows, err := pool.Query(ctx, "SELECT id FROM orders WHERE status = $1", s)`}},
		&want{KindORMCall, ORMRawSQL, false}},
	{fixtureFile{path: "README.md", added: []string{"Run `rails db:migrate` first."}}, nil},
	{fixtureFile{path: "docs/guide.md", added: []string{"SELECT * FROM orders WHERE id = 1;",
		"Order.where(status: 'x')"}}, nil},
	{fixtureFile{path: "web/src/styles.css", added: []string{".select { color: red; }"}}, nil},
	{fixtureFile{path: "web/public/logo.png", binary: true}, nil},
	{fixtureFile{path: "web/src/components/Button.tsx", added: []string{
		"  return <select onChange={update}>{options}</select>;"}}, nil},
	{fixtureFile{path: "package.json", added: []string{`  "version": "1.2.3",`}}, nil},
	{fixtureFile{path: ".github/workflows/ci.yml", added: []string{"      - run: make test"}},
		nil},
	{fixtureFile{path: "internal/util/strings.go", added: []string{
		"func Title(s string) string { return strings.ToUpper(s[:1]) + s[1:] }"}}, nil},
	{fixtureFile{path: "Makefile", added: []string{"test:", "\tgo test ./..."}}, nil},
}

func fixtureDiff() string {
	files := make([]fixtureFile, len(thirtyFiles))
	for i, f := range thirtyFiles {
		files[i] = f.fixtureFile
	}
	return buildDiff(files)
}

func TestClassify_ThirtyFileFixture(t *testing.T) {
	if len(thirtyFiles) != 30 {
		t.Fatalf("fixture has %d files, want 30", len(thirtyFiles))
	}
	files := Parse(fixtureDiff())
	if len(files) != 30 {
		t.Fatalf("parsed %d files, want 30", len(files))
	}
	for i, f := range files {
		w := thirtyFiles[i].want
		c := Classify(f)
		if w == nil {
			if c.DataAccess || c.Kind != KindNone {
				t.Errorf("%s classified %+v, want not data access", f.Path, c)
			}
			continue
		}
		if !c.DataAccess || c.Kind != w.kind || c.ORM != w.orm || c.MigrationCode != w.code {
			t.Errorf("%s classified %+v, want %+v", f.Path, c, *w)
		}
	}
}

func TestClassify_ORMCallsOnlyCountOnAddedLines(t *testing.T) {
	diff := "diff --git a/app/services/x.rb b/app/services/x.rb\n--- a/app/services/x.rb\n" +
		"+++ b/app/services/x.rb\n@@ -1,3 +1,3 @@\n Order.where(status: 'x')\n" +
		"-  puts 'a'\n+  puts 'b'\n"
	f := Parse(diff)[0]
	if c := Classify(f); c.DataAccess {
		t.Fatalf("an ORM call in context only: %+v, want not data access", c)
	}
}

func TestClassify_DeletedMigrationStillDataAccess(t *testing.T) {
	diff := "diff --git a/db/migrate/1_x.rb b/db/migrate/1_x.rb\ndeleted file mode 100644\n" +
		"--- a/db/migrate/1_x.rb\n+++ /dev/null\n@@ -1 +0,0 @@\n-class X; end\n"
	c := Classify(Parse(diff)[0])
	if !c.DataAccess || c.Kind != KindMigration {
		t.Fatalf("deleted migration = %+v", c)
	}
}

func TestClassify_ZeroFile(t *testing.T) {
	if c := Classify(File{}); c.DataAccess || c.Kind != KindNone || c.ORM != "" {
		t.Fatalf("zero file = %+v", c)
	}
}

func offeredPaths(o Offered) []string {
	var out []string
	for _, f := range o.Files {
		out = append(out, f.Path)
	}
	return out
}

func dataAccessPaths() []string {
	var out []string
	for _, f := range thirtyFiles {
		if f.want != nil {
			out = append(out, f.path)
		}
	}
	return out
}

func TestOffer_OffersEveryDataAccessFileAndNothingElse(t *testing.T) {
	o := Offer(Parse(fixtureDiff()), 200000)
	got, wantPaths := offeredPaths(o), dataAccessPaths()
	sort.Strings(got)
	sort.Strings(wantPaths)
	if strings.Join(got, ",") != strings.Join(wantPaths, ",") || len(o.Cut) != 0 {
		t.Fatalf("offered %v (cut %v), want exactly %v", got, o.Cut, wantPaths)
	}
	total := 0
	for _, f := range o.Files {
		if f.Bytes != len(f.Text) || !strings.Contains(f.Text, f.Path) {
			t.Errorf("%s: bytes %d of %d, text %q", f.Path, f.Bytes, len(f.Text), f.Text)
		}
		total += f.Bytes
	}
	if o.Bytes != total {
		t.Fatalf("offered bytes %d, files sum to %d", o.Bytes, total)
	}
	if !strings.HasPrefix(offeredPaths(o)[0], "db/migrate/") {
		t.Fatalf("migrations come first: %v", offeredPaths(o))
	}
}

func TestOffer_CapCutsWholeFilesAndListsThem(t *testing.T) {
	files := Parse(fixtureDiff())
	for _, max := range []int{0, 1, 600, 1500, 4000} {
		o := Offer(files, max)
		if o.Bytes > max {
			t.Fatalf("max %d: offered %d bytes", max, o.Bytes)
		}
		all := append(offeredPaths(o), o.Cut...)
		sort.Strings(all)
		wantPaths := dataAccessPaths()
		sort.Strings(wantPaths)
		if strings.Join(all, ",") != strings.Join(wantPaths, ",") {
			t.Fatalf("max %d: offered+cut %v, want %v", max, all, wantPaths)
		}
		if max < 4000 && len(o.Cut) == 0 {
			t.Fatalf("max %d cut nothing", max)
		}
	}
}

func TestOffer_EmptyInput(t *testing.T) {
	o := Offer(nil, 1000)
	if len(o.Files) != 0 || len(o.Cut) != 0 || o.Bytes != 0 {
		t.Fatalf("empty offer = %+v", o)
	}
}

func TestTouchesDatabase(t *testing.T) {
	if !TouchesDatabase(fixtureDiff()) {
		t.Fatal("the fixture touches the database")
	}
	readme := buildDiff([]fixtureFile{{path: "README.md", added: []string{"SELECT 1 FROM t"}},
		{path: "web/logo.png", binary: true}})
	if TouchesDatabase(readme) || TouchesDatabase("") {
		t.Fatal("a README-only change does not touch the database")
	}
}

func TestHunkFilter_DataAccessChangesOnlyStableUnderLineShifts(t *testing.T) {
	base := fixtureDiff()
	got := HunkFilter(base)
	if !strings.Contains(got, "rename_column :orders, :email, :email_address") ||
		strings.Contains(got, "rails db:migrate") || strings.Contains(got, "color: red") {
		t.Fatalf("hunk filter kept the wrong files:\n%s", got)
	}
	shifted := strings.ReplaceAll(base, "@@ -1,1 +1,", "@@ -40,1 +40,")
	if HunkFilter(shifted) != got {
		t.Fatal("a line shift changed the filtered hunks")
	}
	edited := strings.Replace(base, ":email_address", ":contact_email", 1)
	if HunkFilter(edited) == got {
		t.Fatal("an edited data-access line kept the filtered hunks")
	}
	docs := strings.Replace(base, "Run `rails db:migrate` first.", "Read the guide.", 1)
	if HunkFilter(docs) != got {
		t.Fatal("a README edit changed the filtered hunks")
	}
	if HunkFilter("") != "" {
		t.Fatal("an empty diff filters to nothing")
	}
}

func TestHunkFilter_Redacts(t *testing.T) {
	diff := buildDiff([]fixtureFile{{path: "db/migrate/1_x.rb",
		added: []string{"# key AKIAIOSFODNN7EXAMPLE"}}})
	if got := HunkFilter(diff); strings.Contains(got, "AKIA") ||
		!strings.Contains(got, Redacted) {
		t.Fatalf("filtered hunks not redacted: %q", got)
	}
}

func TestRenderFile_NumbersAndRedacts(t *testing.T) {
	f := Parse(buildDiff([]fixtureFile{{path: "db/migrate/1_x.rb",
		added: []string{"add_column :t, :c, :text", "# postgres://u:pw@h/db"}}}))[0]
	got := RenderFile(f)
	for _, want := range []string{"db/migrate/1_x.rb", "@@ -1,1 +1,3 @@", "   1   context",
		"   2 + add_column :t, :c, :text", "postgres://" + Redacted + "@h/db"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered file lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, ":pw@") {
		t.Fatalf("rendered file not redacted:\n%s", got)
	}
}

package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

func columns() []Column {
	return []Column{
		{Name: "id", Type: types.KindInt, PrimaryKey: true},
		{Name: "name", Type: types.KindText, NotNull: true},
		{Name: "score", Type: types.KindFloat},
	}
}

func newCatalog(t *testing.T) (*Catalog, *storage.Memory) {
	t.Helper()
	store := storage.NewMemory()
	cat, err := Load(store)
	if err != nil {
		t.Fatalf("Load on an empty store: %v", err)
	}
	return cat, store
}

func TestDefineRequiresExactlyOnePrimaryKey(t *testing.T) {
	def, err := Define("users", columns())
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	if def.PK != 0 || !def.Columns[0].NotNull {
		t.Errorf("primary key at %d, NOT NULL = %v", def.PK, def.Columns[0].NotNull)
	}

	none := []Column{{Name: "a", Type: types.KindInt}}
	if _, err := Define("t", none); !errors.Is(err, ErrNoPrimaryKey) {
		t.Errorf("a table without a primary key gave %v", err)
	}

	two := []Column{
		{Name: "a", Type: types.KindInt, PrimaryKey: true},
		{Name: "b", Type: types.KindInt, PrimaryKey: true},
	}
	if _, err := Define("t", two); !errors.Is(err, ErrNoPrimaryKey) {
		t.Errorf("two primary keys gave %v", err)
	}

	dup := []Column{
		{Name: "a", Type: types.KindInt, PrimaryKey: true},
		{Name: "a", Type: types.KindInt},
	}
	if _, err := Define("t", dup); err == nil {
		t.Error("a duplicate column name should be refused")
	}
	if _, err := Define("t", nil); err == nil {
		t.Error("a table with no columns should be refused")
	}
}

func TestCreateAssignsIdentifiersAndPersists(t *testing.T) {
	cat, store := newCatalog(t)
	def, _ := Define("users", columns())
	table, err := cat.Create(store, def)
	if err != nil {
		t.Fatal(err)
	}
	if table.ID == 0 {
		t.Error("a created table should get a non-zero identifier")
	}

	second, _ := Define("posts", columns())
	other, err := cat.Create(store, second)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == table.ID {
		t.Error("two tables must not share an identifier")
	}

	if _, err := cat.Create(store, def); !errors.Is(err, ErrTableExists) {
		t.Errorf("recreating a table gave %v", err)
	}
}

// TestCatalogSurvivesReopen is the point of storing the schema in the store
// rather than beside it.
func TestCatalogSurvivesReopen(t *testing.T) {
	cat, store := newCatalog(t)
	def, _ := Define("users", columns())
	created, err := cat.Create(store, def)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cat.AddIndex(store, "users", "idx_name", "name"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Load(store)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	table, err := reopened.Get("users")
	if err != nil {
		t.Fatal(err)
	}
	if table.ID != created.ID {
		t.Errorf("identifier changed across reopen: %d then %d", created.ID, table.ID)
	}
	if len(table.Columns) != 3 || table.Columns[1].Name != "name" {
		t.Errorf("columns came back as %v", table.Columns)
	}
	if table.Columns[2].Type != types.KindFloat {
		t.Errorf("column type came back as %v", table.Columns[2].Type)
	}
	if !table.Columns[1].NotNull {
		t.Error("the NOT NULL constraint was lost")
	}
	idx, ok := table.FindIndex("idx_name")
	if !ok || idx.Column != "name" || idx.Pos != 1 {
		t.Errorf("index came back as %v, %v", idx, ok)
	}

	// The identifier counter has to survive too, or a new table would
	// collide with an existing one's key range.
	next, _ := Define("posts", columns())
	fresh, err := reopened.Create(store, next)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == created.ID {
		t.Errorf("the identifier counter restarted: both tables got %d", fresh.ID)
	}
}

func TestIndexLifecycle(t *testing.T) {
	cat, store := newCatalog(t)
	def, _ := Define("users", columns())
	if _, err := cat.Create(store, def); err != nil {
		t.Fatal(err)
	}

	table, idx, err := cat.AddIndex(store, "users", "idx_name", "name")
	if err != nil {
		t.Fatal(err)
	}
	if idx.ID == 0 {
		t.Error("an index should get a non-zero identifier")
	}
	if len(table.Indexes) != 1 {
		t.Fatalf("table has %d indexes", len(table.Indexes))
	}
	if found, ok := table.IndexOn("name"); !ok || found.Name != "idx_name" {
		t.Errorf("IndexOn(name) = %v, %v", found, ok)
	}
	if _, ok := table.IndexOn("score"); ok {
		t.Error("IndexOn(score) should report false")
	}

	if _, _, err := cat.AddIndex(store, "users", "idx_name", "score"); !errors.Is(err, ErrIndexExists) {
		t.Errorf("a duplicate index name gave %v", err)
	}
	if _, _, err := cat.AddIndex(store, "users", "idx_missing", "nosuch"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("an index on a missing column gave %v", err)
	}
	if _, _, err := cat.AddIndex(store, "nosuch", "i", "name"); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("an index on a missing table gave %v", err)
	}

	owner, found, err := cat.FindIndex("idx_name")
	if err != nil || owner.Name != "users" || found.Name != "idx_name" {
		t.Fatalf("FindIndex = %v, %v, %v", owner, found, err)
	}

	if _, _, err := cat.RemoveIndex(store, "idx_name"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cat.FindIndex("idx_name"); !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("FindIndex after removal gave %v", err)
	}
	if _, _, err := cat.RemoveIndex(store, "idx_name"); !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("removing a missing index gave %v", err)
	}
}

func TestDropForgetsTheTable(t *testing.T) {
	cat, store := newCatalog(t)
	def, _ := Define("users", columns())
	if _, err := cat.Create(store, def); err != nil {
		t.Fatal(err)
	}
	if !cat.Has("users") {
		t.Fatal("Has should report the created table")
	}
	if err := cat.Drop(store, "users"); err != nil {
		t.Fatal(err)
	}
	if cat.Has("users") {
		t.Error("Has should report false after Drop")
	}
	if _, err := cat.Get("users"); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("Get after Drop gave %v", err)
	}
	if err := cat.Drop(store, "users"); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("dropping twice gave %v", err)
	}
	if reopened, err := Load(store); err != nil {
		t.Fatal(err)
	} else if reopened.Has("users") {
		t.Error("the dropped table came back after a reload")
	}
}

func TestNamesAndAllAreSorted(t *testing.T) {
	cat, store := newCatalog(t)
	for _, name := range []string{"zebra", "apple", "mango"} {
		def, _ := Define(name, columns())
		if _, err := cat.Create(store, def); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(cat.Names(), ","); got != "apple,mango,zebra" {
		t.Errorf("Names = %s", got)
	}
	all := cat.All()
	if len(all) != 3 || all[0].Name != "apple" {
		t.Errorf("All = %v", all)
	}
}

func TestTableHelpers(t *testing.T) {
	def, _ := Define("users", columns())
	if def.PKColumn().Name != "id" {
		t.Errorf("PKColumn = %v", def.PKColumn())
	}
	if pos, ok := def.ColumnIndex("score"); !ok || pos != 2 {
		t.Errorf("ColumnIndex(score) = %d, %v", pos, ok)
	}
	if _, ok := def.ColumnIndex("nosuch"); ok {
		t.Error("ColumnIndex of a missing column should report false")
	}
	if got := strings.Join(def.ColumnNames(), ","); got != "id,name,score" {
		t.Errorf("ColumnNames = %s", got)
	}

	clone := def.Clone()
	clone.Columns[0].Name = "changed"
	if def.Columns[0].Name != "id" {
		t.Error("Clone shares its column slice with the original")
	}
}

func TestTableSQL(t *testing.T) {
	def, _ := Define("users", columns())
	def.Indexes = []Index{{Name: "idx_name", ID: 1, Column: "name", Pos: 1}}
	sql := def.SQL()
	for _, want := range []string{
		"CREATE TABLE users (",
		"id INT PRIMARY KEY",
		"name TEXT NOT NULL",
		"score FLOAT",
		"CREATE INDEX idx_name ON users (name);",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL() is missing %q:\n%s", want, sql)
		}
	}
}

func TestLoadRejectsCorruptEntries(t *testing.T) {
	store := storage.NewMemory()
	// A catalog key whose value is not JSON.
	store.Put(keycodec.MetaTableKey("broken"), []byte("{not json"))
	if _, err := Load(store); !errors.Is(err, ErrCorruptCatalog) {
		t.Errorf("Load of a broken entry gave %v, want ErrCorruptCatalog", err)
	}
}

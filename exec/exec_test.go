package exec

import (
	"errors"
	"strings"
	"testing"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/parser"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

// A harness is one table in an in-memory store, with a planner over it. No
// keelstore, no files: the operators only ever see the storage interfaces.
type harness struct {
	store   *storage.Memory
	cat     *catalog.Catalog
	planner *plan.Planner
	table   *catalog.Table
}

func newHarness(t *testing.T, indexed bool) *harness {
	t.Helper()
	store := storage.NewMemory()
	cat, err := catalog.Load(store)
	if err != nil {
		t.Fatal(err)
	}
	def, err := catalog.Define("t", []catalog.Column{
		{Name: "id", Type: types.KindInt, PrimaryKey: true},
		{Name: "name", Type: types.KindText, NotNull: true},
		{Name: "score", Type: types.KindFloat},
	})
	if err != nil {
		t.Fatal(err)
	}
	table, err := cat.Create(store, def)
	if err != nil {
		t.Fatal(err)
	}
	if indexed {
		table, _, err = cat.AddIndex(store, "t", "idx_name", "name")
		if err != nil {
			t.Fatal(err)
		}
	}
	return &harness{store: store, cat: cat, planner: plan.New(cat), table: table}
}

func (h *harness) exec(t *testing.T, sql string) int64 {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	p, err := h.planner.Plan(stmt)
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	n, err := RunDML(p, h.store)
	if err != nil {
		t.Fatalf("RunDML(%q): %v", sql, err)
	}
	return n
}

func (h *harness) execErr(t *testing.T, sql string) error {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		return err
	}
	p, err := h.planner.Plan(stmt)
	if err != nil {
		return err
	}
	_, err = RunDML(p, h.store)
	if err == nil {
		t.Fatalf("RunDML(%q) succeeded, want an error", sql)
	}
	return err
}

// rows runs a query and renders the result as "a,b|c,d", which makes an
// expected result readable on one line.
func (h *harness) rows(t *testing.T, sql string) string {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	p, err := h.planner.Plan(stmt)
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	op, err := Build(p, h.store)
	if err != nil {
		t.Fatalf("Build(%q): %v", sql, err)
	}
	defer op.Close()
	return drain(t, op)
}

func drain(t *testing.T, op Operator) string {
	t.Helper()
	var out []string
	for {
		row, ok, err := op.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return strings.Join(out, "|")
		}
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = v.String()
		}
		out = append(out, strings.Join(cells, ","))
	}
}

func seeded(t *testing.T, indexed bool) *harness {
	t.Helper()
	h := newHarness(t, indexed)
	h.exec(t, `INSERT INTO t VALUES
		(3, 'carol', 30.0),
		(1, 'alice', 10.0),
		(4, 'dave', NULL),
		(2, 'bob', 20.0),
		(5, 'erin', 10.0)`)
	return h
}

// ---------------------------------------------------------------------
// scans
// ---------------------------------------------------------------------

// TestSeqScanReturnsPrimaryKeyOrder is the property the key encoding buys:
// the rows go in in a jumbled order and come back sorted, with no sort.
func TestSeqScanReturnsPrimaryKeyOrder(t *testing.T) {
	h := seeded(t, false)
	got := h.rows(t, "SELECT id FROM t")
	if got != "1|2|3|4|5" {
		t.Errorf("SeqScan returned %q, want primary-key order", got)
	}
}

func TestRangeScanReadsOnlyItsSlice(t *testing.T) {
	h := seeded(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT id FROM t WHERE id = 3", "3"},
		{"SELECT id FROM t WHERE id > 3", "4|5"},
		{"SELECT id FROM t WHERE id >= 3", "3|4|5"},
		{"SELECT id FROM t WHERE id < 3", "1|2"},
		{"SELECT id FROM t WHERE id <= 3", "1|2|3"},
		{"SELECT id FROM t WHERE id BETWEEN 2 AND 4", "2|3|4"},
		{"SELECT id FROM t WHERE id > 2 AND id < 5", "3|4"},
		{"SELECT id FROM t WHERE id = 99", ""},
		{"SELECT id FROM t WHERE id > 4 AND id < 2", ""},
	}
	for _, c := range cases {
		if got := h.rows(t, c.sql); got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

func TestIndexScanAgreesWithASequentialScan(t *testing.T) {
	indexed := seeded(t, true)
	plain := seeded(t, false)
	queries := []string{
		"SELECT id FROM t WHERE name = 'bob'",
		"SELECT id FROM t WHERE name > 'bob'",
		"SELECT id FROM t WHERE name BETWEEN 'b' AND 'd'",
		"SELECT id FROM t WHERE name = 'nobody'",
	}
	for _, sql := range queries {
		withIndex := indexed.rows(t, sql)
		withoutIndex := plain.rows(t, sql)
		if withIndex != withoutIndex {
			t.Errorf("%s: index scan gave %q, sequential scan gave %q", sql, withIndex, withoutIndex)
		}
	}
}

// TestIndexScanReturnsIndexedOrder is why an index scan can replace a sort.
func TestIndexScanReturnsIndexedOrder(t *testing.T) {
	h := seeded(t, true)
	if got := h.rows(t, "SELECT name FROM t WHERE name > 'a'"); got != "alice|bob|carol|dave|erin" {
		t.Errorf("index scan returned %q", got)
	}
}

func TestIndexIsMaintainedByEveryWrite(t *testing.T) {
	h := seeded(t, true)

	h.exec(t, "UPDATE t SET name = 'zoe' WHERE id = 1")
	if got := h.rows(t, "SELECT id FROM t WHERE name = 'alice'"); got != "" {
		t.Errorf("the old index entry survived the update: %q", got)
	}
	if got := h.rows(t, "SELECT id FROM t WHERE name = 'zoe'"); got != "1" {
		t.Errorf("the new index entry is missing: %q", got)
	}

	h.exec(t, "DELETE FROM t WHERE id = 2")
	if got := h.rows(t, "SELECT id FROM t WHERE name = 'bob'"); got != "" {
		t.Errorf("the index entry survived the delete: %q", got)
	}

	h.exec(t, "INSERT INTO t VALUES (9, 'bob', 1.0)")
	if got := h.rows(t, "SELECT id FROM t WHERE name = 'bob'"); got != "9" {
		t.Errorf("the insert did not add an index entry: %q", got)
	}
}

// TestIndexCoversNullValues keeps an unbounded index scan equivalent to a
// table scan: every row has an entry, NULL included.
func TestIndexCoversNullValues(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', 1.0), (2, 'b', NULL)")
	table, idx, err := h.cat.AddIndex(h.store, "t", "idx_score", "score")
	if err != nil {
		t.Fatal(err)
	}
	if err := BuildIndex(h.store, table, idx); err != nil {
		t.Fatal(err)
	}

	prefix := keycodec.IndexPrefix(table.ID, idx.ID)
	it := h.store.Scan(prefix, keycodec.PrefixEnd(prefix))
	defer it.Close()
	count := 0
	for it.Next() {
		count++
	}
	if count != 2 {
		t.Errorf("the index holds %d entries, want one per row", count)
	}
}

func TestBuildIndexOnAPopulatedTable(t *testing.T) {
	h := seeded(t, false)
	table, idx, err := h.cat.AddIndex(h.store, "t", "idx_name", "name")
	if err != nil {
		t.Fatal(err)
	}
	if err := BuildIndex(h.store, table, idx); err != nil {
		t.Fatal(err)
	}
	if got := h.rows(t, "SELECT id FROM t WHERE name = 'carol'"); got != "3" {
		t.Errorf("a query through the new index returned %q", got)
	}

	if err := DropIndexEntries(h.store, table, idx); err != nil {
		t.Fatal(err)
	}
	prefix := keycodec.IndexPrefix(table.ID, idx.ID)
	it := h.store.Scan(prefix, keycodec.PrefixEnd(prefix))
	defer it.Close()
	if it.Next() {
		t.Error("DropIndexEntries left entries behind")
	}
}

func TestDropTableDataRemovesRowsAndEntries(t *testing.T) {
	h := seeded(t, true)
	before := h.store.Len()
	if err := DropTableData(h.store, h.table); err != nil {
		t.Fatal(err)
	}
	if h.store.Len() >= before {
		t.Error("DropTableData removed nothing")
	}
	if got := h.rows(t, "SELECT id FROM t"); got != "" {
		t.Errorf("rows survived: %q", got)
	}
}

// ---------------------------------------------------------------------
// filter, project, sort, limit, distinct
// ---------------------------------------------------------------------

func TestFilterKeepsOnlyTrueRows(t *testing.T) {
	h := seeded(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT id FROM t WHERE score > 15.0", "2|3"},
		{"SELECT id FROM t WHERE score IS NULL", "4"},
		{"SELECT id FROM t WHERE score IS NOT NULL", "1|2|3|5"},
		{"SELECT id FROM t WHERE name LIKE '%o%'", "2|3"},
		{"SELECT id FROM t WHERE name NOT LIKE '%o%'", "1|4|5"},
		{"SELECT id FROM t WHERE id IN (1, 3, 99)", "1|3"},
		{"SELECT id FROM t WHERE id NOT IN (1, 3)", "2|4|5"},
		{"SELECT id FROM t WHERE NOT (id > 2)", "1|2"},
		{"SELECT id FROM t WHERE id > 1 AND score < 25.0", "2|5"},
		{"SELECT id FROM t WHERE id = 1 OR id = 5", "1|5"},
	}
	for _, c := range cases {
		if got := h.rows(t, c.sql); got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

// TestUnknownDropsRows pins the three-valued rule end to end: a comparison
// against a NULL column is UNKNOWN, and UNKNOWN is not TRUE.
func TestUnknownDropsRows(t *testing.T) {
	h := seeded(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT id FROM t WHERE score = 30.0", "3"},
		{"SELECT id FROM t WHERE score <> 30.0", "1|2|5"}, // row 4 has NULL: unknown
		{"SELECT id FROM t WHERE NOT (score = 30.0)", "1|2|5"},
		{"SELECT id FROM t WHERE score = NULL", ""},
		{"SELECT id FROM t WHERE score <> NULL", ""},
		{"SELECT id FROM t WHERE score IN (10.0, NULL)", "1|5"},
		{"SELECT id FROM t WHERE score NOT IN (10.0, NULL)", ""},
		{"SELECT id FROM t WHERE score BETWEEN 5.0 AND 25.0", "1|2|5"},
		{"SELECT id FROM t WHERE score > 15.0 OR id = 4", "2|3|4"},
		{"SELECT id FROM t WHERE score > 15.0 AND id = 4", ""},
	}
	for _, c := range cases {
		if got := h.rows(t, c.sql); got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

func TestProjectEvaluatesExpressions(t *testing.T) {
	h := seeded(t, false)
	if got := h.rows(t, "SELECT id * 10, name FROM t WHERE id = 2"); got != "20,bob" {
		t.Errorf("got %q", got)
	}
	if got := h.rows(t, "SELECT score + 1.0 FROM t WHERE id = 4"); got != "NULL" {
		t.Errorf("arithmetic on NULL should stay NULL, got %q", got)
	}
	if got := h.rows(t, "SELECT id > 3 FROM t WHERE id IN (2, 4)"); got != "false|true" {
		t.Errorf("a comparison should project as a boolean, got %q", got)
	}
	if got := h.rows(t, "SELECT score = 1.0 FROM t WHERE id = 4"); got != "NULL" {
		t.Errorf("UNKNOWN should project as NULL, got %q", got)
	}
}

func TestSortOrdersAndPutsNullsFirst(t *testing.T) {
	h := seeded(t, false)
	if got := h.rows(t, "SELECT id FROM t ORDER BY score"); got != "4|1|5|2|3" {
		t.Errorf("ascending sort = %q, want NULL first then 10, 10, 20, 30", got)
	}
	// Descending reverses the keys but not the tie-break: rows 1 and 5
	// both score 10, and the earlier one still comes first.
	if got := h.rows(t, "SELECT id FROM t ORDER BY score DESC"); got != "3|2|1|5|4" {
		t.Errorf("descending sort = %q, want NULL last", got)
	}
	// Ties keep input order, which is primary-key order here.
	if got := h.rows(t, "SELECT id FROM t ORDER BY score, id DESC"); got != "4|5|1|2|3" {
		t.Errorf("two-key sort = %q", got)
	}
}

func TestTopNSortMatchesAFullSort(t *testing.T) {
	h := seeded(t, false)
	full := h.rows(t, "SELECT id FROM t ORDER BY score DESC")
	limited := h.rows(t, "SELECT id FROM t ORDER BY score DESC LIMIT 3")
	if !strings.HasPrefix(full, limited) {
		t.Errorf("top-3 gave %q, which is not the first three of %q", limited, full)
	}
	if got := h.rows(t, "SELECT id FROM t ORDER BY score DESC LIMIT 2 OFFSET 1"); got != "2|1" {
		t.Errorf("top-N with an offset = %q", got)
	}
	if got := h.rows(t, "SELECT id FROM t ORDER BY id LIMIT 0"); got != "" {
		t.Errorf("LIMIT 0 = %q, want nothing", got)
	}
}

func TestLimitAndOffset(t *testing.T) {
	h := seeded(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT id FROM t LIMIT 2", "1|2"},
		{"SELECT id FROM t LIMIT 2 OFFSET 2", "3|4"},
		{"SELECT id FROM t OFFSET 3", "4|5"},
		{"SELECT id FROM t LIMIT 99", "1|2|3|4|5"},
		{"SELECT id FROM t LIMIT 2 OFFSET 99", ""},
	}
	for _, c := range cases {
		if got := h.rows(t, c.sql); got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

func TestDistinct(t *testing.T) {
	h := seeded(t, false)
	if got := h.rows(t, "SELECT DISTINCT score FROM t ORDER BY score"); got != "NULL|10.0|20.0|30.0" {
		t.Errorf("DISTINCT = %q", got)
	}
}

// ---------------------------------------------------------------------
// aggregation
// ---------------------------------------------------------------------

func TestAggregatesWithoutGrouping(t *testing.T) {
	h := seeded(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT COUNT(*) FROM t", "5"},
		{"SELECT COUNT(score) FROM t", "4"}, // NULL is not counted
		{"SELECT SUM(score) FROM t", "70.0"},
		{"SELECT AVG(score) FROM t", "17.5"}, // 70 / 4, not 70 / 5
		{"SELECT MIN(score), MAX(score) FROM t", "10.0,30.0"},
		{"SELECT MIN(name), MAX(name) FROM t", "alice,erin"},
		{"SELECT SUM(id) FROM t", "15"}, // integers stay integral
		{"SELECT COUNT(*), SUM(id) FROM t WHERE id > 3", "2,9"},
	}
	for _, c := range cases {
		if got := h.rows(t, c.sql); got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

// TestAggregatesOverNothing checks the rule that an ungrouped aggregation
// always produces exactly one row.
func TestAggregatesOverNothing(t *testing.T) {
	h := newHarness(t, false)
	if got := h.rows(t, "SELECT COUNT(*) FROM t"); got != "0" {
		t.Errorf("COUNT(*) over an empty table = %q, want 0", got)
	}
	if got := h.rows(t, "SELECT SUM(score), AVG(score), MIN(id), MAX(id) FROM t"); got != "NULL,NULL,NULL,NULL" {
		t.Errorf("the other aggregates over nothing = %q, want NULL", got)
	}
	// A grouped aggregation over nothing produces nothing at all.
	if got := h.rows(t, "SELECT name, COUNT(*) FROM t GROUP BY name"); got != "" {
		t.Errorf("GROUP BY over an empty table = %q", got)
	}
}

func TestGroupBy(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, `INSERT INTO t VALUES
		(1, 'a', 1.0), (2, 'b', 2.0), (3, 'a', 3.0),
		(4, 'c', NULL), (5, 'b', NULL)`)

	// Groups come out in group-key order, because the group key is the
	// order-preserving encoding of the group values.
	if got := h.rows(t, "SELECT name, COUNT(*) FROM t GROUP BY name"); got != "a,2|b,2|c,1" {
		t.Errorf("GROUP BY name = %q", got)
	}
	if got := h.rows(t, "SELECT name, SUM(score) FROM t GROUP BY name"); got != "a,4.0|b,2.0|c,NULL" {
		t.Errorf("SUM by group = %q", got)
	}
	if got := h.rows(t, "SELECT name, COUNT(score) FROM t GROUP BY name"); got != "a,2|b,1|c,0" {
		t.Errorf("COUNT of a nullable column by group = %q", got)
	}
	if got := h.rows(t, "SELECT name FROM t GROUP BY name ORDER BY COUNT(*) DESC, name"); got != "a|b|c" {
		t.Errorf("ordering by an aggregate = %q", got)
	}
	if got := h.rows(t, "SELECT name, COUNT(*) FROM t WHERE score IS NOT NULL GROUP BY name"); got != "a,2|b,1" {
		t.Errorf("a filter before the grouping = %q", got)
	}
}

// TestGroupByNullGroupsThemTogether: NULL is one group here, unlike in a
// comparison, because grouping uses the total order rather than `=`.
func TestGroupByNullGroupsThemTogether(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', NULL), (2, 'b', NULL), (3, 'c', 1.0)")
	if got := h.rows(t, "SELECT score, COUNT(*) FROM t GROUP BY score"); got != "NULL,2|1.0,1" {
		t.Errorf("GROUP BY over NULLs = %q", got)
	}
}

func TestAggregateTypeErrors(t *testing.T) {
	h := seeded(t, false)
	stmt, err := parser.Parse("SELECT SUM(name) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.planner.Plan(stmt)
	if err != nil {
		t.Fatal(err)
	}
	op, err := Build(p, h.store)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if _, _, err := op.Next(); !errors.Is(err, types.ErrType) {
		t.Errorf("SUM over TEXT gave %v, want a type error", err)
	}
}

// ---------------------------------------------------------------------
// DML and constraints
// ---------------------------------------------------------------------

func TestInsertUpdateDeleteRoundTrip(t *testing.T) {
	h := newHarness(t, true)

	if n := h.exec(t, "INSERT INTO t VALUES (1, 'a', 1.0), (2, 'b', 2.0)"); n != 2 {
		t.Errorf("INSERT reported %d rows", n)
	}
	if got := h.rows(t, "SELECT id, name, score FROM t"); got != "1,a,1.0|2,b,2.0" {
		t.Errorf("after insert: %q", got)
	}

	if n := h.exec(t, "UPDATE t SET score = score * 2.0 WHERE id = 2"); n != 1 {
		t.Errorf("UPDATE reported %d rows", n)
	}
	if got := h.rows(t, "SELECT score FROM t WHERE id = 2"); got != "4.0" {
		t.Errorf("after update: %q", got)
	}

	if n := h.exec(t, "DELETE FROM t WHERE id = 1"); n != 1 {
		t.Errorf("DELETE reported %d rows", n)
	}
	if got := h.rows(t, "SELECT id FROM t"); got != "2" {
		t.Errorf("after delete: %q", got)
	}
}

func TestInsertWithNamedColumnsDefaultsTheRestToNull(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t (name, id) VALUES ('a', 1)")
	if got := h.rows(t, "SELECT id, name, score FROM t"); got != "1,a,NULL" {
		t.Errorf("got %q", got)
	}
}

func TestUpdateCanMoveARow(t *testing.T) {
	h := newHarness(t, true)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', 1.0), (5, 'b', 2.0)")
	h.exec(t, "UPDATE t SET id = 9 WHERE id = 1")
	if got := h.rows(t, "SELECT id, name FROM t"); got != "5,b|9,a" {
		t.Errorf("after moving the key: %q", got)
	}
	if got := h.rows(t, "SELECT id FROM t WHERE name = 'a'"); got != "9" {
		t.Errorf("the index should point at the new key, got %q", got)
	}
}

// TestUpdateReadsTheOldRow: every right-hand side sees the row as it was
// before the statement, so a swap swaps.
func TestUpdateReadsTheOldRow(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', 1.0)")
	h.exec(t, "UPDATE t SET score = score + 1.0, id = id + 10")
	if got := h.rows(t, "SELECT id, score FROM t"); got != "11,2.0" {
		t.Errorf("got %q", got)
	}
}

// TestUpdateDoesNotSeeItsOwnWrites is the Halloween problem: a statement
// that moves rows forward in the key space must not visit them twice.
func TestUpdateDoesNotSeeItsOwnWrites(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', 1.0), (2, 'b', 1.0), (3, 'c', 1.0)")
	n := h.exec(t, "UPDATE t SET id = id + 10")
	if n != 3 {
		t.Errorf("the update touched %d rows, want 3", n)
	}
	if got := h.rows(t, "SELECT id FROM t"); got != "11|12|13" {
		t.Errorf("got %q", got)
	}
}

func TestConstraintViolations(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', 1.0)")

	if err := h.execErr(t, "INSERT INTO t VALUES (1, 'b', 2.0)"); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("a duplicate key gave %v", err)
	}
	if err := h.execErr(t, "INSERT INTO t VALUES (2, NULL, 2.0)"); !errors.Is(err, ErrNotNull) {
		t.Errorf("a NULL in a NOT NULL column gave %v", err)
	}
	if err := h.execErr(t, "INSERT INTO t VALUES (NULL, 'b', 2.0)"); !errors.Is(err, ErrNullKey) {
		t.Errorf("a NULL primary key gave %v", err)
	}
	if err := h.execErr(t, "INSERT INTO t VALUES ('x', 'b', 2.0)"); !errors.Is(err, types.ErrType) {
		t.Errorf("a TEXT value in an INT column gave %v", err)
	}

	h.exec(t, "INSERT INTO t VALUES (2, 'b', 2.0)")
	if err := h.execErr(t, "UPDATE t SET id = 1 WHERE id = 2"); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("updating a key onto an existing one gave %v", err)
	}
}

// TestIntegerWidensIntoAFloatColumn is the one implicit conversion keelsql
// performs.
func TestIntegerWidensIntoAFloatColumn(t *testing.T) {
	h := newHarness(t, false)
	h.exec(t, "INSERT INTO t VALUES (1, 'a', 3)")
	if got := h.rows(t, "SELECT score FROM t"); got != "3.0" {
		t.Errorf("got %q, want the integer widened to a float", got)
	}
}

// ---------------------------------------------------------------------
// operators in isolation
// ---------------------------------------------------------------------

func TestOperatorsComposeByHand(t *testing.T) {
	h := seeded(t, false)
	env := plan.TableEnv(h.table)
	mask := []bool{true, true, true}

	// SeqScan -> Filter -> Sort(top-2) -> Project, assembled directly
	// rather than through the planner.
	pred, err := parser.ParseExpr("score IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	node := plan.Plan(&plan.SeqScan{Table: h.table, Mask: mask})
	node = &plan.Filter{Input: node, Pred: pred, Env: env}
	node = &plan.Sort{
		Input: node,
		Keys:  []plan.SortKey{{Expr: &ast.Column{Name: "score"}, Desc: true}},
		Limit: 2,
		Env:   env,
	}
	node = &plan.Project{
		Input: node,
		Exprs: []ast.Expr{&ast.Column{Name: "id"}, &ast.Column{Name: "score"}},
		Names: []string{"id", "score"},
		Env:   env,
	}

	op, err := Build(node, h.store)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if got := drain(t, op); got != "3,30.0|2,20.0" {
		t.Errorf("hand-built pipeline gave %q", got)
	}
	if names := Columns(node); strings.Join(names, ",") != "id,score" {
		t.Errorf("Columns = %v", names)
	}
}

func TestEvalStandsAlone(t *testing.T) {
	env := plan.NewEnv()
	env.Columns["a"] = 0
	row := Row{types.Int(5)}

	cases := []struct{ expr, want string }{
		{"a + 1", "6"},
		{"a * 2 - 3", "7"},
		{"a / 2", "2"},
		{"a % 2", "1"},
		{"-a", "-5"},
		{"a > 4", "true"},
		{"a > 4 AND a < 4", "false"},
		{"NOT (a = 5)", "false"},
		{"a IS NULL", "false"},
		{"a BETWEEN 1 AND 10", "true"},
		{"a IN (4, 5)", "true"},
		{"1 + 2 * 3", "7"},
	}
	for _, c := range cases {
		e, err := parser.ParseExpr(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		v, err := Eval(e, env, row)
		if err != nil {
			t.Fatalf("Eval(%q): %v", c.expr, err)
		}
		if v.String() != c.want {
			t.Errorf("Eval(%q) = %s, want %s", c.expr, v, c.want)
		}
	}
}

func TestEvalErrors(t *testing.T) {
	env := plan.NewEnv()
	env.Columns["a"] = 0
	row := Row{types.Int(5)}

	for _, expr := range []string{"b + 1", "'x' + 1", "a / 0", "COUNT(*)"} {
		e, err := parser.ParseExpr(expr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Eval(e, env, row); err == nil {
			t.Errorf("Eval(%q) succeeded, want an error", expr)
		}
	}
	// A nil predicate is TRUE: a query with no WHERE keeps everything.
	got, err := EvalPredicate(nil, env, row)
	if err != nil || got != types.True {
		t.Errorf("EvalPredicate(nil) = %v, %v", got, err)
	}
}

func TestBuildRejectsNonQueryPlans(t *testing.T) {
	h := newHarness(t, false)
	if _, err := Build(&plan.Transaction{Op: "BEGIN"}, h.store); err == nil {
		t.Error("Build of a transaction plan should fail")
	}
	if _, err := RunDML(&plan.Transaction{Op: "BEGIN"}, h.store); err == nil {
		t.Error("RunDML of a transaction plan should fail")
	}
}

func TestDecodeStoredRow(t *testing.T) {
	h := seeded(t, false)
	blob, err := h.store.Get(keycodec.RowKey(h.table.ID, types.Int(1)))
	if err != nil {
		t.Fatal(err)
	}
	row, err := DecodeStoredRow(h.table, blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(row) != 3 || row[1].AsText() != "alice" {
		t.Errorf("decoded %v", row)
	}
	if _, err := DecodeStoredRow(h.table, []byte{0x42}); err == nil {
		t.Error("a corrupt row should not decode")
	}
}

package plan

import (
	"strings"
	"testing"

	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/parser"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

// fixture builds a catalog with one table:
//
//	CREATE TABLE t (id INT PRIMARY KEY, name TEXT NOT NULL, score FLOAT)
//
// and, when indexed is set, an index on name.
func fixture(t *testing.T, indexed bool) *Planner {
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
	if _, err := cat.Create(store, def); err != nil {
		t.Fatal(err)
	}
	if indexed {
		if _, _, err := cat.AddIndex(store, "t", "idx_name", "name"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := cat.AddIndex(store, "t", "idx_score", "score"); err != nil {
			t.Fatal(err)
		}
	}
	return New(cat)
}

func explainSQL(t *testing.T, p *Planner, sql string) string {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	node, err := p.Plan(stmt)
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	return Explain(node)
}

func planError(t *testing.T, p *Planner, sql string) error {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		return err
	}
	_, err = p.Plan(stmt)
	if err == nil {
		t.Fatalf("Plan(%q) succeeded, want an error", sql)
	}
	return err
}

func TestExplainShapeForAPlainScan(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "SELECT id, name FROM t")
	want := strings.Join([]string{
		"Project columns=[id, name]",
		"  -> SeqScan table=t columns=[id, name]",
	}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// TestPredicatePushdownOnThePrimaryKey is the before-and-after the README
// shows: the same query, once without a usable predicate and once with one.
func TestPredicatePushdownOnThePrimaryKey(t *testing.T) {
	p := fixture(t, false)

	full := explainSQL(t, p, "SELECT name FROM t WHERE score > 1.0")
	if !strings.Contains(full, "SeqScan table=t") {
		t.Errorf("a predicate on an unindexed column should stay a full scan:\n%s", full)
	}
	if !strings.Contains(full, "Filter predicate=score > 1.0") {
		t.Errorf("the predicate should survive as a filter:\n%s", full)
	}

	pushed := explainSQL(t, p, "SELECT name FROM t WHERE id = 5")
	if !strings.Contains(pushed, "RangeScan table=t pk=id range=[5, 5]") {
		t.Errorf("an equality on the primary key should become a point range:\n%s", pushed)
	}
	if strings.Contains(pushed, "Filter") {
		t.Errorf("a fully pushed predicate should leave no filter behind:\n%s", pushed)
	}
}

func TestPushdownRangeShapes(t *testing.T) {
	p := fixture(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT id FROM t WHERE id = 5", "range=[5, 5]"},
		{"SELECT id FROM t WHERE id > 5", "range=(5, +inf)"},
		{"SELECT id FROM t WHERE id >= 5", "range=[5, +inf)"},
		{"SELECT id FROM t WHERE id < 5", "range=(-inf, 5)"},
		{"SELECT id FROM t WHERE id <= 5", "range=(-inf, 5]"},
		{"SELECT id FROM t WHERE id BETWEEN 5 AND 9", "range=[5, 9]"},
		{"SELECT id FROM t WHERE id >= 5 AND id < 9", "range=[5, 9)"},
		{"SELECT id FROM t WHERE 5 < id", "range=(5, +inf)"},
		{"SELECT id FROM t WHERE id = -5", "range=[-5, -5]"},
	}
	for _, c := range cases {
		got := explainSQL(t, p, c.sql)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s\n got %s\nwant it to contain %s", c.sql, got, c.want)
		}
	}
}

func TestPushdownKeepsUnrelatedConjuncts(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "SELECT id FROM t WHERE id > 5 AND name = 'x'")
	if !strings.Contains(got, "range=(5, +inf)") {
		t.Errorf("the primary-key predicate should be pushed:\n%s", got)
	}
	if !strings.Contains(got, "Filter predicate=name = 'x'") {
		t.Errorf("the other predicate should stay a filter:\n%s", got)
	}
}

// TestPushdownRefusesAMismatchedType is a correctness guard, not an
// optimisation: an INT column compared against a float is legal SQL, but a
// float bound would encode into a different part of the key space and
// silently return nothing.
func TestPushdownRefusesAMismatchedType(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "SELECT id FROM t WHERE id > 1.5")
	if strings.Contains(got, "RangeScan") {
		t.Errorf("a FLOAT bound must not be pushed onto an INT key:\n%s", got)
	}
	if !strings.Contains(got, "Filter predicate=id > 1.5") {
		t.Errorf("the predicate should stay a filter:\n%s", got)
	}

	// NULL is never a bound either: the comparison is UNKNOWN for every
	// row, which only the filter can express.
	got = explainSQL(t, p, "SELECT id FROM t WHERE id = NULL")
	if strings.Contains(got, "RangeScan") {
		t.Errorf("`= NULL` must not become a range:\n%s", got)
	}
}

func TestIndexSelection(t *testing.T) {
	indexed := fixture(t, true)
	got := explainSQL(t, indexed, "SELECT id FROM t WHERE name = 'grace'")
	if !strings.Contains(got, "IndexScan table=t index=idx_name on=name range=['grace', 'grace']") {
		t.Errorf("an equality on an indexed column should use the index:\n%s", got)
	}

	unindexed := fixture(t, false)
	got = explainSQL(t, unindexed, "SELECT id FROM t WHERE name = 'grace'")
	if !strings.Contains(got, "SeqScan") {
		t.Errorf("without an index the same query should scan:\n%s", got)
	}
}

// TestPrimaryKeyBeatsASecondaryIndex checks the cost model's ordering: a
// point lookup on the key needs no second fetch, so it wins.
func TestPrimaryKeyBeatsASecondaryIndex(t *testing.T) {
	p := fixture(t, true)
	got := explainSQL(t, p, "SELECT id FROM t WHERE id = 1 AND name = 'x'")
	if !strings.Contains(got, "RangeScan") {
		t.Errorf("the primary key should win:\n%s", got)
	}
	if !strings.Contains(got, "Filter predicate=name = 'x'") {
		t.Errorf("the index predicate should fall back to a filter:\n%s", got)
	}
}

// TestTighterRangeWins compares two indexed columns: an equality is
// narrower than an open-ended range, so the equality's index is chosen.
func TestTighterRangeWins(t *testing.T) {
	p := fixture(t, true)
	got := explainSQL(t, p, "SELECT id FROM t WHERE score > 1.0 AND name = 'x'")
	if !strings.Contains(got, "index=idx_name") {
		t.Errorf("the equality's index should win over the open range:\n%s", got)
	}
}

func TestProjectionPruning(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "SELECT id FROM t")
	if !strings.Contains(got, "SeqScan table=t columns=[id]") {
		t.Errorf("only the projected column should be decoded:\n%s", got)
	}

	got = explainSQL(t, p, "SELECT id FROM t WHERE name = 'x' ORDER BY score")
	if !strings.Contains(got, "columns=[id, name, score]") {
		t.Errorf("filter and sort columns should be decoded too:\n%s", got)
	}

	// COUNT(*) reads no column at all.
	got = explainSQL(t, p, "SELECT COUNT(*) FROM t")
	if !strings.Contains(got, "SeqScan table=t columns=[]") {
		t.Errorf("COUNT(*) should decode nothing:\n%s", got)
	}
}

func TestLimitPushdownMakesATopNSort(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "SELECT id FROM t ORDER BY score DESC LIMIT 3")
	if !strings.Contains(got, "Sort keys=[score DESC] mode=top-3") {
		t.Errorf("LIMIT should turn the sort into a top-N:\n%s", got)
	}

	got = explainSQL(t, p, "SELECT id FROM t ORDER BY score DESC LIMIT 3 OFFSET 2")
	if !strings.Contains(got, "mode=top-5") {
		t.Errorf("the offset has to be added to the top-N bound:\n%s", got)
	}

	got = explainSQL(t, p, "SELECT id FROM t ORDER BY score DESC")
	if !strings.Contains(got, "mode=full") {
		t.Errorf("without a limit the sort stays full:\n%s", got)
	}

	// DISTINCT can shrink the result after the sort, so the limit cannot
	// be pushed through it.
	got = explainSQL(t, p, "SELECT DISTINCT score FROM t ORDER BY score LIMIT 3")
	if !strings.Contains(got, "mode=full") {
		t.Errorf("DISTINCT should block limit pushdown:\n%s", got)
	}
}

func TestSortEliminationWhenTheScanAlreadyOrders(t *testing.T) {
	p := fixture(t, true)

	got := explainSQL(t, p, "SELECT id FROM t ORDER BY id")
	if strings.Contains(got, "Sort") {
		t.Errorf("a scan in primary-key order already satisfies ORDER BY id:\n%s", got)
	}

	got = explainSQL(t, p, "SELECT id FROM t WHERE name > 'a' ORDER BY name")
	if strings.Contains(got, "Sort") {
		t.Errorf("an index scan already satisfies ORDER BY on its column:\n%s", got)
	}

	// Descending needs a reverse scan, which keelstore's forward-only
	// iterator cannot do, so the sort has to stay.
	got = explainSQL(t, p, "SELECT id FROM t ORDER BY id DESC")
	if !strings.Contains(got, "Sort keys=[id DESC]") {
		t.Errorf("a descending order still needs a sort:\n%s", got)
	}

	got = explainSQL(t, p, "SELECT id FROM t ORDER BY score")
	if !strings.Contains(got, "Sort keys=[score ASC]") {
		t.Errorf("ordering by a column the scan does not walk needs a sort:\n%s", got)
	}
}

func TestAggregatePlanShape(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "SELECT name, COUNT(*), SUM(score) FROM t GROUP BY name")
	want := strings.Join([]string{
		"Project columns=[name, COUNT(*), SUM(score)]",
		"  -> HashAggregate group=[name] aggregates=[COUNT(*), SUM(score)]",
		"    -> SeqScan table=t columns=[name, score]",
	}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	got = explainSQL(t, p, "SELECT COUNT(*) FROM t")
	if !strings.Contains(got, "HashAggregate group=none") {
		t.Errorf("an ungrouped aggregate should say so:\n%s", got)
	}
}

func TestFullPipelineExplain(t *testing.T) {
	p := fixture(t, true)
	got := explainSQL(t, p,
		"SELECT name, score FROM t WHERE id BETWEEN 10 AND 20 AND score > 1.0 ORDER BY score DESC LIMIT 2 OFFSET 1")
	want := strings.Join([]string{
		"Limit count=2 offset=1",
		"  -> Project columns=[name, score]",
		"    -> Sort keys=[score DESC] mode=top-3",
		"      -> Filter predicate=score > 1.0",
		"        -> RangeScan table=t pk=id range=[10, 20] columns=[name, score]",
	}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestUpdateAndDeletePlansGetPushdownToo(t *testing.T) {
	p := fixture(t, false)
	got := explainSQL(t, p, "UPDATE t SET name = 'x' WHERE id = 3")
	if !strings.Contains(got, "Update table=t set=[name = 'x']") {
		t.Errorf("update plan:\n%s", got)
	}
	if !strings.Contains(got, "RangeScan table=t pk=id range=[3, 3]") {
		t.Errorf("an UPDATE should get the same pushdown a SELECT does:\n%s", got)
	}
	// A write rewrites the whole row, so nothing is pruned.
	if !strings.Contains(got, "columns=[id, name, score]") {
		t.Errorf("a write needs every column:\n%s", got)
	}

	got = explainSQL(t, p, "DELETE FROM t WHERE id < 3")
	if !strings.Contains(got, "Delete table=t") || !strings.Contains(got, "range=(-inf, 3)") {
		t.Errorf("delete plan:\n%s", got)
	}
}

func TestDDLAndInsertPlans(t *testing.T) {
	p := fixture(t, false)
	cases := []struct{ sql, want string }{
		{"INSERT INTO t VALUES (1, 'a', 1.0), (2, 'b', 2.0)", "Insert table=t rows=2"},
		{"CREATE TABLE u (a INT PRIMARY KEY)", "CreateTable table=u"},
		{"DROP TABLE t", "DropTable table=t"},
		{"CREATE INDEX i ON t (name)", "CreateIndex index=i table=t on=name"},
		{"DROP INDEX i", "DropIndex index=i"},
		{"BEGIN", "BEGIN"},
		{"COMMIT", "COMMIT"},
		{"ROLLBACK", "ROLLBACK"},
	}
	for _, c := range cases {
		if got := explainSQL(t, p, c.sql); got != c.want {
			t.Errorf("Plan(%q) = %q, want %q", c.sql, got, c.want)
		}
	}
}

func TestPlannerErrors(t *testing.T) {
	p := fixture(t, false)
	cases := []struct{ sql, want string }{
		{"SELECT * FROM missing", "no such table: missing"},
		{"SELECT nosuch FROM t", "no such column: t.nosuch"},
		{"SELECT id FROM t WHERE nosuch = 1", "no such column: t.nosuch"},
		{"SELECT id FROM t ORDER BY nosuch", "no such column: t.nosuch"},
		{"SELECT u.id FROM t", "no such table: u"},
		{"INSERT INTO t (nosuch) VALUES (1)", "no such column: t.nosuch"},
		{"INSERT INTO t VALUES (1)", "row 1 has 1 values, table t wants 3"},
		{"INSERT INTO t (id, id) VALUES (1, 2)", `column "id" listed twice`},
		{"UPDATE t SET nosuch = 1", "no such column: t.nosuch"},
		{"SELECT name, COUNT(*) FROM t GROUP BY score", `column "name" must appear in the GROUP BY clause`},
		{"SELECT * FROM t GROUP BY name", "SELECT * cannot be combined with aggregation"},
		{"SELECT name FROM t GROUP BY COUNT(*)", "GROUP BY cannot contain an aggregate"},
		{"SELECT id FROM t ORDER BY COUNT(*)", "must appear in the GROUP BY clause"},
		{"CREATE INDEX i ON t (nosuch)", "no such column: t.nosuch"},
		{"CREATE INDEX i ON missing (a)", "no such table: missing"},
		{"CREATE TABLE u (a INT)", "PRIMARY KEY"},
	}
	for _, c := range cases {
		err := planError(t, p, c.sql)
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Plan(%q)\n got %q\nwant it to contain %q", c.sql, err, c.want)
		}
	}
}

// TestRangeKeyBytes checks the translation from a logical range to the byte
// range the iterator walks, including the exclusive bounds that
// PrefixEnd implements.
func TestRangeKeyBytes(t *testing.T) {
	prefix := []byte{0x02, 0x00, 0x00, 0x00, 0x01}
	enc := func(v types.Value) []byte { return keycodec.Encode(append([]byte(nil), prefix...), v) }

	unbounded := Range{}
	start, end := unbounded.KeyBytes(prefix)
	if string(start) != string(prefix) || string(end) != string(keycodec.PrefixEnd(prefix)) {
		t.Errorf("an unbounded range should cover the whole prefix")
	}

	inclusive := Range{
		Lo: Bound{Value: types.Int(5), Inclusive: true, Set: true},
		Hi: Bound{Value: types.Int(9), Inclusive: true, Set: true},
	}
	start, end = inclusive.KeyBytes(prefix)
	if string(start) != string(enc(types.Int(5))) {
		t.Error("an inclusive lower bound should start exactly at the value")
	}
	if string(end) != string(keycodec.PrefixEnd(enc(types.Int(9)))) {
		t.Error("an inclusive upper bound should end just past the value")
	}

	exclusive := Range{
		Lo: Bound{Value: types.Int(5), Set: true},
		Hi: Bound{Value: types.Int(9), Set: true},
	}
	start, end = exclusive.KeyBytes(prefix)
	if string(start) != string(keycodec.PrefixEnd(enc(types.Int(5)))) {
		t.Error("an exclusive lower bound should start just past the value")
	}
	if string(end) != string(enc(types.Int(9))) {
		t.Error("an exclusive upper bound should end exactly at the value")
	}
}

func TestRangeIntersectAndEmptiness(t *testing.T) {
	ge5 := Range{Lo: Bound{Value: types.Int(5), Inclusive: true, Set: true}}
	lt9 := Range{Hi: Bound{Value: types.Int(9), Set: true}}
	both := ge5.Intersect(lt9)
	if both.String() != "[5, 9)" {
		t.Errorf("intersection = %s", both.String())
	}

	gt5 := Range{Lo: Bound{Value: types.Int(5), Set: true}}
	tighter := ge5.Intersect(gt5)
	if tighter.Lo.Inclusive {
		t.Error("the exclusive bound is tighter and should win")
	}

	contradiction := Range{
		Lo: Bound{Value: types.Int(9), Inclusive: true, Set: true},
		Hi: Bound{Value: types.Int(5), Inclusive: true, Set: true},
	}
	if !contradiction.Empty() {
		t.Error("a > b should be an empty range")
	}
	point := Range{
		Lo: Bound{Value: types.Int(5), Inclusive: true, Set: true},
		Hi: Bound{Value: types.Int(5), Set: true},
	}
	if !point.Empty() {
		t.Error("[5, 5) should be empty")
	}
	if !ge5.Bounded() || (Range{}).Bounded() {
		t.Error("Bounded is wrong")
	}
	if (Range{}).Selectivity() != 1 {
		t.Error("an unbounded range should keep everything")
	}
}

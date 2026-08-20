package keelsql_test

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/aminyx/keelsql/exec"
	"github.com/aminyx/keelsql/types"
)

// The differential test runs thousands of random statements against three
// implementations at once and demands that all three agree:
//
//  1. a keelsql database with a secondary index on k,
//  2. a keelsql database without one,
//  3. a reference model written in plain Go — a map of rows and a
//     hand-written predicate evaluator.
//
// The model is the oracle for *what* the answer is. The second database is
// the oracle for the index: any query that the planner answers through the
// index in one database is answered by a full scan in the other, so an
// index entry that was not maintained shows up immediately as a
// disagreement between two engines that share every other line of code.

// ---------------------------------------------------------------------
// the reference model
// ---------------------------------------------------------------------

// refRow mirrors `CREATE TABLE t (id INT PRIMARY KEY, k INT, s TEXT NOT
// NULL, f FLOAT)`. Nullable columns are pointers, so "absent" is a
// different thing from "zero" without any help from keelsql's own types.
type refRow struct {
	id int64
	k  *int64
	s  string
	f  *float64
}

type model struct {
	rows map[int64]refRow
}

func newModel() *model { return &model{rows: map[int64]refRow{}} }

// sorted returns the rows in primary-key order, which is the order every
// query in this test asks for.
func (m *model) sorted() []refRow {
	out := make([]refRow, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// tri is a three-valued boolean held as a pointer: nil is UNKNOWN. Writing
// it this way, rather than reusing types.Bool3, keeps the oracle from
// inheriting the bug it is supposed to catch.
type tri = *bool

var (
	triTrue  = func() tri { b := true; return &b }()
	triFalse = func() tri { b := false; return &b }()
)

func triOf(b bool) tri {
	if b {
		return triTrue
	}
	return triFalse
}

func triAnd(a, b tri) tri {
	if (a != nil && !*a) || (b != nil && !*b) {
		return triFalse
	}
	if a == nil || b == nil {
		return nil
	}
	return triTrue
}

func triOr(a, b tri) tri {
	if (a != nil && *a) || (b != nil && *b) {
		return triTrue
	}
	if a == nil || b == nil {
		return nil
	}
	return triFalse
}

func triNot(a tri) tri {
	if a == nil {
		return nil
	}
	return triOf(!*a)
}

func triIsTrue(a tri) bool { return a != nil && *a }

// ---------------------------------------------------------------------
// predicates
// ---------------------------------------------------------------------

// A predicate knows how to write itself as SQL and how to decide a model
// row. The two halves are written independently on purpose.
type predicate struct {
	sql   string
	match func(refRow) tri
}

func cmpInt64(a *int64, b int64, op string) tri {
	if a == nil {
		return nil
	}
	switch op {
	case "=":
		return triOf(*a == b)
	case "<>":
		return triOf(*a != b)
	case "<":
		return triOf(*a < b)
	case "<=":
		return triOf(*a <= b)
	case ">":
		return triOf(*a > b)
	case ">=":
		return triOf(*a >= b)
	}
	panic("bad operator " + op)
}

// likePrefix implements the only LIKE shape this test generates: a literal
// prefix followed by '%'.
func likePrefix(s, prefix string) tri { return triOf(strings.HasPrefix(s, prefix)) }

func randomPredicate(rng *rand.Rand) predicate {
	x := int64(rng.Intn(60))
	y := x + int64(rng.Intn(20))

	switch rng.Intn(14) {
	case 0:
		return predicate{
			sql:   fmt.Sprintf("id = %d", x),
			match: func(r refRow) tri { return cmpInt64(&r.id, x, "=") },
		}
	case 1:
		return predicate{
			sql:   fmt.Sprintf("id > %d", x),
			match: func(r refRow) tri { return cmpInt64(&r.id, x, ">") },
		}
	case 2:
		return predicate{
			sql:   fmt.Sprintf("id <= %d", x),
			match: func(r refRow) tri { return cmpInt64(&r.id, x, "<=") },
		}
	case 3:
		return predicate{
			sql: fmt.Sprintf("id BETWEEN %d AND %d", x, y),
			match: func(r refRow) tri {
				return triAnd(cmpInt64(&r.id, x, ">="), cmpInt64(&r.id, y, "<="))
			},
		}
	case 4:
		return predicate{
			sql:   fmt.Sprintf("k = %d", x),
			match: func(r refRow) tri { return cmpInt64(r.k, x, "=") },
		}
	case 5:
		return predicate{
			sql:   fmt.Sprintf("k > %d", x),
			match: func(r refRow) tri { return cmpInt64(r.k, x, ">") },
		}
	case 6:
		return predicate{
			sql: fmt.Sprintf("k BETWEEN %d AND %d", x, y),
			match: func(r refRow) tri {
				return triAnd(cmpInt64(r.k, x, ">="), cmpInt64(r.k, y, "<="))
			},
		}
	case 7:
		return predicate{
			sql:   "k IS NULL",
			match: func(r refRow) tri { return triOf(r.k == nil) },
		}
	case 8:
		return predicate{
			sql:   "k IS NOT NULL",
			match: func(r refRow) tri { return triOf(r.k != nil) },
		}
	case 9:
		letter := string(rune('a' + rng.Intn(4)))
		return predicate{
			sql:   fmt.Sprintf("s LIKE '%s%%'", letter),
			match: func(r refRow) tri { return likePrefix(r.s, letter) },
		}
	case 10:
		return predicate{
			sql: fmt.Sprintf("k IN (%d, %d)", x, y),
			match: func(r refRow) tri {
				return triOr(cmpInt64(r.k, x, "="), cmpInt64(r.k, y, "="))
			},
		}
	case 11:
		return predicate{
			sql: fmt.Sprintf("id > %d AND k = %d", x, y),
			match: func(r refRow) tri {
				return triAnd(cmpInt64(&r.id, x, ">"), cmpInt64(r.k, y, "="))
			},
		}
	case 12:
		return predicate{
			sql: fmt.Sprintf("id < %d OR k IS NULL", x),
			match: func(r refRow) tri {
				return triOr(cmpInt64(&r.id, x, "<"), triOf(r.k == nil))
			},
		}
	default:
		return predicate{
			sql:   fmt.Sprintf("NOT (k = %d)", x),
			match: func(r refRow) tri { return triNot(cmpInt64(r.k, x, "=")) },
		}
	}
}

// ---------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------

// renderRef prints a model row the way keelsql prints a result row. Only
// the formatting is borrowed from types; every decision about which rows
// exist and which values they hold is the model's own.
func renderRef(r refRow) string {
	k := types.Null()
	if r.k != nil {
		k = types.Int(*r.k)
	}
	f := types.Null()
	if r.f != nil {
		f = types.Float(*r.f)
	}
	return strings.Join([]string{
		types.Int(r.id).String(), k.String(), types.Text(r.s).String(), f.String(),
	}, ",")
}

func renderModel(rows []refRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = renderRef(r)
	}
	return strings.Join(out, "|")
}

// ---------------------------------------------------------------------
// the test
// ---------------------------------------------------------------------

const schema = `CREATE TABLE t (id INT PRIMARY KEY, k INT, s TEXT NOT NULL, f FLOAT)`

func TestDifferentialAgainstAReferenceImplementation(t *testing.T) {
	operations := 3000
	if testing.Short() {
		operations = 300
	}

	indexed := open(t)
	plain := open(t)
	mustExec(t, indexed, schema)
	mustExec(t, indexed, "CREATE INDEX idx_k ON t (k)")
	mustExec(t, plain, schema)

	ref := newModel()
	rng := rand.New(rand.NewSource(20260821))

	// bothExec runs a statement on both databases and insists they agree
	// about whether it worked and how many rows it touched.
	bothExec := func(sql string) (int64, error) {
		a, errA := indexed.Exec(sql)
		b, errB := plain.Exec(sql)
		switch {
		case (errA == nil) != (errB == nil):
			t.Fatalf("%s\n  indexed: %v\n  plain:   %v", sql, errA, errB)
		case errA != nil:
			if errA.Error() != errB.Error() {
				t.Fatalf("%s\n  indexed: %v\n  plain:   %v", sql, errA, errB)
			}
			return 0, errA
		case a.Affected != b.Affected:
			t.Fatalf("%s: indexed changed %d rows, plain changed %d", sql, a.Affected, b.Affected)
		}
		return a.Affected, nil
	}

	// bothQuery runs a query on both databases, insists they agree, and
	// returns the shared answer for comparison against the model.
	bothQuery := func(sql string) string {
		a, err := indexed.Exec(sql)
		if err != nil {
			t.Fatalf("%s (indexed): %v", sql, err)
		}
		b, err := plain.Exec(sql)
		if err != nil {
			t.Fatalf("%s (plain): %v", sql, err)
		}
		gotA, gotB := render(a), render(b)
		if gotA != gotB {
			t.Fatalf("%s\n  through the index: %q\n  through a scan:    %q", sql, gotA, gotB)
		}
		return gotA
	}

	inserts, updates, deletes, selects, rejected := 0, 0, 0, 0, 0

	for step := 0; step < operations; step++ {
		switch rng.Intn(10) {

		case 0, 1, 2, 3, 4: // INSERT
			id := int64(rng.Intn(60))
			row := refRow{id: id, s: randomText(rng)}
			if rng.Intn(4) > 0 {
				k := int64(rng.Intn(60))
				row.k = &k
			}
			if rng.Intn(4) > 0 {
				f := float64(rng.Intn(200)-100) / 4
				row.f = &f
			}

			sql := fmt.Sprintf("INSERT INTO t VALUES (%d, %s, '%s', %s)",
				row.id, sqlInt(row.k), row.s, sqlFloat(row.f))
			_, err := bothExec(sql)

			if _, exists := ref.rows[id]; exists {
				if !errors.Is(err, exec.ErrDuplicateKey) {
					t.Fatalf("step %d: %s should have hit a duplicate key, got %v", step, sql, err)
				}
				rejected++
				continue
			}
			if err != nil {
				t.Fatalf("step %d: %s: %v", step, sql, err)
			}
			ref.rows[id] = row
			inserts++

		case 5, 6: // UPDATE
			p := randomPredicate(rng)
			var sql string
			var apply func(refRow) refRow

			if rng.Intn(2) == 0 {
				k := int64(rng.Intn(60))
				sql = fmt.Sprintf("UPDATE t SET k = %d WHERE %s", k, p.sql)
				apply = func(r refRow) refRow { v := k; r.k = &v; return r }
			} else {
				sql = fmt.Sprintf("UPDATE t SET k = NULL, s = 'z' WHERE %s", p.sql)
				apply = func(r refRow) refRow { r.k = nil; r.s = "z"; return r }
			}

			affected, err := bothExec(sql)
			if err != nil {
				t.Fatalf("step %d: %s: %v", step, sql, err)
			}
			want := int64(0)
			for _, r := range ref.sorted() {
				if triIsTrue(p.match(r)) {
					ref.rows[r.id] = apply(r)
					want++
				}
			}
			if affected != want {
				t.Fatalf("step %d: %s changed %d rows, the model changed %d", step, sql, affected, want)
			}
			updates++

		case 7: // DELETE
			p := randomPredicate(rng)
			sql := "DELETE FROM t WHERE " + p.sql
			affected, err := bothExec(sql)
			if err != nil {
				t.Fatalf("step %d: %s: %v", step, sql, err)
			}
			want := int64(0)
			for _, r := range ref.sorted() {
				if triIsTrue(p.match(r)) {
					delete(ref.rows, r.id)
					want++
				}
			}
			if affected != want {
				t.Fatalf("step %d: %s removed %d rows, the model removed %d", step, sql, affected, want)
			}
			deletes++

		default: // SELECT
			p := randomPredicate(rng)
			sql := "SELECT id, k, s, f FROM t WHERE " + p.sql + " ORDER BY id"
			got := bothQuery(sql)

			var want []refRow
			for _, r := range ref.sorted() {
				if triIsTrue(p.match(r)) {
					want = append(want, r)
				}
			}
			if expected := renderModel(want); got != expected {
				t.Fatalf("step %d: %s\n  keelsql: %q\n  model:   %q", step, sql, got, expected)
			}
			selects++
		}
	}

	// A final full comparison, plus the aggregates, which take a different
	// path through the executor than a plain projection does.
	if got, want := bothQuery("SELECT id, k, s, f FROM t ORDER BY id"), renderModel(ref.sorted()); got != want {
		t.Fatalf("final table differs\n  keelsql: %q\n  model:   %q", got, want)
	}
	checkAggregates(t, bothQuery, ref)

	t.Logf("%d operations: %d inserts (%d rejected as duplicates), %d updates, %d deletes, %d selects; %d rows left",
		operations, inserts, rejected, updates, deletes, selects, len(ref.rows))
}

// checkAggregates compares COUNT, SUM, MIN and MAX against the model,
// including their behaviour over NULLs and over an empty table.
func checkAggregates(t *testing.T, ask func(string) string, ref *model) {
	t.Helper()
	rows := ref.sorted()

	count := int64(len(rows))
	var sum int64
	var nonNull int64
	var min, max int64
	for _, r := range rows {
		if r.k == nil {
			continue
		}
		if nonNull == 0 || *r.k < min {
			min = *r.k
		}
		if nonNull == 0 || *r.k > max {
			max = *r.k
		}
		sum += *r.k
		nonNull++
	}

	if got, want := ask("SELECT COUNT(*) FROM t"), fmt.Sprint(count); got != want {
		t.Errorf("COUNT(*) = %s, want %s", got, want)
	}
	if got, want := ask("SELECT COUNT(k) FROM t"), fmt.Sprint(nonNull); got != want {
		t.Errorf("COUNT(k) = %s, want %s", got, want)
	}

	wantSum, wantMin, wantMax := "NULL", "NULL", "NULL"
	if nonNull > 0 {
		wantSum, wantMin, wantMax = fmt.Sprint(sum), fmt.Sprint(min), fmt.Sprint(max)
	}
	if got := ask("SELECT SUM(k) FROM t"); got != wantSum {
		t.Errorf("SUM(k) = %s, want %s", got, wantSum)
	}
	if got := ask("SELECT MIN(k) FROM t"); got != wantMin {
		t.Errorf("MIN(k) = %s, want %s", got, wantMin)
	}
	if got := ask("SELECT MAX(k) FROM t"); got != wantMax {
		t.Errorf("MAX(k) = %s, want %s", got, wantMax)
	}

	// GROUP BY, checked against a model-built histogram.
	histogram := map[string]int{}
	for _, r := range rows {
		key := "NULL"
		if r.k != nil {
			key = fmt.Sprint(*r.k)
		}
		histogram[key]++
	}
	var want []string
	keys := make([]string, 0, len(histogram))
	for key := range histogram {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "NULL" {
			return true
		}
		if keys[j] == "NULL" {
			return false
		}
		return atoi(keys[i]) < atoi(keys[j])
	})
	for _, key := range keys {
		want = append(want, fmt.Sprintf("%s,%d", key, histogram[key]))
	}
	if got := ask("SELECT k, COUNT(*) FROM t GROUP BY k"); got != strings.Join(want, "|") {
		t.Errorf("GROUP BY k\n  keelsql: %q\n  model:   %q", got, strings.Join(want, "|"))
	}
}

func atoi(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}

func randomText(rng *rand.Rand) string {
	letters := "abcd"
	n := 1 + rng.Intn(3)
	out := make([]byte, n)
	for i := range out {
		out[i] = letters[rng.Intn(len(letters))]
	}
	return string(out)
}

func sqlInt(v *int64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(*v)
}

func sqlFloat(v *float64) string {
	if v == nil {
		return "NULL"
	}
	return types.Float(*v).String()
}

// TestDifferentialTransactionRollback is the same idea applied to
// transactions: a rolled-back transaction must leave the database exactly
// where the model says it was.
func TestDifferentialTransactionRollback(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)
	mustExec(t, db, "CREATE INDEX idx_k ON t (k)")

	ref := newModel()
	rng := rand.New(rand.NewSource(7))

	for i := 0; i < 40; i++ {
		row := refRow{id: int64(i), s: randomText(rng)}
		k := int64(rng.Intn(20))
		row.k = &k
		ref.rows[row.id] = row
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d, '%s', NULL)", row.id, k, row.s))
	}
	before := renderModel(ref.sorted())

	conn := db.Conn()
	if _, err := conn.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		p := randomPredicate(rng)
		var sql string
		switch rng.Intn(3) {
		case 0:
			sql = fmt.Sprintf("UPDATE t SET k = %d WHERE %s", rng.Intn(20), p.sql)
		case 1:
			sql = "DELETE FROM t WHERE " + p.sql
		default:
			sql = fmt.Sprintf("INSERT INTO t VALUES (%d, %d, 'x', 1.0)", 100+i, rng.Intn(20))
		}
		if _, err := conn.Exec(sql); err != nil && !errors.Is(err, exec.ErrDuplicateKey) {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := conn.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	if got := query(t, db, "SELECT id, k, s, f FROM t ORDER BY id"); got != before {
		t.Errorf("rollback did not restore the database\n  got:  %q\n  want: %q", got, before)
	}
	// The index has to be back where it started too.
	for i := 0; i < 20; i++ {
		sql := fmt.Sprintf("SELECT id FROM t WHERE k = %d ORDER BY id", i)
		viaIndex := query(t, db, sql)

		var want []string
		for _, r := range ref.sorted() {
			if r.k != nil && *r.k == int64(i) {
				want = append(want, fmt.Sprint(r.id))
			}
		}
		if viaIndex != strings.Join(want, "|") {
			t.Errorf("%s\n  keelsql: %q\n  model:   %q", sql, viaIndex, strings.Join(want, "|"))
		}
	}
}

package ast

import (
	"strings"
	"testing"

	"github.com/aminyx/keelsql/types"
)

func col(name string) *Column    { return &Column{Name: name} }
func lit(v types.Value) *Literal { return &Literal{Value: v} }

// TestPrinterAddsOnlyNecessaryParentheses: EXPLAIN renders predicates by
// asking the tree to print itself, so the printer has to be both correct
// and quiet.
func TestPrinterAddsOnlyNecessaryParentheses(t *testing.T) {
	cases := []struct {
		expr Expr
		want string
	}{
		{
			&Binary{Op: "+", Left: lit(types.Int(1)),
				Right: &Binary{Op: "*", Left: lit(types.Int(2)), Right: lit(types.Int(3))}},
			"1 + 2 * 3",
		},
		{
			&Binary{Op: "*", Left: &Binary{Op: "+", Left: lit(types.Int(1)), Right: lit(types.Int(2))},
				Right: lit(types.Int(3))},
			"(1 + 2) * 3",
		},
		{
			&Binary{Op: "-", Left: lit(types.Int(1)),
				Right: &Binary{Op: "-", Left: lit(types.Int(2)), Right: lit(types.Int(3))}},
			"1 - (2 - 3)",
		},
		{
			&Binary{Op: "AND",
				Left:  &Binary{Op: "=", Left: col("a"), Right: lit(types.Int(1))},
				Right: &Binary{Op: "OR", Left: col("b"), Right: col("c")}},
			"a = 1 AND (b OR c)",
		},
		{
			&Unary{Op: "NOT", Expr: &Binary{Op: "AND", Left: col("a"), Right: col("b")}},
			"NOT (a AND b)",
		},
		{
			&Unary{Op: "NOT", Expr: &Binary{Op: "=", Left: col("a"), Right: lit(types.Int(1))}},
			"NOT a = 1",
		},
		{&Unary{Op: "-", Expr: col("a")}, "-a"},
		{&IsNull{Expr: col("a")}, "a IS NULL"},
		{&IsNull{Expr: col("a"), Not: true}, "a IS NOT NULL"},
		{&Between{Expr: col("a"), Lo: lit(types.Int(1)), Hi: lit(types.Int(5))}, "a BETWEEN 1 AND 5"},
		{&Between{Expr: col("a"), Lo: lit(types.Int(1)), Hi: lit(types.Int(5)), Not: true}, "a NOT BETWEEN 1 AND 5"},
		{&In{Expr: col("a"), List: []Expr{lit(types.Int(1)), lit(types.Int(2))}}, "a IN (1, 2)"},
		{&In{Expr: col("a"), List: []Expr{lit(types.Text("x"))}, Not: true}, "a NOT IN ('x')"},
		{&Like{Expr: col("a"), Pattern: lit(types.Text("x%"))}, "a LIKE 'x%'"},
		{&Like{Expr: col("a"), Pattern: lit(types.Text("x%")), Not: true}, "a NOT LIKE 'x%'"},
		{&FuncCall{Name: "COUNT", Star: true}, "COUNT(*)"},
		{&FuncCall{Name: "SUM", Arg: col("a")}, "SUM(a)"},
		{&Column{Table: "t", Name: "a"}, "t.a"},
		{&Star{}, "*"},
		{lit(types.Null()), "NULL"},
		{lit(types.Text("it's")), "'it''s'"},
	}
	for _, c := range cases {
		if got := c.expr.String(); got != c.want {
			t.Errorf("%T printed %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestQuotedIdentifiersRoundTrip(t *testing.T) {
	if got := (&Column{Name: "Mixed Case"}).String(); got != `"Mixed Case"` {
		t.Errorf("got %s", got)
	}
	if got := (&Column{Name: "plain_1"}).String(); got != "plain_1" {
		t.Errorf("got %s", got)
	}
	if got := (&Column{Name: `has"quote`}).String(); got != `"has""quote"` {
		t.Errorf("got %s", got)
	}
	if got := (&Column{Name: ""}).String(); got != `""` {
		t.Errorf("got %s", got)
	}
}

func TestStatementPrinting(t *testing.T) {
	cases := []struct {
		stmt Statement
		want string
	}{
		{
			&CreateTable{Table: "t", Columns: []ColumnDef{
				{Name: "a", Type: types.KindInt, PrimaryKey: true},
				{Name: "b", Type: types.KindText, NotNull: true},
			}},
			"CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL)",
		},
		{&DropTable{Table: "t", IfExists: true}, "DROP TABLE IF EXISTS t"},
		{&CreateIndex{Name: "i", Table: "t", Column: "b"}, "CREATE INDEX i ON t (b)"},
		{&DropIndex{Name: "i"}, "DROP INDEX i"},
		{
			&Insert{Table: "t", Columns: []string{"a"}, Rows: [][]Expr{{lit(types.Int(1))}}},
			"INSERT INTO t (a) VALUES (1)",
		},
		{&Delete{Table: "t"}, "DELETE FROM t"},
		{
			&Update{Table: "t", Set: []Assignment{{Column: "a", Value: lit(types.Int(1))}},
				Where: &Binary{Op: ">", Left: col("b"), Right: lit(types.Int(2))}},
			"UPDATE t SET a = 1 WHERE b > 2",
		},
		{&Begin{}, "BEGIN"},
		{&Commit{}, "COMMIT"},
		{&Rollback{}, "ROLLBACK"},
		{&Explain{Stmt: &Delete{Table: "t"}}, "EXPLAIN DELETE FROM t"},
	}
	for _, c := range cases {
		if got := c.stmt.String(); got != c.want {
			t.Errorf("%T printed %q, want %q", c.stmt, got, c.want)
		}
	}
}

func TestSelectPrintingCoversEveryClause(t *testing.T) {
	sel := &Select{
		Distinct: true,
		Columns: []ResultColumn{
			{Star: true},
			{Expr: col("a"), Alias: "x"},
		},
		Table:   "t",
		Where:   &Binary{Op: ">", Left: col("a"), Right: lit(types.Int(1))},
		GroupBy: []Expr{col("b")},
		OrderBy: []OrderTerm{{Expr: col("a"), Desc: true}, {Expr: col("b")}},
		Limit:   lit(types.Int(5)),
		Offset:  lit(types.Int(2)),
	}
	want := "SELECT DISTINCT *, a AS x FROM t WHERE a > 1 GROUP BY b ORDER BY a DESC, b ASC LIMIT 5 OFFSET 2"
	if got := sel.String(); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestInspectVisitsEveryChild(t *testing.T) {
	e := &Binary{Op: "AND",
		Left: &Between{Expr: col("a"), Lo: col("b"), Hi: col("c")},
		Right: &In{Expr: &Like{Expr: col("d"), Pattern: col("e")},
			List: []Expr{col("f"), &Unary{Op: "-", Expr: col("g")}}},
	}
	got := strings.Join(Columns(e), ",")
	if got != "a,b,c,d,e,f,g" {
		t.Errorf("Columns = %s", got)
	}

	// Inspect stops descending when the callback says so.
	seen := 0
	Inspect(e, func(Expr) bool { seen++; return false })
	if seen != 1 {
		t.Errorf("returning false should stop after the root, visited %d nodes", seen)
	}
	Inspect(nil, func(Expr) bool { t.Error("Inspect(nil) should visit nothing"); return true })
}

func TestHasAggregateAndIsConstant(t *testing.T) {
	agg := &Binary{Op: "+", Left: &FuncCall{Name: "COUNT", Star: true}, Right: lit(types.Int(1))}
	if !HasAggregate(agg) {
		t.Error("HasAggregate should find a nested call")
	}
	if IsConstant(agg) {
		t.Error("an aggregate is not constant")
	}
	if !IsConstant(&Binary{Op: "+", Left: lit(types.Int(1)), Right: lit(types.Int(2))}) {
		t.Error("1 + 2 is constant")
	}
	if IsConstant(col("a")) {
		t.Error("a column reference is not constant")
	}
	if HasAggregate(col("a")) {
		t.Error("a column reference is not an aggregate")
	}
}

func TestPrecedenceTable(t *testing.T) {
	cases := []struct {
		op   string
		want int
	}{
		{"OR", PrecOr},
		{"AND", PrecAnd},
		{"=", PrecCompare},
		{"<>", PrecCompare},
		{"+", PrecAdd},
		{"*", PrecMul},
		{"%", PrecMul},
		{"NOT", PrecLowest}, // NOT is a prefix operator, not an infix one
	}
	for _, c := range cases {
		if got := BinaryPrecedence(c.op); got != c.want {
			t.Errorf("BinaryPrecedence(%q) = %d, want %d", c.op, got, c.want)
		}
	}
	if Precedence(col("a")) != PrecAtom {
		t.Error("a column reference should bind tightest")
	}
	if Precedence(&Unary{Op: "NOT", Expr: col("a")}) != PrecNot {
		t.Error("NOT should sit at its own level")
	}
	if Precedence(&IsNull{Expr: col("a")}) != PrecCompare {
		t.Error("IS NULL should sit at comparison precedence")
	}
}

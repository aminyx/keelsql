package parser

import (
	"strings"
	"testing"

	"github.com/aminyx/keelsql/ast"
)

// parseOK parses one statement and returns its canonical printed form. The
// printer is precedence-aware, so comparing against a string checks the
// tree's shape, not just its contents.
func parseOK(t *testing.T, src string) string {
	t.Helper()
	stmt, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return stmt.String()
}

func TestParseCreateTable(t *testing.T) {
	cases := []struct{ src, want string }{
		{
			"CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL, c FLOAT)",
			"CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL, c FLOAT)",
		},
		{
			"create table if not exists T (Id integer primary key, Flag bool)",
			"CREATE TABLE IF NOT EXISTS t (id INT PRIMARY KEY, flag BOOL)",
		},
		{
			"CREATE TABLE t (a INT NOT NULL PRIMARY KEY)",
			"CREATE TABLE t (a INT PRIMARY KEY)",
		},
	}
	for _, c := range cases {
		if got := parseOK(t, c.src); got != c.want {
			t.Errorf("Parse(%q)\n got %q\nwant %q", c.src, got, c.want)
		}
	}
}

func TestParseDropAndIndexStatements(t *testing.T) {
	cases := []struct{ src, want string }{
		{"DROP TABLE t", "DROP TABLE t"},
		{"DROP TABLE IF EXISTS t", "DROP TABLE IF EXISTS t"},
		{"CREATE INDEX idx ON t (b)", "CREATE INDEX idx ON t (b)"},
		{"CREATE INDEX IF NOT EXISTS idx ON t (b)", "CREATE INDEX IF NOT EXISTS idx ON t (b)"},
		{"DROP INDEX idx", "DROP INDEX idx"},
		{"DROP INDEX IF EXISTS idx", "DROP INDEX IF EXISTS idx"},
	}
	for _, c := range cases {
		if got := parseOK(t, c.src); got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestParseInsert(t *testing.T) {
	cases := []struct{ src, want string }{
		{"INSERT INTO t VALUES (1, 'a')", "INSERT INTO t VALUES (1, 'a')"},
		{"INSERT INTO t (b, a) VALUES (1, 2), (3, 4)", "INSERT INTO t (b, a) VALUES (1, 2), (3, 4)"},
		{"INSERT INTO t VALUES (NULL, TRUE, -1, 1.5)", "INSERT INTO t VALUES (NULL, true, -1, 1.5)"},
	}
	for _, c := range cases {
		if got := parseOK(t, c.src); got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestParseSelectForms(t *testing.T) {
	cases := []struct{ src, want string }{
		{"SELECT * FROM t", "SELECT * FROM t"},
		{"SELECT a, b FROM t", "SELECT a, b FROM t"},
		{"SELECT DISTINCT a FROM t", "SELECT DISTINCT a FROM t"},
		{"SELECT a AS x FROM t", "SELECT a AS x FROM t"},
		{"SELECT a x FROM t", "SELECT a AS x FROM t"},
		{"SELECT t.a FROM t", "SELECT t.a FROM t"},
		{"SELECT a FROM t WHERE a > 1", "SELECT a FROM t WHERE a > 1"},
		{"SELECT a FROM t ORDER BY a", "SELECT a FROM t ORDER BY a ASC"},
		{"SELECT a FROM t ORDER BY a DESC, b ASC", "SELECT a FROM t ORDER BY a DESC, b ASC"},
		{"SELECT a FROM t LIMIT 5", "SELECT a FROM t LIMIT 5"},
		{"SELECT a FROM t LIMIT 5 OFFSET 2", "SELECT a FROM t LIMIT 5 OFFSET 2"},
		{"SELECT b, COUNT(*) FROM t GROUP BY b", "SELECT b, COUNT(*) FROM t GROUP BY b"},
		{"SELECT SUM(a), AVG(a), MIN(a), MAX(a) FROM t", "SELECT SUM(a), AVG(a), MIN(a), MAX(a) FROM t"},
	}
	for _, c := range cases {
		if got := parseOK(t, c.src); got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestParseUpdateAndDelete(t *testing.T) {
	cases := []struct{ src, want string }{
		{"UPDATE t SET a = 1", "UPDATE t SET a = 1"},
		{"UPDATE t SET a = a + 1, b = 'x' WHERE a < 5", "UPDATE t SET a = a + 1, b = 'x' WHERE a < 5"},
		{"DELETE FROM t", "DELETE FROM t"},
		{"DELETE FROM t WHERE a IS NULL", "DELETE FROM t WHERE a IS NULL"},
	}
	for _, c := range cases {
		if got := parseOK(t, c.src); got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestParseTransactionAndExplain(t *testing.T) {
	cases := []struct{ src, want string }{
		{"BEGIN", "BEGIN"},
		{"begin transaction", "BEGIN"},
		{"COMMIT", "COMMIT"},
		{"ROLLBACK", "ROLLBACK"},
		{"EXPLAIN SELECT a FROM t", "EXPLAIN SELECT a FROM t"},
	}
	for _, c := range cases {
		if got := parseOK(t, c.src); got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

// TestExpressionPrecedence checks the shape of the tree by printing it with
// the minimum number of parentheses: if the tree were wrong, the printer
// would have to put brackets somewhere else.
func TestExpressionPrecedence(t *testing.T) {
	cases := []struct{ src, want string }{
		{"1 + 2 * 3", "1 + 2 * 3"},
		{"(1 + 2) * 3", "(1 + 2) * 3"},
		{"1 - 2 - 3", "1 - 2 - 3"},
		{"1 - (2 - 3)", "1 - (2 - 3)"},
		{"a = 1 AND b = 2", "a = 1 AND b = 2"},
		{"a = 1 OR b = 2 AND c = 3", "a = 1 OR b = 2 AND c = 3"},
		{"(a = 1 OR b = 2) AND c = 3", "(a = 1 OR b = 2) AND c = 3"},
		{"NOT a = 1", "NOT a = 1"},
		{"NOT a AND b", "NOT a AND b"},
		{"NOT (a AND b)", "NOT (a AND b)"},
		{"a + 1 > b * 2", "a + 1 > b * 2"},
		{"-a + 1", "-a + 1"},
		{"a % 3 = 0", "a % 3 = 0"},
		{"a == 1", "a = 1"}, // == is normalised to =
	}
	for _, c := range cases {
		e, err := ParseExpr(c.src)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", c.src, err)
		}
		if got := e.String(); got != c.want {
			t.Errorf("ParseExpr(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestParsePredicates(t *testing.T) {
	cases := []struct{ src, want string }{
		{"a IS NULL", "a IS NULL"},
		{"a IS NOT NULL", "a IS NOT NULL"},
		{"a BETWEEN 1 AND 5", "a BETWEEN 1 AND 5"},
		{"a NOT BETWEEN 1 AND 5", "a NOT BETWEEN 1 AND 5"},
		{"a BETWEEN 1 AND 5 AND b = 2", "a BETWEEN 1 AND 5 AND b = 2"},
		{"a IN (1, 2, 3)", "a IN (1, 2, 3)"},
		{"a NOT IN ('x')", "a NOT IN ('x')"},
		{"a LIKE 'x%'", "a LIKE 'x%'"},
		{"a NOT LIKE '_y'", "a NOT LIKE '_y'"},
		{"a LIKE 'x%' AND b IS NULL", "a LIKE 'x%' AND b IS NULL"},
	}
	for _, c := range cases {
		e, err := ParseExpr(c.src)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", c.src, err)
		}
		if got := e.String(); got != c.want {
			t.Errorf("ParseExpr(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

// TestNegativeLiteralsAreFolded matters to the planner: only a literal can
// become a scan bound, so -5 has to arrive as one value rather than as a
// negation of one.
func TestNegativeLiteralsAreFolded(t *testing.T) {
	e, err := ParseExpr("-5")
	if err != nil {
		t.Fatal(err)
	}
	lit, ok := e.(*ast.Literal)
	if !ok {
		t.Fatalf("ParseExpr(-5) gave %T, want a literal", e)
	}
	if lit.Value.AsInt() != -5 {
		t.Errorf("folded to %v", lit.Value)
	}
	if _, ok := mustParseExpr(t, "-a").(*ast.Unary); !ok {
		t.Error("-a should stay a unary expression")
	}
}

func mustParseExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", src, err)
	}
	return e
}

// TestParseErrorsAreSpecific pins both the message and the position: an
// error that does not say what was expected, and where, is not much of an
// error.
func TestParseErrorsAreSpecific(t *testing.T) {
	cases := []struct {
		src       string
		message   string
		line, col int
	}{
		{"SELECT a WHERE b", "expected FROM, found keyword WHERE", 1, 10},
		{"SELECT a FROM", "expected a table name, found end of input", 1, 14},
		{"SELECT FROM t", "expected an expression, found keyword FROM", 1, 8},
		{"INSERT t VALUES (1)", "expected INTO, found identifier \"t\"", 1, 8},
		{"INSERT INTO t (a) VALUES 1", `expected (, found "1"`, 1, 26},
		{"UPDATE t a = 1", "expected SET, found identifier \"a\"", 1, 10},
		{"DELETE t", "expected FROM, found identifier \"t\"", 1, 8},
		{"CREATE VIEW v", "expected TABLE or INDEX after CREATE, found identifier \"view\"", 1, 8},
		{"CREATE TABLE t (a)", "expected a column type for \"a\", found \")\"", 1, 18},
		{"CREATE TABLE t (a BLOB)", `unknown column type "blob"`, 1, 19},
		{"CREATE TABLE t (a INT PRIMARY)", "expected KEY after PRIMARY, found \")\"", 1, 30},
		{"CREATE INDEX i ON t (a, b)", "keelsql indexes one column", 1, 23},
		{"SELECT a FROM t LIMIT 'x'", "LIMIT wants an integer", 1, 23},
		{"SELECT a FROM t LIMIT -1", "LIMIT must not be negative", 1, 23},
		{"SELECT a FROM t WHERE a IN (SELECT b FROM u)", "subqueries are not supported", 1, 29},
		{"SELECT nosuch(a) FROM t", `unknown function "nosuch"`, 1, 8},
		{"SELECT SUM(*) FROM t", "SUM(*) is not allowed", 1, 12},
		{"SELECT COUNT(SUM(a)) FROM t", "aggregate functions cannot be nested", 1, 20},
		{"SELECT a FROM t WHERE COUNT(*) > 1", "aggregate functions are not allowed in WHERE", 1, 35},
		{"SELECT a FROM t t2", "expected end of statement", 1, 17},
		{"SELECT a FROM t WHERE a NOT b", "expected BETWEEN, IN or LIKE after NOT, found identifier \"b\"", 1, 29},
		{"INSERT INTO t VALUES (a)", "VALUES accepts constant expressions only", 1, 24},
		{"SELECT 9223372036854775808 FROM t", "integer literal 9223372036854775808 is out of range", 1, 8},
		{"EXPLAIN EXPLAIN SELECT a FROM t", "EXPLAIN cannot explain EXPLAIN", 1, 9},
		{"DROP VIEW v", "expected TABLE or INDEX after DROP", 1, 6},
		{"", "expected a statement, found end of input", 1, 1},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.message) {
			t.Errorf("Parse(%q)\n got %q\nwant it to contain %q", c.src, err, c.message)
			continue
		}
		perr, ok := err.(*Error)
		if !ok {
			t.Errorf("Parse(%q) returned %T, want *parser.Error", c.src, err)
			continue
		}
		if perr.Pos.Line != c.line || perr.Pos.Column != c.col {
			t.Errorf("Parse(%q) reported %s, want line %d, column %d",
				c.src, perr.Pos, c.line, c.col)
		}
	}
}

func TestParseErrorMentionsLineAndColumn(t *testing.T) {
	_, err := Parse("SELECT a\nFROM")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 2, column 5") {
		t.Errorf("error = %q, want it to point at line 2, column 5", err)
	}
}

func TestLexicalErrorsSurfaceThroughTheParser(t *testing.T) {
	_, err := Parse("SELECT 'unterminated FROM t")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unterminated string literal") {
		t.Errorf("error = %q", err)
	}
}

func TestParseMany(t *testing.T) {
	stmts, err := ParseMany("CREATE TABLE t (a INT PRIMARY KEY); INSERT INTO t VALUES (1);; SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	if _, ok := stmts[2].(*ast.Select); !ok {
		t.Errorf("last statement is %T, want *ast.Select", stmts[2])
	}
	if _, err := ParseMany("SELECT 1 FROM t SELECT 2 FROM t"); err == nil {
		t.Error("two statements without a semicolon should fail")
	}
	empty, err := ParseMany("  ;; \n")
	if err != nil || len(empty) != 0 {
		t.Errorf("ParseMany of an empty script = %v, %v", empty, err)
	}
}

func TestSemicolonIsOptionalForASingleStatement(t *testing.T) {
	if got := parseOK(t, "SELECT * FROM t;"); got != "SELECT * FROM t" {
		t.Errorf("got %q", got)
	}
}

func TestCommentsAreIgnored(t *testing.T) {
	got := parseOK(t, "-- pick everything\nSELECT * /* really */ FROM t")
	if got != "SELECT * FROM t" {
		t.Errorf("got %q", got)
	}
}

func TestIsAggregate(t *testing.T) {
	for _, name := range []string{"count", "COUNT", "Sum", "avg", "MIN", "max"} {
		if !IsAggregate(name) {
			t.Errorf("IsAggregate(%q) = false", name)
		}
	}
	if IsAggregate("median") {
		t.Error("IsAggregate(median) = true")
	}
}

func TestColumnsAndHelpers(t *testing.T) {
	e := mustParseExpr(t, "a + b > c AND a IN (1, d)")
	got := strings.Join(ast.Columns(e), ",")
	if got != "a,b,c,d" {
		t.Errorf("ast.Columns = %s, want a,b,c,d", got)
	}
	if ast.HasAggregate(e) {
		t.Error("HasAggregate should be false here")
	}
	if !ast.HasAggregate(mustParseExpr(t, "COUNT(*) + 1")) {
		t.Error("HasAggregate should be true for COUNT(*) + 1")
	}
	if !ast.IsConstant(mustParseExpr(t, "1 + 2 * 3")) {
		t.Error("IsConstant should be true for a constant expression")
	}
	if ast.IsConstant(e) {
		t.Error("IsConstant should be false when a column is referenced")
	}
}

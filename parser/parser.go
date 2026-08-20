// Package parser turns tokens into a syntax tree.
//
// It is a hand-written recursive-descent parser with one token of
// lookahead, and precedence climbing for expressions — no yacc, no goyacc,
// no parser generator of any kind. The grammar it accepts is written out in
// the README; every production below has a function with the production's
// name, so the code reads in the same order as the grammar.
//
// Errors carry the position of the offending token and say what was
// expected:
//
//	keelsql: syntax error at line 1, column 10: expected FROM, found keyword WHERE
package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/lexer"
	"github.com/aminyx/keelsql/types"
)

// An Error is a parse error at a known position.
type Error struct {
	Msg string
	Pos lexer.Pos
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("keelsql: syntax error at %s: %s", e.Pos, e.Msg)
}

// A Parser holds the scanning state for one input string.
type Parser struct {
	lex  *lexer.Lexer
	tok  lexer.Token // the token under the cursor
	next lexer.Token // one token of lookahead
	err  error       // the first lexical error, if any
}

// New returns a parser over src.
func New(src string) *Parser {
	p := &Parser{lex: lexer.New(src)}
	p.advance()
	p.advance()
	return p
}

// Parse parses exactly one statement and requires the input to end there,
// apart from an optional trailing semicolon.
func Parse(src string) (ast.Statement, error) {
	p := New(src)
	stmt, err := p.Statement()
	if err != nil {
		return nil, err
	}
	if p.tok.Is(";") {
		p.advance()
	}
	if p.tok.Kind != lexer.EOF {
		return nil, p.errorf("expected end of statement, found %s", p.tok.Describe())
	}
	return stmt, p.err
}

// ParseMany parses a semicolon-separated script. Empty statements between
// semicolons are ignored.
func ParseMany(src string) ([]ast.Statement, error) {
	p := New(src)
	var out []ast.Statement
	for {
		for p.tok.Is(";") {
			p.advance()
		}
		if p.tok.Kind == lexer.EOF {
			return out, p.err
		}
		stmt, err := p.Statement()
		if err != nil {
			return nil, err
		}
		out = append(out, stmt)
		if !p.tok.Is(";") && p.tok.Kind != lexer.EOF {
			return nil, p.errorf("expected \";\", found %s", p.tok.Describe())
		}
	}
}

// ParseExpr parses a bare expression. It exists for tests and for the
// planner's own unit tests; SQL text never reaches it directly.
func ParseExpr(src string) (ast.Expr, error) {
	p := New(src)
	e, err := p.expr(ast.PrecLowest)
	if err != nil {
		return nil, err
	}
	if p.tok.Kind != lexer.EOF {
		return nil, p.errorf("expected end of expression, found %s", p.tok.Describe())
	}
	return e, p.err
}

// ---------------------------------------------------------------------
// cursor
// ---------------------------------------------------------------------

func (p *Parser) advance() {
	p.tok = p.next
	tok, err := p.lex.Next()
	if err != nil {
		if p.err == nil {
			p.err = err
		}
		tok = lexer.Token{Kind: lexer.EOF, Pos: tok.Pos}
	}
	p.next = tok
}

func (p *Parser) errorf(format string, args ...any) error {
	if p.err != nil {
		return p.err // a lexical error explains more than a parse error would
	}
	return &Error{Msg: fmt.Sprintf(format, args...), Pos: p.tok.Pos}
}

// expect consumes a keyword or operator, or reports what was found instead.
func (p *Parser) expect(text string) error {
	if !p.tok.Is(text) {
		return p.errorf("expected %s, found %s", text, p.tok.Describe())
	}
	p.advance()
	return nil
}

// accept consumes a keyword or operator if it is there.
func (p *Parser) accept(text string) bool {
	if p.tok.Is(text) {
		p.advance()
		return true
	}
	return false
}

// acceptSeq consumes a run of keywords, but only if all of them are present.
func (p *Parser) acceptSeq(words ...string) bool {
	saved, savedNext, savedLex := p.tok, p.next, *p.lex
	for _, w := range words {
		if !p.tok.Is(w) {
			p.tok, p.next, *p.lex = saved, savedNext, savedLex
			return false
		}
		p.advance()
	}
	return true
}

// ident consumes an identifier. what names the thing being read, so the
// error can say "expected table name" rather than "expected identifier".
func (p *Parser) ident(what string) (string, error) {
	if p.tok.Kind != lexer.Ident {
		return "", p.errorf("expected %s, found %s", what, p.tok.Describe())
	}
	name := p.tok.Text
	p.advance()
	return name, nil
}

// ---------------------------------------------------------------------
// statements
// ---------------------------------------------------------------------

// Statement parses one statement, dispatching on its leading keyword.
func (p *Parser) Statement() (ast.Statement, error) {
	if p.err != nil {
		return nil, p.err
	}
	if p.tok.Kind != lexer.Keyword {
		return nil, p.errorf("expected a statement, found %s", p.tok.Describe())
	}
	switch p.tok.Text {
	case "SELECT":
		return p.selectStmt()
	case "INSERT":
		return p.insertStmt()
	case "UPDATE":
		return p.updateStmt()
	case "DELETE":
		return p.deleteStmt()
	case "CREATE":
		return p.createStmt()
	case "DROP":
		return p.dropStmt()
	case "EXPLAIN":
		return p.explainStmt()
	case "BEGIN":
		p.advance()
		p.accept("TRANSACTION")
		return &ast.Begin{}, nil
	case "COMMIT":
		p.advance()
		p.accept("TRANSACTION")
		return &ast.Commit{}, nil
	case "ROLLBACK":
		p.advance()
		p.accept("TRANSACTION")
		return &ast.Rollback{}, nil
	}
	return nil, p.errorf("expected a statement, found %s", p.tok.Describe())
}

// explainStmt parses `EXPLAIN <statement>`.
func (p *Parser) explainStmt() (ast.Statement, error) {
	if err := p.expect("EXPLAIN"); err != nil {
		return nil, err
	}
	if p.tok.Is("EXPLAIN") {
		return nil, p.errorf("EXPLAIN cannot explain EXPLAIN")
	}
	inner, err := p.Statement()
	if err != nil {
		return nil, err
	}
	return &ast.Explain{Stmt: inner}, nil
}

// selectStmt parses
//
//	SELECT [DISTINCT] <result list> FROM <table>
//	  [WHERE <expr>] [GROUP BY <exprs>] [ORDER BY <terms>]
//	  [LIMIT <expr>] [OFFSET <expr>]
func (p *Parser) selectStmt() (ast.Statement, error) {
	if err := p.expect("SELECT"); err != nil {
		return nil, err
	}
	sel := &ast.Select{Distinct: p.accept("DISTINCT")}

	for {
		col, err := p.resultColumn()
		if err != nil {
			return nil, err
		}
		sel.Columns = append(sel.Columns, col)
		if !p.accept(",") {
			break
		}
	}

	if err := p.expect("FROM"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	sel.Table = table

	if p.accept("WHERE") {
		if sel.Where, err = p.expr(ast.PrecLowest); err != nil {
			return nil, err
		}
		if ast.HasAggregate(sel.Where) {
			return nil, p.errorf("aggregate functions are not allowed in WHERE")
		}
	}

	if p.accept("GROUP") {
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expr(ast.PrecLowest)
			if err != nil {
				return nil, err
			}
			sel.GroupBy = append(sel.GroupBy, e)
			if !p.accept(",") {
				break
			}
		}
	}

	if p.accept("ORDER") {
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expr(ast.PrecLowest)
			if err != nil {
				return nil, err
			}
			term := ast.OrderTerm{Expr: e}
			switch {
			case p.accept("DESC"):
				term.Desc = true
			case p.accept("ASC"):
			}
			sel.OrderBy = append(sel.OrderBy, term)
			if !p.accept(",") {
				break
			}
		}
	}

	if p.accept("LIMIT") {
		if sel.Limit, err = p.countExpr("LIMIT"); err != nil {
			return nil, err
		}
	}
	if p.accept("OFFSET") {
		if sel.Offset, err = p.countExpr("OFFSET"); err != nil {
			return nil, err
		}
	}
	return sel, nil
}

// countExpr parses the constant integer that follows LIMIT or OFFSET.
func (p *Parser) countExpr(clause string) (ast.Expr, error) {
	pos := p.tok.Pos
	e, err := p.expr(ast.PrecLowest)
	if err != nil {
		return nil, err
	}
	lit, ok := e.(*ast.Literal)
	if !ok || lit.Value.Kind() != types.KindInt {
		return nil, &Error{Msg: fmt.Sprintf("%s wants an integer, found %s", clause, e.String()), Pos: pos}
	}
	if lit.Value.AsInt() < 0 {
		return nil, &Error{Msg: fmt.Sprintf("%s must not be negative", clause), Pos: pos}
	}
	return lit, nil
}

// resultColumn parses one entry of the SELECT list.
func (p *Parser) resultColumn() (ast.ResultColumn, error) {
	if p.tok.Is("*") {
		p.advance()
		return ast.ResultColumn{Star: true}, nil
	}
	e, err := p.expr(ast.PrecLowest)
	if err != nil {
		return ast.ResultColumn{}, err
	}
	col := ast.ResultColumn{Expr: e}
	switch {
	case p.accept("AS"):
		alias, err := p.ident("a column alias")
		if err != nil {
			return ast.ResultColumn{}, err
		}
		col.Alias = alias
	case p.tok.Kind == lexer.Ident:
		col.Alias = p.tok.Text
		p.advance()
	}
	return col, nil
}

// insertStmt parses
//
//	INSERT INTO <table> [ ( <columns> ) ] VALUES ( <exprs> ) [, ( <exprs> )]…
func (p *Parser) insertStmt() (ast.Statement, error) {
	if err := p.expect("INSERT"); err != nil {
		return nil, err
	}
	if err := p.expect("INTO"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	ins := &ast.Insert{Table: table}

	if p.accept("(") {
		for {
			name, err := p.ident("a column name")
			if err != nil {
				return nil, err
			}
			ins.Columns = append(ins.Columns, name)
			if !p.accept(",") {
				break
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}

	if err := p.expect("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expect("("); err != nil {
			return nil, err
		}
		var row []ast.Expr
		for {
			e, err := p.expr(ast.PrecLowest)
			if err != nil {
				return nil, err
			}
			if !ast.IsConstant(e) {
				return nil, p.errorf("VALUES accepts constant expressions only, found %s", e.String())
			}
			row = append(row, e)
			if !p.accept(",") {
				break
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		if !p.accept(",") {
			break
		}
	}
	return ins, nil
}

// updateStmt parses `UPDATE <table> SET <assignments> [WHERE <expr>]`.
func (p *Parser) updateStmt() (ast.Statement, error) {
	if err := p.expect("UPDATE"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expect("SET"); err != nil {
		return nil, err
	}
	upd := &ast.Update{Table: table}
	for {
		name, err := p.ident("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expect("="); err != nil {
			return nil, err
		}
		value, err := p.expr(ast.PrecLowest)
		if err != nil {
			return nil, err
		}
		if ast.HasAggregate(value) {
			return nil, p.errorf("aggregate functions are not allowed in SET")
		}
		upd.Set = append(upd.Set, ast.Assignment{Column: name, Value: value})
		if !p.accept(",") {
			break
		}
	}
	if p.accept("WHERE") {
		if upd.Where, err = p.expr(ast.PrecLowest); err != nil {
			return nil, err
		}
	}
	return upd, nil
}

// deleteStmt parses `DELETE FROM <table> [WHERE <expr>]`.
func (p *Parser) deleteStmt() (ast.Statement, error) {
	if err := p.expect("DELETE"); err != nil {
		return nil, err
	}
	if err := p.expect("FROM"); err != nil {
		return nil, err
	}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	del := &ast.Delete{Table: table}
	if p.accept("WHERE") {
		if del.Where, err = p.expr(ast.PrecLowest); err != nil {
			return nil, err
		}
	}
	return del, nil
}

// createStmt parses CREATE TABLE and CREATE INDEX.
func (p *Parser) createStmt() (ast.Statement, error) {
	if err := p.expect("CREATE"); err != nil {
		return nil, err
	}
	switch {
	case p.accept("TABLE"):
		return p.createTableTail()
	case p.accept("INDEX"):
		return p.createIndexTail()
	}
	return nil, p.errorf("expected TABLE or INDEX after CREATE, found %s", p.tok.Describe())
}

func (p *Parser) createTableTail() (ast.Statement, error) {
	stmt := &ast.CreateTable{IfNotExists: p.acceptSeq("IF", "NOT", "EXISTS")}
	table, err := p.ident("a table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = table

	if err := p.expect("("); err != nil {
		return nil, err
	}
	for {
		def, err := p.columnDef()
		if err != nil {
			return nil, err
		}
		stmt.Columns = append(stmt.Columns, def)
		if !p.accept(",") {
			break
		}
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return stmt, nil
}

// columnDef parses `<name> <type> [PRIMARY KEY] [NOT NULL]`, in either
// order and with either constraint absent.
func (p *Parser) columnDef() (ast.ColumnDef, error) {
	name, err := p.ident("a column name")
	if err != nil {
		return ast.ColumnDef{}, err
	}
	if p.tok.Kind != lexer.Ident {
		return ast.ColumnDef{}, p.errorf("expected a column type for %q, found %s", name, p.tok.Describe())
	}
	typePos := p.tok.Pos
	typeName := p.tok.Text
	p.advance()
	kind, ok := types.ParseKind(typeName)
	if !ok {
		return ast.ColumnDef{}, &Error{
			Msg: fmt.Sprintf("unknown column type %q (want INT, FLOAT, TEXT or BOOL)", typeName),
			Pos: typePos,
		}
	}
	def := ast.ColumnDef{Name: name, Type: kind}
	for {
		switch {
		case p.acceptSeq("PRIMARY", "KEY"):
			def.PrimaryKey = true
			def.NotNull = true
		case p.acceptSeq("NOT", "NULL"):
			def.NotNull = true
		case p.tok.Is("PRIMARY"):
			p.advance()
			return ast.ColumnDef{}, p.errorf("expected KEY after PRIMARY, found %s", p.tok.Describe())
		default:
			return def, nil
		}
	}
}

func (p *Parser) createIndexTail() (ast.Statement, error) {
	stmt := &ast.CreateIndex{IfNotExists: p.acceptSeq("IF", "NOT", "EXISTS")}
	name, err := p.ident("an index name")
	if err != nil {
		return nil, err
	}
	stmt.Name = name
	if err := p.expect("ON"); err != nil {
		return nil, err
	}
	if stmt.Table, err = p.ident("a table name"); err != nil {
		return nil, err
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	if stmt.Column, err = p.ident("a column name"); err != nil {
		return nil, err
	}
	if p.tok.Is(",") {
		return nil, p.errorf("keelsql indexes one column; multi-column indexes are not supported")
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return stmt, nil
}

// dropStmt parses DROP TABLE and DROP INDEX.
func (p *Parser) dropStmt() (ast.Statement, error) {
	if err := p.expect("DROP"); err != nil {
		return nil, err
	}
	switch {
	case p.accept("TABLE"):
		ifExists := p.acceptSeq("IF", "EXISTS")
		name, err := p.ident("a table name")
		if err != nil {
			return nil, err
		}
		return &ast.DropTable{Table: name, IfExists: ifExists}, nil
	case p.accept("INDEX"):
		ifExists := p.acceptSeq("IF", "EXISTS")
		name, err := p.ident("an index name")
		if err != nil {
			return nil, err
		}
		return &ast.DropIndex{Name: name, IfExists: ifExists}, nil
	}
	return nil, p.errorf("expected TABLE or INDEX after DROP, found %s", p.tok.Describe())
}

// ---------------------------------------------------------------------
// expressions
// ---------------------------------------------------------------------

// expr is precedence climbing: parse a prefix expression, then keep
// absorbing operators whose precedence is at least min. Left-associativity
// falls out of parsing the right operand at min+1.
func (p *Parser) expr(min int) (ast.Expr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		// The postfix predicates all sit at comparison precedence.
		if ast.PrecCompare >= min {
			handled, e, err := p.postfix(left)
			if err != nil {
				return nil, err
			}
			if handled {
				left = e
				continue
			}
		}

		op := p.tok.Text
		if p.tok.Kind != lexer.Punct && p.tok.Kind != lexer.Keyword {
			return left, nil
		}
		prec := ast.BinaryPrecedence(op)
		if prec == ast.PrecLowest || prec < min {
			return left, nil
		}
		p.advance()
		right, err := p.expr(prec + 1)
		if err != nil {
			return nil, err
		}
		if op == "==" {
			op = "="
		}
		left = &ast.Binary{Op: op, Left: left, Right: right}
	}
}

// postfix handles IS [NOT] NULL, [NOT] BETWEEN, [NOT] IN and [NOT] LIKE.
func (p *Parser) postfix(left ast.Expr) (bool, ast.Expr, error) {
	// An expression followed by NOT can only be `NOT BETWEEN`, `NOT IN` or
	// `NOT LIKE`: a prefix NOT is handled by unary, before an expression
	// rather than after one. Anything else is a mistake worth naming.
	negate := false
	if p.tok.Is("NOT") {
		switch p.next.Text {
		case "BETWEEN", "IN", "LIKE":
			negate = true
			p.advance()
		default:
			p.advance()
			return false, nil, p.errorf("expected BETWEEN, IN or LIKE after NOT, found %s", p.tok.Describe())
		}
	}

	switch {
	case p.tok.Is("IS"):
		p.advance()
		not := p.accept("NOT")
		if err := p.expect("NULL"); err != nil {
			return false, nil, err
		}
		return true, &ast.IsNull{Expr: left, Not: not}, nil

	case p.tok.Is("BETWEEN"):
		p.advance()
		// The bounds are parsed above AND's precedence so that the AND
		// separating them is not mistaken for a conjunction.
		lo, err := p.expr(ast.PrecAdd)
		if err != nil {
			return false, nil, err
		}
		if err := p.expect("AND"); err != nil {
			return false, nil, err
		}
		hi, err := p.expr(ast.PrecAdd)
		if err != nil {
			return false, nil, err
		}
		return true, &ast.Between{Expr: left, Lo: lo, Hi: hi, Not: negate}, nil

	case p.tok.Is("IN"):
		p.advance()
		if err := p.expect("("); err != nil {
			return false, nil, err
		}
		if p.tok.Is("SELECT") {
			return false, nil, p.errorf("subqueries are not supported")
		}
		in := &ast.In{Expr: left, Not: negate}
		for {
			e, err := p.expr(ast.PrecLowest)
			if err != nil {
				return false, nil, err
			}
			in.List = append(in.List, e)
			if !p.accept(",") {
				break
			}
		}
		if err := p.expect(")"); err != nil {
			return false, nil, err
		}
		return true, in, nil

	case p.tok.Is("LIKE"):
		p.advance()
		pattern, err := p.expr(ast.PrecCompare + 1)
		if err != nil {
			return false, nil, err
		}
		return true, &ast.Like{Expr: left, Pattern: pattern, Not: negate}, nil
	}

	return false, nil, nil
}

// unary parses the prefix operators and then a primary expression.
func (p *Parser) unary() (ast.Expr, error) {
	switch {
	case p.tok.Is("NOT"):
		p.advance()
		// NOT binds looser than a comparison but tighter than AND.
		operand, err := p.expr(ast.PrecNot + 1)
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: "NOT", Expr: operand}, nil

	case p.tok.Is("-"), p.tok.Is("+"):
		op := p.tok.Text
		p.advance()
		operand, err := p.unary()
		if err != nil {
			return nil, err
		}
		// Fold the sign into a numeric literal so that the planner sees
		// `pk = -5` as a constant it can turn into a range bound.
		if lit, ok := operand.(*ast.Literal); ok && lit.Value.IsNumeric() {
			if op == "+" {
				return lit, nil
			}
			neg, err := types.Neg(lit.Value)
			if err != nil {
				return nil, p.errorf("%s", err)
			}
			return &ast.Literal{Value: neg}, nil
		}
		if op == "+" {
			return operand, nil
		}
		return &ast.Unary{Op: "-", Expr: operand}, nil
	}
	return p.primary()
}

// primary parses a literal, a parenthesised expression, a column reference
// or an aggregate call.
func (p *Parser) primary() (ast.Expr, error) {
	tok := p.tok
	switch tok.Kind {
	case lexer.Int:
		p.advance()
		n, err := strconv.ParseInt(tok.Text, 10, 64)
		if err != nil {
			return nil, &Error{Msg: fmt.Sprintf("integer literal %s is out of range", tok.Text), Pos: tok.Pos}
		}
		return &ast.Literal{Value: types.Int(n)}, nil

	case lexer.Float:
		p.advance()
		f, err := strconv.ParseFloat(tok.Text, 64)
		if err != nil {
			return nil, &Error{Msg: fmt.Sprintf("float literal %s is out of range", tok.Text), Pos: tok.Pos}
		}
		return &ast.Literal{Value: types.Float(f)}, nil

	case lexer.String:
		p.advance()
		return &ast.Literal{Value: types.Text(tok.Text)}, nil

	case lexer.Keyword:
		switch tok.Text {
		case "NULL":
			p.advance()
			return &ast.Literal{Value: types.Null()}, nil
		case "TRUE":
			p.advance()
			return &ast.Literal{Value: types.Bool(true)}, nil
		case "FALSE":
			p.advance()
			return &ast.Literal{Value: types.Bool(false)}, nil
		}
		return nil, p.errorf("expected an expression, found %s", tok.Describe())

	case lexer.Ident:
		if p.next.Is("(") {
			return p.funcCall()
		}
		p.advance()
		if p.tok.Is(".") {
			p.advance()
			name, err := p.ident("a column name")
			if err != nil {
				return nil, err
			}
			return &ast.Column{Table: tok.Text, Name: name}, nil
		}
		return &ast.Column{Name: tok.Text}, nil

	case lexer.Punct:
		if tok.Text == "(" {
			p.advance()
			e, err := p.expr(ast.PrecLowest)
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, p.errorf("expected an expression, found %s", tok.Describe())
}

// aggregates are the only functions keelsql has.
var aggregates = map[string]bool{"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true}

// IsAggregate reports whether name is one of the aggregate functions.
func IsAggregate(name string) bool { return aggregates[strings.ToUpper(name)] }

func (p *Parser) funcCall() (ast.Expr, error) {
	tok := p.tok
	name := strings.ToUpper(tok.Text)
	if !aggregates[name] {
		return nil, &Error{
			Msg: fmt.Sprintf("unknown function %q (keelsql has COUNT, SUM, AVG, MIN and MAX)", tok.Text),
			Pos: tok.Pos,
		}
	}
	p.advance()
	if err := p.expect("("); err != nil {
		return nil, err
	}
	call := &ast.FuncCall{Name: name}
	if p.tok.Is("*") {
		if name != "COUNT" {
			return nil, p.errorf("%s(*) is not allowed; only COUNT(*) is", name)
		}
		p.advance()
		call.Star = true
	} else {
		arg, err := p.expr(ast.PrecLowest)
		if err != nil {
			return nil, err
		}
		if ast.HasAggregate(arg) {
			return nil, p.errorf("aggregate functions cannot be nested")
		}
		call.Arg = arg
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return call, nil
}

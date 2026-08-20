// Package ast defines the syntax tree that the parser builds and the
// planner consumes.
//
// Every node knows how to print itself back as SQL, with the minimum number
// of parentheses. That is not decoration: EXPLAIN renders predicates by
// asking the tree to print itself, so the plan output and the query text
// stay in step automatically.
package ast

import (
	"strings"

	"github.com/aminyx/keelsql/types"
)

// A Node is any syntax tree node.
type Node interface {
	String() string
}

// A Statement is one complete SQL statement.
type Statement interface {
	Node
	stmtNode()
}

// An Expr is a scalar expression.
type Expr interface {
	Node
	exprNode()
}

// ---------------------------------------------------------------------
// expressions
// ---------------------------------------------------------------------

// A Literal is a constant: 1, 2.5, 'text', TRUE, NULL.
type Literal struct {
	Value types.Value
}

// A Column is a reference to a column, optionally qualified by a table
// name (t.a). keelsql has one table per query, so the qualifier is only
// checked, never used to resolve.
type Column struct {
	Table string
	Name  string
}

// A Star is the '*' of SELECT * and COUNT(*).
type Star struct{}

// A Unary applies a prefix operator: NOT, - or +.
type Unary struct {
	Op   string
	Expr Expr
}

// A Binary applies an infix operator: the arithmetic operators, the six
// comparisons, AND and OR.
type Binary struct {
	Op          string
	Left, Right Expr
}

// An IsNull is `expr IS NULL` or, with Not set, `expr IS NOT NULL`.
type IsNull struct {
	Expr Expr
	Not  bool
}

// A Between is `expr BETWEEN lo AND hi`, inclusive on both ends.
type Between struct {
	Expr   Expr
	Lo, Hi Expr
	Not    bool
}

// An In is `expr IN (a, b, c)`.
type In struct {
	Expr Expr
	List []Expr
	Not  bool
}

// A Like is `expr LIKE pattern`, where the pattern may use % and _.
type Like struct {
	Expr    Expr
	Pattern Expr
	Not     bool
}

// A FuncCall is an aggregate call: COUNT(*), SUM(a), AVG(a), MIN(a),
// MAX(a). keelsql has no scalar functions, so every call is an aggregate.
type FuncCall struct {
	Name string
	Arg  Expr // nil when Star is set
	Star bool
}

func (*Literal) exprNode()  {}
func (*Column) exprNode()   {}
func (*Star) exprNode()     {}
func (*Unary) exprNode()    {}
func (*Binary) exprNode()   {}
func (*IsNull) exprNode()   {}
func (*Between) exprNode()  {}
func (*In) exprNode()       {}
func (*Like) exprNode()     {}
func (*FuncCall) exprNode() {}

// ---------------------------------------------------------------------
// statements
// ---------------------------------------------------------------------

// A ColumnDef is one column of a CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       types.Kind
	NotNull    bool
	PrimaryKey bool
}

// A CreateTable statement.
type CreateTable struct {
	Table       string
	Columns     []ColumnDef
	IfNotExists bool
}

// A DropTable statement.
type DropTable struct {
	Table    string
	IfExists bool
}

// A CreateIndex statement. keelsql indexes exactly one column.
type CreateIndex struct {
	Name        string
	Table       string
	Column      string
	IfNotExists bool
}

// A DropIndex statement.
type DropIndex struct {
	Name     string
	IfExists bool
}

// An Insert statement. Columns may be empty, meaning every column in
// declaration order.
type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr
}

// A ResultColumn is one entry of a SELECT list.
type ResultColumn struct {
	Star  bool
	Expr  Expr
	Alias string
}

// An OrderTerm is one entry of ORDER BY.
type OrderTerm struct {
	Expr Expr
	Desc bool
}

// A Select statement.
type Select struct {
	Columns  []ResultColumn
	Table    string
	Where    Expr
	GroupBy  []Expr
	OrderBy  []OrderTerm
	Limit    Expr
	Offset   Expr
	Distinct bool
}

// An Assignment is one `column = expr` of an UPDATE.
type Assignment struct {
	Column string
	Value  Expr
}

// An Update statement.
type Update struct {
	Table string
	Set   []Assignment
	Where Expr
}

// A Delete statement.
type Delete struct {
	Table string
	Where Expr
}

// An Explain statement wraps the statement whose plan is wanted.
type Explain struct {
	Stmt Statement
}

// Begin starts an explicit transaction.
type Begin struct{}

// Commit ends the current transaction, applying its writes.
type Commit struct{}

// Rollback ends the current transaction, discarding its writes.
type Rollback struct{}

func (*CreateTable) stmtNode() {}
func (*DropTable) stmtNode()   {}
func (*CreateIndex) stmtNode() {}
func (*DropIndex) stmtNode()   {}
func (*Insert) stmtNode()      {}
func (*Select) stmtNode()      {}
func (*Update) stmtNode()      {}
func (*Delete) stmtNode()      {}
func (*Explain) stmtNode()     {}
func (*Begin) stmtNode()       {}
func (*Commit) stmtNode()      {}
func (*Rollback) stmtNode()    {}

// ---------------------------------------------------------------------
// traversal
// ---------------------------------------------------------------------

// Inspect calls fn for every node in the expression tree, depth first. If
// fn returns false the node's children are skipped.
func Inspect(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch n := e.(type) {
	case *Unary:
		Inspect(n.Expr, fn)
	case *Binary:
		Inspect(n.Left, fn)
		Inspect(n.Right, fn)
	case *IsNull:
		Inspect(n.Expr, fn)
	case *Between:
		Inspect(n.Expr, fn)
		Inspect(n.Lo, fn)
		Inspect(n.Hi, fn)
	case *In:
		Inspect(n.Expr, fn)
		for _, item := range n.List {
			Inspect(item, fn)
		}
	case *Like:
		Inspect(n.Expr, fn)
		Inspect(n.Pattern, fn)
	case *FuncCall:
		Inspect(n.Arg, fn)
	}
}

// Columns returns the names of every column referenced by e, in first-seen
// order and without duplicates.
func Columns(e Expr) []string {
	var out []string
	seen := map[string]bool{}
	Inspect(e, func(n Expr) bool {
		if c, ok := n.(*Column); ok && !seen[c.Name] {
			seen[c.Name] = true
			out = append(out, c.Name)
		}
		return true
	})
	return out
}

// HasAggregate reports whether e contains an aggregate call.
func HasAggregate(e Expr) bool {
	found := false
	Inspect(e, func(n Expr) bool {
		if _, ok := n.(*FuncCall); ok {
			found = true
		}
		return !found
	})
	return found
}

// IsConstant reports whether e can be evaluated without a row.
func IsConstant(e Expr) bool {
	constant := true
	Inspect(e, func(n Expr) bool {
		switch n.(type) {
		case *Column, *Star, *FuncCall:
			constant = false
		}
		return constant
	})
	return constant
}

// quoteIdent renders an identifier, adding quotes only when the spelling
// needs them.
func quoteIdent(name string) string {
	if name == "" {
		return `""`
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9' && i > 0)
		if !ok {
			return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		}
	}
	return name
}

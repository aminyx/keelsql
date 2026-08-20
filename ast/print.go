package ast

import (
	"fmt"
	"strings"
)

// Operator precedences, lowest binding first. They drive both the parser's
// climbing loop and the printer's decision about parentheses, so the two
// cannot drift apart.
const (
	PrecLowest  = 0
	PrecOr      = 1
	PrecAnd     = 2
	PrecNot     = 3
	PrecCompare = 4 // = != < <= > >=, IS NULL, BETWEEN, IN, LIKE
	PrecAdd     = 5 // + -
	PrecMul     = 6 // * / %
	PrecUnary   = 7 // -x
	PrecAtom    = 8
)

// Precedence returns the binding strength of an expression's top operator.
// An atom binds tightest and therefore never needs parentheses.
func Precedence(e Expr) int {
	switch n := e.(type) {
	case *Binary:
		return BinaryPrecedence(n.Op)
	case *Unary:
		if n.Op == "NOT" {
			return PrecNot
		}
		return PrecUnary
	case *IsNull, *Between, *In, *Like:
		return PrecCompare
	}
	return PrecAtom
}

// BinaryPrecedence returns the precedence of an infix operator, or
// PrecLowest if the token is not one.
func BinaryPrecedence(op string) int {
	switch op {
	case "OR":
		return PrecOr
	case "AND":
		return PrecAnd
	case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
		return PrecCompare
	case "+", "-":
		return PrecAdd
	case "*", "/", "%":
		return PrecMul
	}
	return PrecLowest
}

// sub prints a child expression, parenthesising it only when its operator
// binds more loosely than the context it is being placed in.
func sub(e Expr, parent int) string {
	if e == nil {
		return "NULL"
	}
	if Precedence(e) < parent {
		return "(" + e.String() + ")"
	}
	return e.String()
}

// String renders the literal as SQL.
func (n *Literal) String() string { return n.Value.SQL() }

// String renders the column reference, qualified if the query qualified it.
func (n *Column) String() string {
	if n.Table != "" {
		return quoteIdent(n.Table) + "." + quoteIdent(n.Name)
	}
	return quoteIdent(n.Name)
}

// String renders '*'.
func (n *Star) String() string { return "*" }

// String renders the prefix operator and its operand.
func (n *Unary) String() string {
	if n.Op == "NOT" {
		return "NOT " + sub(n.Expr, PrecNot)
	}
	return n.Op + sub(n.Expr, PrecUnary)
}

// String renders the infix operator. The right operand is printed one level
// tighter so that a - (b - c) keeps its parentheses.
func (n *Binary) String() string {
	p := BinaryPrecedence(n.Op)
	return sub(n.Left, p) + " " + n.Op + " " + sub(n.Right, p+1)
}

// String renders IS NULL / IS NOT NULL.
func (n *IsNull) String() string {
	if n.Not {
		return sub(n.Expr, PrecCompare) + " IS NOT NULL"
	}
	return sub(n.Expr, PrecCompare) + " IS NULL"
}

// String renders BETWEEN.
func (n *Between) String() string {
	not := ""
	if n.Not {
		not = "NOT "
	}
	return fmt.Sprintf("%s %sBETWEEN %s AND %s",
		sub(n.Expr, PrecCompare), not, sub(n.Lo, PrecAdd), sub(n.Hi, PrecAdd))
}

// String renders IN.
func (n *In) String() string {
	items := make([]string, len(n.List))
	for i, item := range n.List {
		items[i] = item.String()
	}
	not := ""
	if n.Not {
		not = "NOT "
	}
	return fmt.Sprintf("%s %sIN (%s)", sub(n.Expr, PrecCompare), not, strings.Join(items, ", "))
}

// String renders LIKE.
func (n *Like) String() string {
	not := ""
	if n.Not {
		not = "NOT "
	}
	return fmt.Sprintf("%s %sLIKE %s", sub(n.Expr, PrecCompare), not, sub(n.Pattern, PrecCompare))
}

// String renders an aggregate call.
func (n *FuncCall) String() string {
	if n.Star {
		return n.Name + "(*)"
	}
	return n.Name + "(" + n.Arg.String() + ")"
}

// String renders the result column, alias included.
func (c ResultColumn) String() string {
	s := "*"
	if !c.Star {
		s = c.Expr.String()
	}
	if c.Alias != "" {
		s += " AS " + quoteIdent(c.Alias)
	}
	return s
}

// String renders one ORDER BY term.
func (t OrderTerm) String() string {
	if t.Desc {
		return t.Expr.String() + " DESC"
	}
	return t.Expr.String() + " ASC"
}

// String renders the column definition.
func (d ColumnDef) String() string {
	s := quoteIdent(d.Name) + " " + d.Type.String()
	if d.PrimaryKey {
		s += " PRIMARY KEY"
	}
	if d.NotNull && !d.PrimaryKey {
		s += " NOT NULL"
	}
	return s
}

// String renders the CREATE TABLE statement.
func (n *CreateTable) String() string {
	cols := make([]string, len(n.Columns))
	for i, c := range n.Columns {
		cols[i] = c.String()
	}
	ine := ""
	if n.IfNotExists {
		ine = "IF NOT EXISTS "
	}
	return fmt.Sprintf("CREATE TABLE %s%s (%s)", ine, quoteIdent(n.Table), strings.Join(cols, ", "))
}

// String renders the DROP TABLE statement.
func (n *DropTable) String() string {
	if n.IfExists {
		return "DROP TABLE IF EXISTS " + quoteIdent(n.Table)
	}
	return "DROP TABLE " + quoteIdent(n.Table)
}

// String renders the CREATE INDEX statement.
func (n *CreateIndex) String() string {
	ine := ""
	if n.IfNotExists {
		ine = "IF NOT EXISTS "
	}
	return fmt.Sprintf("CREATE INDEX %s%s ON %s (%s)",
		ine, quoteIdent(n.Name), quoteIdent(n.Table), quoteIdent(n.Column))
}

// String renders the DROP INDEX statement.
func (n *DropIndex) String() string {
	if n.IfExists {
		return "DROP INDEX IF EXISTS " + quoteIdent(n.Name)
	}
	return "DROP INDEX " + quoteIdent(n.Name)
}

// String renders the INSERT statement.
func (n *Insert) String() string {
	var sb strings.Builder
	sb.WriteString("INSERT INTO " + quoteIdent(n.Table))
	if len(n.Columns) > 0 {
		names := make([]string, len(n.Columns))
		for i, c := range n.Columns {
			names[i] = quoteIdent(c)
		}
		sb.WriteString(" (" + strings.Join(names, ", ") + ")")
	}
	sb.WriteString(" VALUES ")
	rows := make([]string, len(n.Rows))
	for i, row := range n.Rows {
		vals := make([]string, len(row))
		for j, v := range row {
			vals[j] = v.String()
		}
		rows[i] = "(" + strings.Join(vals, ", ") + ")"
	}
	sb.WriteString(strings.Join(rows, ", "))
	return sb.String()
}

// String renders the SELECT statement.
func (n *Select) String() string {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	if n.Distinct {
		sb.WriteString("DISTINCT ")
	}
	cols := make([]string, len(n.Columns))
	for i, c := range n.Columns {
		cols[i] = c.String()
	}
	sb.WriteString(strings.Join(cols, ", "))
	sb.WriteString(" FROM " + quoteIdent(n.Table))
	if n.Where != nil {
		sb.WriteString(" WHERE " + n.Where.String())
	}
	if len(n.GroupBy) > 0 {
		terms := make([]string, len(n.GroupBy))
		for i, g := range n.GroupBy {
			terms[i] = g.String()
		}
		sb.WriteString(" GROUP BY " + strings.Join(terms, ", "))
	}
	if len(n.OrderBy) > 0 {
		terms := make([]string, len(n.OrderBy))
		for i, o := range n.OrderBy {
			terms[i] = o.String()
		}
		sb.WriteString(" ORDER BY " + strings.Join(terms, ", "))
	}
	if n.Limit != nil {
		sb.WriteString(" LIMIT " + n.Limit.String())
	}
	if n.Offset != nil {
		sb.WriteString(" OFFSET " + n.Offset.String())
	}
	return sb.String()
}

// String renders the UPDATE statement.
func (n *Update) String() string {
	sets := make([]string, len(n.Set))
	for i, a := range n.Set {
		sets[i] = quoteIdent(a.Column) + " = " + a.Value.String()
	}
	s := "UPDATE " + quoteIdent(n.Table) + " SET " + strings.Join(sets, ", ")
	if n.Where != nil {
		s += " WHERE " + n.Where.String()
	}
	return s
}

// String renders the DELETE statement.
func (n *Delete) String() string {
	s := "DELETE FROM " + quoteIdent(n.Table)
	if n.Where != nil {
		s += " WHERE " + n.Where.String()
	}
	return s
}

// String renders the EXPLAIN statement.
func (n *Explain) String() string { return "EXPLAIN " + n.Stmt.String() }

// String renders BEGIN.
func (n *Begin) String() string { return "BEGIN" }

// String renders COMMIT.
func (n *Commit) String() string { return "COMMIT" }

// String renders ROLLBACK.
func (n *Rollback) String() string { return "ROLLBACK" }

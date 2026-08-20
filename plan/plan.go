// Package plan turns a syntax tree into a physical plan.
//
// The pipeline is the textbook one, kept deliberately small:
//
//	AST  ->  logical plan  ->  rewrite rules  ->  physical plan
//
// The logical plan is the shape SQL implies — scan the table, filter it,
// group it, project it, sort it, cut it off. The rules then rewrite that
// shape into something cheaper to run:
//
//   - predicate pushdown, which turns a WHERE clause on the primary key
//     into bounds on the scan itself, so the executor reads a byte range of
//     keelstore instead of the whole table;
//   - index selection, which does the same through a secondary index;
//   - projection pruning, which tells the scan which columns are actually
//     read so the row decoder can skip the rest;
//   - limit pushdown, which turns "sort everything, then take five" into a
//     bounded top-N heap;
//   - sort elimination, which drops the sort entirely when the chosen scan
//     already produces the requested order.
//
// EXPLAIN prints the result of all of that, which is the only honest way to
// show that the rules fired.
package plan

import (
	"fmt"
	"strings"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/catalog"
)

// A Plan is one node of the physical plan tree.
type Plan interface {
	// Line is the node's own one-line description, without its children.
	Line() string
	// Children returns the node's inputs, in execution order.
	Children() []Plan
}

// An Env maps expression fragments onto positions in the rows an operator
// receives. Below an aggregation, Columns holds every table column; above
// one, Columns holds only the GROUP BY columns and Exprs holds the
// aggregate calls, which is exactly the rule that makes
// `SELECT b, COUNT(*) FROM t GROUP BY b` legal and
// `SELECT c, COUNT(*) FROM t GROUP BY b` not.
type Env struct {
	Columns map[string]int
	Exprs   map[string]int
}

// NewEnv returns an empty environment.
func NewEnv() *Env {
	return &Env{Columns: map[string]int{}, Exprs: map[string]int{}}
}

// TableEnv binds every column of a table to its position in a stored row.
func TableEnv(t *catalog.Table) *Env {
	env := NewEnv()
	for i, c := range t.Columns {
		env.Columns[c.Name] = i
	}
	return env
}

// A SortKey is one ORDER BY term.
type SortKey struct {
	Expr ast.Expr
	Desc bool
}

// String renders the key as ORDER BY spells it.
func (k SortKey) String() string {
	if k.Desc {
		return k.Expr.String() + " DESC"
	}
	return k.Expr.String() + " ASC"
}

// An AggCall is one aggregate in the SELECT list.
type AggCall struct {
	Func string   // COUNT, SUM, AVG, MIN or MAX
	Arg  ast.Expr // nil for COUNT(*)
	Text string   // the call as written, used as the binding key
}

// ---------------------------------------------------------------------
// scans
// ---------------------------------------------------------------------

// A SeqScan reads every row of a table in primary-key order.
type SeqScan struct {
	Table *catalog.Table
	Mask  []bool // which columns to decode
}

// A RangeScan reads a contiguous slice of a table's primary-key space. It
// is what a predicate on the primary key is rewritten into.
type RangeScan struct {
	Table  *catalog.Table
	Bounds Range
	Mask   []bool
}

// An IndexScan walks a secondary index over a range of the indexed column
// and fetches each matching row by primary key.
type IndexScan struct {
	Table  *catalog.Table
	Index  *catalog.Index
	Bounds Range
	Mask   []bool
}

// A Filter keeps the rows whose predicate evaluates to TRUE. Rows that
// evaluate to FALSE and rows that evaluate to UNKNOWN are both dropped.
type Filter struct {
	Input Plan
	Pred  ast.Expr
	Env   *Env
}

// A Project evaluates the SELECT list.
type Project struct {
	Input Plan
	Exprs []ast.Expr
	Names []string
	Env   *Env
}

// An Aggregate groups its input and applies the aggregate calls. With no
// GROUP BY there is exactly one group, which is why
// `SELECT COUNT(*) FROM empty_table` returns 0 rather than nothing.
//
// Output rows are the GROUP BY values followed by the call results, and
// groups come out in ascending group-key order because the group key is the
// order-preserving encoding of its values.
type Aggregate struct {
	Input   Plan
	GroupBy []ast.Expr
	Calls   []AggCall
	Env     *Env // the environment the input rows are read in
	Out     *Env // the environment the output rows expose
}

// A Sort orders its input. Limit, when positive, makes it a top-N sort: it
// keeps a bounded heap instead of the whole input.
type Sort struct {
	Input Plan
	Keys  []SortKey
	Limit int64 // -1 for an unbounded sort
	Env   *Env
}

// A Limit passes through at most Count rows after skipping Offset of them.
// Count is -1 when only an offset was given.
type Limit struct {
	Input  Plan
	Count  int64
	Offset int64
}

// A Distinct drops duplicate output rows, comparing them by their
// order-preserving encoding.
type Distinct struct {
	Input Plan
}

// ---------------------------------------------------------------------
// statements
// ---------------------------------------------------------------------

// An Insert writes literal rows.
type Insert struct {
	Table *catalog.Table
	Rows  [][]ast.Expr
	Cols  []int // position in the table for each expression
}

// An Update rewrites the rows produced by Input.
type Update struct {
	Table *catalog.Table
	Input Plan
	Set   []Assignment
	Env   *Env
}

// An Assignment is one `column = expr` of an UPDATE, resolved to a column
// position.
type Assignment struct {
	Column int
	Name   string
	Value  ast.Expr
}

// A Delete removes the rows produced by Input.
type Delete struct {
	Table *catalog.Table
	Input Plan
}

// A CreateTable statement plan.
type CreateTable struct {
	Def         *catalog.Table
	IfNotExists bool
}

// A DropTable statement plan.
type DropTable struct {
	Name     string
	IfExists bool
}

// A CreateIndex statement plan.
type CreateIndex struct {
	Table       *catalog.Table
	Name        string
	Column      int
	IfNotExists bool
}

// A DropIndex statement plan.
type DropIndex struct {
	Name     string
	IfExists bool
}

// A Transaction statement plan: BEGIN, COMMIT or ROLLBACK.
type Transaction struct {
	Op string
}

// ---------------------------------------------------------------------
// Line and Children
// ---------------------------------------------------------------------

func maskNames(t *catalog.Table, mask []bool) string {
	var names []string
	for i, c := range t.Columns {
		if mask == nil || mask[i] {
			names = append(names, c.Name)
		}
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// Line describes the sequential scan.
func (n *SeqScan) Line() string {
	return fmt.Sprintf("SeqScan table=%s columns=%s", n.Table.Name, maskNames(n.Table, n.Mask))
}

// Line describes the primary-key range scan.
func (n *RangeScan) Line() string {
	return fmt.Sprintf("RangeScan table=%s pk=%s range=%s columns=%s",
		n.Table.Name, n.Table.PKColumn().Name, n.Bounds, maskNames(n.Table, n.Mask))
}

// Line describes the index scan.
func (n *IndexScan) Line() string {
	return fmt.Sprintf("IndexScan table=%s index=%s on=%s range=%s columns=%s",
		n.Table.Name, n.Index.Name, n.Index.Column, n.Bounds, maskNames(n.Table, n.Mask))
}

// Line describes the filter.
func (n *Filter) Line() string { return "Filter predicate=" + n.Pred.String() }

// Line describes the projection.
func (n *Project) Line() string {
	parts := make([]string, len(n.Exprs))
	for i, e := range n.Exprs {
		parts[i] = e.String()
		if n.Names[i] != e.String() {
			parts[i] += " AS " + n.Names[i]
		}
	}
	return "Project columns=[" + strings.Join(parts, ", ") + "]"
}

// Line describes the aggregation.
func (n *Aggregate) Line() string {
	group := "none"
	if len(n.GroupBy) > 0 {
		parts := make([]string, len(n.GroupBy))
		for i, e := range n.GroupBy {
			parts[i] = e.String()
		}
		group = "[" + strings.Join(parts, ", ") + "]"
	}
	calls := make([]string, len(n.Calls))
	for i, c := range n.Calls {
		calls[i] = c.Text
	}
	return fmt.Sprintf("HashAggregate group=%s aggregates=[%s]", group, strings.Join(calls, ", "))
}

// Line describes the sort, including whether the limit turned it into a
// bounded top-N.
func (n *Sort) Line() string {
	keys := make([]string, len(n.Keys))
	for i, k := range n.Keys {
		keys[i] = k.String()
	}
	if n.Limit >= 0 {
		return fmt.Sprintf("Sort keys=[%s] mode=top-%d", strings.Join(keys, ", "), n.Limit)
	}
	return fmt.Sprintf("Sort keys=[%s] mode=full", strings.Join(keys, ", "))
}

// Line describes the limit.
func (n *Limit) Line() string {
	count := "all"
	if n.Count >= 0 {
		count = fmt.Sprint(n.Count)
	}
	return fmt.Sprintf("Limit count=%s offset=%d", count, n.Offset)
}

// Line describes the duplicate removal.
func (n *Distinct) Line() string { return "Distinct" }

// Line describes the insert.
func (n *Insert) Line() string {
	return fmt.Sprintf("Insert table=%s rows=%d", n.Table.Name, len(n.Rows))
}

// Line describes the update.
func (n *Update) Line() string {
	sets := make([]string, len(n.Set))
	for i, a := range n.Set {
		sets[i] = a.Name + " = " + a.Value.String()
	}
	return fmt.Sprintf("Update table=%s set=[%s]", n.Table.Name, strings.Join(sets, ", "))
}

// Line describes the delete.
func (n *Delete) Line() string { return "Delete table=" + n.Table.Name }

// Line describes CREATE TABLE.
func (n *CreateTable) Line() string { return "CreateTable table=" + n.Def.Name }

// Line describes DROP TABLE.
func (n *DropTable) Line() string { return "DropTable table=" + n.Name }

// Line describes CREATE INDEX.
func (n *CreateIndex) Line() string {
	return fmt.Sprintf("CreateIndex index=%s table=%s on=%s",
		n.Name, n.Table.Name, n.Table.Columns[n.Column].Name)
}

// Line describes DROP INDEX.
func (n *DropIndex) Line() string { return "DropIndex index=" + n.Name }

// Line describes the transaction control statement.
func (n *Transaction) Line() string { return n.Op }

// Children returns no inputs.
func (n *SeqScan) Children() []Plan { return nil }

// Children returns no inputs.
func (n *RangeScan) Children() []Plan { return nil }

// Children returns no inputs.
func (n *IndexScan) Children() []Plan { return nil }

// Children returns the filtered input.
func (n *Filter) Children() []Plan { return []Plan{n.Input} }

// Children returns the projected input.
func (n *Project) Children() []Plan { return []Plan{n.Input} }

// Children returns the aggregated input.
func (n *Aggregate) Children() []Plan { return []Plan{n.Input} }

// Children returns the sorted input.
func (n *Sort) Children() []Plan { return []Plan{n.Input} }

// Children returns the limited input.
func (n *Limit) Children() []Plan { return []Plan{n.Input} }

// Children returns the deduplicated input.
func (n *Distinct) Children() []Plan { return []Plan{n.Input} }

// Children returns no inputs.
func (n *Insert) Children() []Plan { return nil }

// Children returns the scan that finds the rows to update.
func (n *Update) Children() []Plan { return []Plan{n.Input} }

// Children returns the scan that finds the rows to delete.
func (n *Delete) Children() []Plan { return []Plan{n.Input} }

// Children returns no inputs.
func (n *CreateTable) Children() []Plan { return nil }

// Children returns no inputs.
func (n *DropTable) Children() []Plan { return nil }

// Children returns no inputs.
func (n *CreateIndex) Children() []Plan { return nil }

// Children returns no inputs.
func (n *DropIndex) Children() []Plan { return nil }

// Children returns no inputs.
func (n *Transaction) Children() []Plan { return nil }

// Explain renders a plan as an indented tree, the way EXPLAIN prints it:
//
//	Limit count=3 offset=0
//	  -> Project columns=[a, b]
//	    -> IndexScan table=t index=idx_b on=b range=[10, 20] columns=[a, b]
//
// The output is plain ASCII on purpose: it has to be readable in a Windows
// console and stable enough to assert on in a test.
func Explain(p Plan) string {
	var sb strings.Builder
	explainInto(&sb, p, 0)
	return strings.TrimRight(sb.String(), "\n")
}

func explainInto(sb *strings.Builder, p Plan, depth int) {
	if depth == 0 {
		sb.WriteString(p.Line())
	} else {
		sb.WriteString(strings.Repeat("  ", depth))
		sb.WriteString("-> ")
		sb.WriteString(p.Line())
	}
	sb.WriteByte('\n')
	for _, child := range p.Children() {
		explainInto(sb, child, depth+1)
	}
}

// ScanTable returns the table a scan node reads, and whether the node is a
// scan at all.
func ScanTable(p Plan) (*catalog.Table, bool) {
	switch n := p.(type) {
	case *SeqScan:
		return n.Table, true
	case *RangeScan:
		return n.Table, true
	case *IndexScan:
		return n.Table, true
	}
	return nil, false
}

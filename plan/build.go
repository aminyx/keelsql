package plan

import (
	"errors"
	"fmt"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/types"
)

// ErrUnsupported reports a statement keelsql parses but cannot plan.
var ErrUnsupported = errors.New("keelsql: unsupported")

// estimatedRows is the row count the planner assumes for every table.
// keelsql keeps no statistics, so this number is a placeholder whose only
// job is to make a full scan look expensive next to a bounded one. It is
// the "lite" in cost-lite: the plan choice depends on the *ordering* of the
// costs below, never on their absolute values.
const estimatedRows = 1000.0

// Cost model constants. A primary-key range scan reads matching rows
// directly; a secondary index scan reads an index entry and then fetches
// the row by key, so it pays roughly twice per row plus a fixed cost for
// the extra structure.
const (
	costRowRead   = 1.0
	costIndexRead = 1.0
	costRowFetch  = 1.0
	costStartup   = 1.0
)

// A Planner turns statements into plans against a catalog.
type Planner struct {
	cat *catalog.Catalog
}

// New returns a planner reading schemas from cat.
func New(cat *catalog.Catalog) *Planner { return &Planner{cat: cat} }

// Plan builds the physical plan for a statement.
func (p *Planner) Plan(stmt ast.Statement) (Plan, error) {
	switch s := stmt.(type) {
	case *ast.Explain:
		return p.Plan(s.Stmt)
	case *ast.CreateTable:
		return p.createTable(s)
	case *ast.DropTable:
		return &DropTable{Name: s.Table, IfExists: s.IfExists}, nil
	case *ast.CreateIndex:
		return p.createIndex(s)
	case *ast.DropIndex:
		return &DropIndex{Name: s.Name, IfExists: s.IfExists}, nil
	case *ast.Insert:
		return p.insert(s)
	case *ast.Select:
		return p.selectStmt(s)
	case *ast.Update:
		return p.update(s)
	case *ast.Delete:
		return p.delete(s)
	case *ast.Begin:
		return &Transaction{Op: "BEGIN"}, nil
	case *ast.Commit:
		return &Transaction{Op: "COMMIT"}, nil
	case *ast.Rollback:
		return &Transaction{Op: "ROLLBACK"}, nil
	}
	return nil, fmt.Errorf("%w statement %T", ErrUnsupported, stmt)
}

// ---------------------------------------------------------------------
// DDL
// ---------------------------------------------------------------------

func (p *Planner) createTable(s *ast.CreateTable) (Plan, error) {
	cols := make([]catalog.Column, len(s.Columns))
	for i, c := range s.Columns {
		cols[i] = catalog.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull, PrimaryKey: c.PrimaryKey}
	}
	def, err := catalog.Define(s.Table, cols)
	if err != nil {
		return nil, err
	}
	return &CreateTable{Def: def, IfNotExists: s.IfNotExists}, nil
}

func (p *Planner) createIndex(s *ast.CreateIndex) (Plan, error) {
	t, err := p.cat.Get(s.Table)
	if err != nil {
		return nil, err
	}
	pos, ok := t.ColumnIndex(s.Column)
	if !ok {
		return nil, fmt.Errorf("%w: %s.%s", catalog.ErrColumnNotFound, s.Table, s.Column)
	}
	return &CreateIndex{Table: t, Name: s.Name, Column: pos, IfNotExists: s.IfNotExists}, nil
}

// ---------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------

func (p *Planner) insert(s *ast.Insert) (Plan, error) {
	t, err := p.cat.Get(s.Table)
	if err != nil {
		return nil, err
	}
	targets := make([]int, 0, len(t.Columns))
	if len(s.Columns) == 0 {
		for i := range t.Columns {
			targets = append(targets, i)
		}
	} else {
		seen := map[string]bool{}
		for _, name := range s.Columns {
			if seen[name] {
				return nil, fmt.Errorf("keelsql: column %q listed twice in INSERT", name)
			}
			seen[name] = true
			pos, ok := t.ColumnIndex(name)
			if !ok {
				return nil, fmt.Errorf("%w: %s.%s", catalog.ErrColumnNotFound, t.Name, name)
			}
			targets = append(targets, pos)
		}
	}
	for i, row := range s.Rows {
		if len(row) != len(targets) {
			return nil, fmt.Errorf("keelsql: row %d has %d values, table %s wants %d",
				i+1, len(row), t.Name, len(targets))
		}
	}
	return &Insert{Table: t, Rows: s.Rows, Cols: targets}, nil
}

// ---------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------

func (p *Planner) selectStmt(s *ast.Select) (Plan, error) {
	t, err := p.cat.Get(s.Table)
	if err != nil {
		return nil, err
	}
	tableEnv := TableEnv(t)

	if s.Where != nil {
		if err := checkTableRefs(t, s.Where); err != nil {
			return nil, err
		}
	}
	conjuncts := splitConjuncts(s.Where)

	grouped := len(s.GroupBy) > 0 || selectHasAggregate(s)
	if grouped {
		return p.groupedSelect(s, t, tableEnv, conjuncts)
	}
	return p.plainSelect(s, t, tableEnv, conjuncts)
}

// plainSelect plans a SELECT without aggregation:
//
//	scan -> Filter -> Sort -> Project -> Distinct -> Limit
func (p *Planner) plainSelect(s *ast.Select, t *catalog.Table, env *Env, conjuncts []ast.Expr) (Plan, error) {
	// Expand the SELECT list.
	var exprs []ast.Expr
	var names []string
	for _, col := range s.Columns {
		if col.Star {
			for _, name := range t.ColumnNames() {
				exprs = append(exprs, &ast.Column{Name: name})
				names = append(names, name)
			}
			continue
		}
		if err := checkTableRefs(t, col.Expr); err != nil {
			return nil, err
		}
		exprs = append(exprs, col.Expr)
		names = append(names, resultName(col))
	}

	keys := make([]SortKey, len(s.OrderBy))
	for i, term := range s.OrderBy {
		if ast.HasAggregate(term.Expr) {
			return nil, fmt.Errorf("keelsql: ORDER BY uses an aggregate but the query has no GROUP BY")
		}
		if err := checkTableRefs(t, term.Expr); err != nil {
			return nil, err
		}
		keys[i] = SortKey{Expr: term.Expr, Desc: term.Desc}
	}

	count, offset, err := limitValues(s)
	if err != nil {
		return nil, err
	}

	// Access path selection and predicate pushdown.
	path := p.choosePath(t, conjuncts)
	residual := path.residual(conjuncts)

	// Sort elimination: a scan already returns rows ordered by whatever it
	// walks — the primary key for a table scan, the indexed column for an
	// index scan — so an ORDER BY that asks for exactly that is free. It is
	// decided before the columns are chosen, so that a sort key the query
	// no longer has to evaluate does not keep a column alive.
	if len(keys) == 1 && !keys[0].Desc && path.provides(keys[0].Expr) {
		keys = nil
	}

	// Projection pruning: the scan only has to decode the columns that the
	// residual filter, the SELECT list and the ORDER BY actually read.
	mask := newMask(t)
	markColumns(mask, t, residual)
	markColumns(mask, t, exprs)
	for _, k := range keys {
		markColumns(mask, t, []ast.Expr{k.Expr})
	}

	node := path.build(mask)
	if len(residual) > 0 {
		node = &Filter{Input: node, Pred: joinConjuncts(residual), Env: env}
	}

	if len(keys) > 0 {
		sort := &Sort{Input: node, Keys: keys, Limit: -1, Env: env}
		// Limit pushdown: a top-N sort keeps a bounded heap instead of the
		// whole input. DISTINCT blocks it, because deduplication happens
		// after the sort and can shrink the row count.
		if count >= 0 && !s.Distinct {
			sort.Limit = count + offset
		}
		node = sort
	}

	node = &Project{Input: node, Exprs: exprs, Names: names, Env: env}
	if s.Distinct {
		node = &Distinct{Input: node}
	}
	if count >= 0 || offset > 0 {
		node = &Limit{Input: node, Count: count, Offset: offset}
	}
	return node, nil
}

// groupedSelect plans a SELECT with aggregation:
//
//	scan -> Filter -> HashAggregate -> Sort -> Project -> Distinct -> Limit
func (p *Planner) groupedSelect(s *ast.Select, t *catalog.Table, env *Env, conjuncts []ast.Expr) (Plan, error) {
	for _, col := range s.Columns {
		if col.Star {
			return nil, fmt.Errorf("keelsql: SELECT * cannot be combined with aggregation")
		}
	}

	// The output environment: GROUP BY terms first, then the aggregate
	// calls, in the order they are first seen.
	out := NewEnv()
	for i, g := range s.GroupBy {
		if ast.HasAggregate(g) {
			return nil, fmt.Errorf("keelsql: GROUP BY cannot contain an aggregate")
		}
		if err := checkTableRefs(t, g); err != nil {
			return nil, err
		}
		out.Exprs[g.String()] = i
		if c, ok := g.(*ast.Column); ok {
			out.Columns[c.Name] = i
		}
	}

	var calls []AggCall
	collect := func(e ast.Expr) error {
		var bad error
		ast.Inspect(e, func(n ast.Expr) bool {
			call, ok := n.(*ast.FuncCall)
			if !ok {
				return true
			}
			if call.Arg != nil {
				if err := checkTableRefs(t, call.Arg); err != nil && bad == nil {
					bad = err
				}
			}
			text := call.String()
			if _, seen := out.Exprs[text]; !seen {
				out.Exprs[text] = len(s.GroupBy) + len(calls)
				calls = append(calls, AggCall{Func: call.Name, Arg: call.Arg, Text: text})
			}
			return false
		})
		return bad
	}
	for _, col := range s.Columns {
		if err := collect(col.Expr); err != nil {
			return nil, err
		}
	}
	for _, term := range s.OrderBy {
		if err := collect(term.Expr); err != nil {
			return nil, err
		}
	}

	// Validate the SELECT list and ORDER BY against the output
	// environment: a bare column is only legal if it is grouped by.
	var exprs []ast.Expr
	var names []string
	for _, col := range s.Columns {
		if err := checkGrouped(col.Expr, out); err != nil {
			return nil, err
		}
		exprs = append(exprs, col.Expr)
		names = append(names, resultName(col))
	}
	keys := make([]SortKey, len(s.OrderBy))
	for i, term := range s.OrderBy {
		if err := checkGrouped(term.Expr, out); err != nil {
			return nil, err
		}
		keys[i] = SortKey{Expr: term.Expr, Desc: term.Desc}
	}

	count, offset, err := limitValues(s)
	if err != nil {
		return nil, err
	}

	path := p.choosePath(t, conjuncts)
	residual := path.residual(conjuncts)

	mask := newMask(t)
	markColumns(mask, t, residual)
	markColumns(mask, t, s.GroupBy)
	for _, c := range calls {
		if c.Arg != nil {
			markColumns(mask, t, []ast.Expr{c.Arg})
		}
	}

	node := path.build(mask)
	if len(residual) > 0 {
		node = &Filter{Input: node, Pred: joinConjuncts(residual), Env: env}
	}
	node = &Aggregate{Input: node, GroupBy: s.GroupBy, Calls: calls, Env: env, Out: out}

	if len(keys) > 0 {
		sort := &Sort{Input: node, Keys: keys, Limit: -1, Env: out}
		if count >= 0 && !s.Distinct {
			sort.Limit = count + offset
		}
		node = sort
	}
	node = &Project{Input: node, Exprs: exprs, Names: names, Env: out}
	if s.Distinct {
		node = &Distinct{Input: node}
	}
	if count >= 0 || offset > 0 {
		node = &Limit{Input: node, Count: count, Offset: offset}
	}
	return node, nil
}

// ---------------------------------------------------------------------
// UPDATE and DELETE
// ---------------------------------------------------------------------

func (p *Planner) update(s *ast.Update) (Plan, error) {
	t, err := p.cat.Get(s.Table)
	if err != nil {
		return nil, err
	}
	env := TableEnv(t)
	if s.Where != nil {
		if err := checkTableRefs(t, s.Where); err != nil {
			return nil, err
		}
	}
	sets := make([]Assignment, len(s.Set))
	for i, a := range s.Set {
		pos, ok := t.ColumnIndex(a.Column)
		if !ok {
			return nil, fmt.Errorf("%w: %s.%s", catalog.ErrColumnNotFound, t.Name, a.Column)
		}
		if err := checkTableRefs(t, a.Value); err != nil {
			return nil, err
		}
		sets[i] = Assignment{Column: pos, Name: a.Column, Value: a.Value}
	}

	conjuncts := splitConjuncts(s.Where)
	path := p.choosePath(t, conjuncts)
	residual := path.residual(conjuncts)

	// A write rewrites the whole row and every index entry that points at
	// it, so there is nothing to prune: the scan decodes every column.
	node := path.build(fullMask(t))
	if len(residual) > 0 {
		node = &Filter{Input: node, Pred: joinConjuncts(residual), Env: env}
	}
	return &Update{Table: t, Input: node, Set: sets, Env: env}, nil
}

func (p *Planner) delete(s *ast.Delete) (Plan, error) {
	t, err := p.cat.Get(s.Table)
	if err != nil {
		return nil, err
	}
	env := TableEnv(t)
	if s.Where != nil {
		if err := checkTableRefs(t, s.Where); err != nil {
			return nil, err
		}
	}
	conjuncts := splitConjuncts(s.Where)
	path := p.choosePath(t, conjuncts)
	residual := path.residual(conjuncts)

	node := path.build(fullMask(t))
	if len(residual) > 0 {
		node = &Filter{Input: node, Pred: joinConjuncts(residual), Env: env}
	}
	return &Delete{Table: t, Input: node}, nil
}

// ---------------------------------------------------------------------
// access path selection
// ---------------------------------------------------------------------

// An accessPath is one way of reading a table, together with what it costs
// and which predicates it has already applied.
type accessPath struct {
	table    *catalog.Table
	index    *catalog.Index // nil for a table scan
	bounds   Range
	cost     float64
	consumed map[int]bool // indices into the conjunct list
	ordersBy string       // the column the path returns rows ordered by
}

func (a accessPath) build(mask []bool) Plan {
	switch {
	case a.index != nil:
		return &IndexScan{Table: a.table, Index: a.index, Bounds: a.bounds, Mask: mask}
	case a.bounds.Bounded():
		return &RangeScan{Table: a.table, Bounds: a.bounds, Mask: mask}
	}
	return &SeqScan{Table: a.table, Mask: mask}
}

// residual returns the conjuncts the path did not absorb, which is what the
// Filter above it still has to check.
func (a accessPath) residual(conjuncts []ast.Expr) []ast.Expr {
	var out []ast.Expr
	for i, c := range conjuncts {
		if !a.consumed[i] {
			out = append(out, c)
		}
	}
	return out
}

// provides reports whether the path already returns rows ordered by e.
func (a accessPath) provides(e ast.Expr) bool {
	c, ok := e.(*ast.Column)
	return ok && c.Name == a.ordersBy
}

// choosePath is the cost-lite optimiser. It collects every sargable
// predicate, works out the range each column is restricted to, and prices
// the resulting access paths against a plain table scan.
func (p *Planner) choosePath(t *catalog.Table, conjuncts []ast.Expr) accessPath {
	ranges := map[string]Range{}
	owners := map[string][]int{}
	for i, c := range conjuncts {
		col, r, ok := sargable(t, c)
		if !ok {
			continue
		}
		if prev, seen := ranges[col]; seen {
			ranges[col] = prev.Intersect(r)
		} else {
			ranges[col] = r
		}
		owners[col] = append(owners[col], i)
	}

	best := accessPath{
		table:    t,
		cost:     estimatedRows*costRowRead + costStartup,
		ordersBy: t.PKColumn().Name,
	}

	if r, ok := ranges[t.PKColumn().Name]; ok && r.Bounded() {
		cost := estimatedRows*r.Selectivity()*costRowRead + costStartup
		if cost < best.cost {
			best = accessPath{
				table:    t,
				bounds:   r,
				cost:     cost,
				consumed: indexSet(owners[t.PKColumn().Name]),
				ordersBy: t.PKColumn().Name,
			}
		}
	}

	for i := range t.Indexes {
		idx := &t.Indexes[i]
		r, ok := ranges[idx.Column]
		if !ok || !r.Bounded() {
			continue
		}
		matched := estimatedRows * r.Selectivity()
		cost := matched*(costIndexRead+costRowFetch) + 2*costStartup
		if cost >= best.cost {
			continue
		}
		best = accessPath{
			table:    t,
			index:    idx,
			bounds:   r,
			cost:     cost,
			consumed: indexSet(owners[idx.Column]),
			ordersBy: idx.Column,
		}
	}
	return best
}

// sargable decides whether a predicate can be turned into a scan range —
// "search-argumentable", in the old IBM term. It has to be a comparison or
// a BETWEEN between one column of this table and one constant.
//
// The type rule matters more than it looks: a bound is only pushed down
// when the constant's kind matches the column's, because the encoding sorts
// kinds apart from each other. Comparing an INT column against 1.5 is
// perfectly legal SQL and must keep working, so it stays in the filter,
// where numeric comparison handles it, instead of becoming a byte range
// that would silently return nothing.
func sargable(t *catalog.Table, e ast.Expr) (string, Range, bool) {
	switch n := e.(type) {
	case *ast.Binary:
		op, col, lit := n.Op, asColumn(n.Left), asLiteral(n.Right)
		if col == nil || lit == nil {
			col, lit = asColumn(n.Right), asLiteral(n.Left)
			op = flipOp(n.Op)
		}
		if col == nil || lit == nil || op == "" {
			return "", Range{}, false
		}
		v, ok := boundValue(t, col.Name, lit)
		if !ok {
			return "", Range{}, false
		}
		switch op {
		case "=":
			return col.Name, Range{
				Lo: Bound{Value: v, Inclusive: true, Set: true},
				Hi: Bound{Value: v, Inclusive: true, Set: true},
			}, true
		case "<":
			return col.Name, Range{Hi: Bound{Value: v, Set: true}}, true
		case "<=":
			return col.Name, Range{Hi: Bound{Value: v, Inclusive: true, Set: true}}, true
		case ">":
			return col.Name, Range{Lo: Bound{Value: v, Set: true}}, true
		case ">=":
			return col.Name, Range{Lo: Bound{Value: v, Inclusive: true, Set: true}}, true
		}

	case *ast.Between:
		if n.Not {
			return "", Range{}, false
		}
		col := asColumn(n.Expr)
		lo, hi := asLiteral(n.Lo), asLiteral(n.Hi)
		if col == nil || lo == nil || hi == nil {
			return "", Range{}, false
		}
		lov, okLo := boundValue(t, col.Name, lo)
		hiv, okHi := boundValue(t, col.Name, hi)
		if !okLo || !okHi {
			return "", Range{}, false
		}
		return col.Name, Range{
			Lo: Bound{Value: lov, Inclusive: true, Set: true},
			Hi: Bound{Value: hiv, Inclusive: true, Set: true},
		}, true
	}
	return "", Range{}, false
}

// boundValue checks that a literal can serve as a bound on a column and
// returns the value to encode. NULL never can: `a = NULL` is UNKNOWN for
// every row, which the filter reports correctly and a range cannot.
func boundValue(t *catalog.Table, column string, lit *ast.Literal) (types.Value, bool) {
	pos, ok := t.ColumnIndex(column)
	if !ok {
		return types.Value{}, false
	}
	want := t.Columns[pos].Type
	v := lit.Value
	if v.IsNull() {
		return types.Value{}, false
	}
	if v.Kind() == types.KindInt && want == types.KindFloat {
		v = types.Float(float64(v.AsInt()))
	}
	if v.Kind() != want {
		return types.Value{}, false
	}
	return v, true
}

func asColumn(e ast.Expr) *ast.Column {
	c, _ := e.(*ast.Column)
	return c
}

func asLiteral(e ast.Expr) *ast.Literal {
	l, _ := e.(*ast.Literal)
	return l
}

// flipOp mirrors a comparison so that `5 < a` can be read as `a > 5`.
func flipOp(op string) string {
	switch op {
	case "=", "==":
		return "="
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return ""
}

func indexSet(idx []int) map[int]bool {
	out := make(map[int]bool, len(idx))
	for _, i := range idx {
		out[i] = true
	}
	return out
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

// splitConjuncts flattens `a AND b AND c` into three predicates, which is
// what lets the planner push one of them into the scan and leave the rest
// behind.
func splitConjuncts(e ast.Expr) []ast.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*ast.Binary); ok && b.Op == "AND" {
		return append(splitConjuncts(b.Left), splitConjuncts(b.Right)...)
	}
	return []ast.Expr{e}
}

// joinConjuncts is the inverse, used to rebuild the residual predicate.
func joinConjuncts(list []ast.Expr) ast.Expr {
	if len(list) == 0 {
		return nil
	}
	out := list[0]
	for _, e := range list[1:] {
		out = &ast.Binary{Op: "AND", Left: out, Right: e}
	}
	return out
}

func newMask(t *catalog.Table) []bool { return make([]bool, len(t.Columns)) }

func fullMask(t *catalog.Table) []bool {
	mask := make([]bool, len(t.Columns))
	for i := range mask {
		mask[i] = true
	}
	return mask
}

func markColumns(mask []bool, t *catalog.Table, exprs []ast.Expr) {
	for _, e := range exprs {
		for _, name := range ast.Columns(e) {
			if pos, ok := t.ColumnIndex(name); ok {
				mask[pos] = true
			}
		}
	}
}

// checkTableRefs verifies that every column an expression mentions exists,
// and that any table qualifier names the table being read.
func checkTableRefs(t *catalog.Table, e ast.Expr) error {
	var bad error
	ast.Inspect(e, func(n ast.Expr) bool {
		c, ok := n.(*ast.Column)
		if !ok || bad != nil {
			return true
		}
		if c.Table != "" && c.Table != t.Name {
			bad = fmt.Errorf("%w: %s", catalog.ErrTableNotFound, c.Table)
			return false
		}
		if _, exists := t.ColumnIndex(c.Name); !exists {
			bad = fmt.Errorf("%w: %s.%s", catalog.ErrColumnNotFound, t.Name, c.Name)
			return false
		}
		return true
	})
	return bad
}

// checkGrouped verifies that an expression is legal above an aggregation:
// it may use aggregate calls freely, but a bare column has to be one the
// query grouped by.
func checkGrouped(e ast.Expr, out *Env) error {
	if e == nil {
		return nil
	}
	if _, ok := out.Exprs[e.String()]; ok {
		return nil
	}
	switch n := e.(type) {
	case *ast.Column:
		if _, ok := out.Columns[n.Name]; ok {
			return nil
		}
		return fmt.Errorf("keelsql: column %q must appear in the GROUP BY clause or be used in an aggregate", n.Name)
	case *ast.FuncCall:
		return nil // collected above; its text is already bound
	case *ast.Unary:
		return checkGrouped(n.Expr, out)
	case *ast.Binary:
		if err := checkGrouped(n.Left, out); err != nil {
			return err
		}
		return checkGrouped(n.Right, out)
	case *ast.IsNull:
		return checkGrouped(n.Expr, out)
	case *ast.Between:
		for _, sub := range []ast.Expr{n.Expr, n.Lo, n.Hi} {
			if err := checkGrouped(sub, out); err != nil {
				return err
			}
		}
	case *ast.In:
		if err := checkGrouped(n.Expr, out); err != nil {
			return err
		}
		for _, item := range n.List {
			if err := checkGrouped(item, out); err != nil {
				return err
			}
		}
	case *ast.Like:
		if err := checkGrouped(n.Expr, out); err != nil {
			return err
		}
		return checkGrouped(n.Pattern, out)
	}
	return nil
}

func selectHasAggregate(s *ast.Select) bool {
	for _, col := range s.Columns {
		if !col.Star && ast.HasAggregate(col.Expr) {
			return true
		}
	}
	for _, term := range s.OrderBy {
		if ast.HasAggregate(term.Expr) {
			return true
		}
	}
	return false
}

// resultName is the header a result column gets: its alias, or the
// expression as written.
func resultName(col ast.ResultColumn) string {
	if col.Alias != "" {
		return col.Alias
	}
	return col.Expr.String()
}

// limitValues extracts LIMIT and OFFSET. A missing LIMIT is -1.
func limitValues(s *ast.Select) (count, offset int64, err error) {
	count = -1
	if s.Limit != nil {
		lit, ok := s.Limit.(*ast.Literal)
		if !ok || lit.Value.Kind() != types.KindInt {
			return 0, 0, fmt.Errorf("keelsql: LIMIT must be an integer constant")
		}
		count = lit.Value.AsInt()
	}
	if s.Offset != nil {
		lit, ok := s.Offset.(*ast.Literal)
		if !ok || lit.Value.Kind() != types.KindInt {
			return 0, 0, fmt.Errorf("keelsql: OFFSET must be an integer constant")
		}
		offset = lit.Value.AsInt()
	}
	return count, offset, nil
}

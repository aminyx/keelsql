package exec

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

// Build turns a physical plan into a running operator tree.
func Build(p plan.Plan, rw storage.ReadWriter) (Operator, error) {
	switch n := p.(type) {
	case *plan.SeqScan:
		prefix := keycodec.DataPrefix(n.Table.ID)
		return newScan(rw, n.Table, n.Mask, prefix, keycodec.PrefixEnd(prefix)), nil

	case *plan.RangeScan:
		start, end := n.Bounds.KeyBytes(keycodec.DataPrefix(n.Table.ID))
		return newScan(rw, n.Table, n.Mask, start, end), nil

	case *plan.IndexScan:
		start, end := n.Bounds.KeyBytes(keycodec.IndexPrefix(n.Table.ID, n.Index.ID))
		return &indexScan{
			rw:    rw,
			table: n.Table,
			mask:  n.Mask,
			it:    rw.Scan(start, end),
		}, nil

	case *plan.Filter:
		input, err := Build(n.Input, rw)
		if err != nil {
			return nil, err
		}
		return &filter{input: input, pred: n.Pred, env: n.Env}, nil

	case *plan.Project:
		input, err := Build(n.Input, rw)
		if err != nil {
			return nil, err
		}
		return &project{input: input, exprs: n.Exprs, env: n.Env}, nil

	case *plan.Aggregate:
		input, err := Build(n.Input, rw)
		if err != nil {
			return nil, err
		}
		return &aggregate{input: input, node: n}, nil

	case *plan.Sort:
		input, err := Build(n.Input, rw)
		if err != nil {
			return nil, err
		}
		return &sortOp{input: input, keys: n.Keys, limit: n.Limit, env: n.Env}, nil

	case *plan.Limit:
		input, err := Build(n.Input, rw)
		if err != nil {
			return nil, err
		}
		return &limitOp{input: input, count: n.Count, offset: n.Offset}, nil

	case *plan.Distinct:
		input, err := Build(n.Input, rw)
		if err != nil {
			return nil, err
		}
		return &distinct{input: input, seen: map[string]bool{}}, nil
	}
	return nil, fmt.Errorf("keelsql: %T is not a query operator", p)
}

// Columns returns the output column names of a query plan.
func Columns(p plan.Plan) []string {
	for p != nil {
		if pr, ok := p.(*plan.Project); ok {
			return pr.Names
		}
		children := p.Children()
		if len(children) == 0 {
			return nil
		}
		p = children[0]
	}
	return nil
}

// ---------------------------------------------------------------------
// scans
// ---------------------------------------------------------------------

// scan reads stored rows out of a byte range of keelstore and decodes the
// columns the planner asked for.
type scan struct {
	table *catalog.Table
	mask  []bool
	it    storage.Iterator
	done  bool
}

func newScan(rw storage.ReadWriter, t *catalog.Table, mask []bool, start, end []byte) *scan {
	return &scan{table: t, mask: mask, it: rw.Scan(start, end)}
}

func (s *scan) Next() (Row, bool, error) {
	if s.done {
		return nil, false, nil
	}
	if !s.it.Next() {
		s.done = true
		return nil, false, s.it.Err()
	}
	row, err := keycodec.DecodeRowMasked(s.it.Value(), s.mask)
	if err != nil {
		return nil, false, fmt.Errorf("table %s: %w", s.table.Name, err)
	}
	return row, true, nil
}

func (s *scan) Close() error {
	if s.it == nil {
		return nil
	}
	it := s.it
	s.it = nil
	return it.Close()
}

// indexScan walks a secondary index and fetches each row it points at. The
// index entry carries the indexed value and the primary key, so the lookup
// is a single Get on the row key — no extra structure and no scan.
type indexScan struct {
	table *catalog.Table
	mask  []bool
	rw    storage.ReadWriter
	it    storage.Iterator
	done  bool
}

func (s *indexScan) Next() (Row, bool, error) {
	if s.done {
		return nil, false, nil
	}
	if !s.it.Next() {
		s.done = true
		return nil, false, s.it.Err()
	}
	_, pk, err := keycodec.IndexEntry(s.it.Key())
	if err != nil {
		return nil, false, err
	}
	blob, err := s.rw.Get(keycodec.RowKey(s.table.ID, pk))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// An index entry that points nowhere means the index and the
			// table disagree, which keelsql maintains transactionally and
			// so should never happen. Report it rather than skip it.
			return nil, false, fmt.Errorf("keelsql: index entry for %s points at missing row %s",
				s.table.Name, pk.SQL())
		}
		return nil, false, err
	}
	row, err := keycodec.DecodeRowMasked(blob, s.mask)
	if err != nil {
		return nil, false, fmt.Errorf("table %s: %w", s.table.Name, err)
	}
	return row, true, nil
}

func (s *indexScan) Close() error {
	if s.it == nil {
		return nil
	}
	it := s.it
	s.it = nil
	return it.Close()
}

// ---------------------------------------------------------------------
// filter, project, limit, distinct
// ---------------------------------------------------------------------

type filter struct {
	input Operator
	pred  ast.Expr
	env   *plan.Env
}

func (f *filter) Next() (Row, bool, error) {
	for {
		row, ok, err := f.input.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		keep, err := EvalPredicate(f.pred, f.env, row)
		if err != nil {
			return nil, false, err
		}
		// Only TRUE keeps a row. UNKNOWN is discarded, which is why
		// `WHERE c = 1` never returns a row whose c is NULL.
		if keep.IsTrue() {
			return row, true, nil
		}
	}
}

func (f *filter) Close() error { return f.input.Close() }

type project struct {
	input Operator
	exprs []ast.Expr
	env   *plan.Env
}

func (p *project) Next() (Row, bool, error) {
	row, ok, err := p.input.Next()
	if err != nil || !ok {
		return nil, false, err
	}
	out := make(Row, len(p.exprs))
	for i, e := range p.exprs {
		v, err := Eval(e, p.env, row)
		if err != nil {
			return nil, false, err
		}
		out[i] = v
	}
	return out, true, nil
}

func (p *project) Close() error { return p.input.Close() }

type limitOp struct {
	input   Operator
	count   int64
	offset  int64
	emitted int64
	skipped int64
}

func (l *limitOp) Next() (Row, bool, error) {
	for l.skipped < l.offset {
		_, ok, err := l.input.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		l.skipped++
	}
	if l.count >= 0 && l.emitted >= l.count {
		return nil, false, nil
	}
	row, ok, err := l.input.Next()
	if err != nil || !ok {
		return nil, false, err
	}
	l.emitted++
	return row, true, nil
}

func (l *limitOp) Close() error { return l.input.Close() }

// distinct drops duplicate rows. Two rows are the same when their
// order-preserving encodings are, which makes NULL equal to NULL here —
// the rule DISTINCT uses, unlike the one `=` uses.
type distinct struct {
	input Operator
	seen  map[string]bool
}

func (d *distinct) Next() (Row, bool, error) {
	for {
		row, ok, err := d.input.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		key := string(keycodec.Key(row...))
		if d.seen[key] {
			continue
		}
		d.seen[key] = true
		return row, true, nil
	}
}

func (d *distinct) Close() error { return d.input.Close() }

// ---------------------------------------------------------------------
// sort
// ---------------------------------------------------------------------

// sortOp is a blocking operator: it drains its input before returning
// anything, because the last row read can be the first row out.
//
// The sort is in memory. keelsql does not spill to disk, so a query that
// orders more rows than fit in memory will not finish — the honest
// limitation of a small engine, and the reason LIMIT is pushed into the
// sort whenever it can be.
type sortOp struct {
	input Operator
	keys  []plan.SortKey
	limit int64
	env   *plan.Env

	rows   []Row
	pos    int
	loaded bool
}

func (s *sortOp) Next() (Row, bool, error) {
	if !s.loaded {
		if err := s.load(); err != nil {
			return nil, false, err
		}
		s.loaded = true
	}
	if s.pos >= len(s.rows) {
		return nil, false, nil
	}
	row := s.rows[s.pos]
	s.pos++
	return row, true, nil
}

func (s *sortOp) Close() error { return s.input.Close() }

// sortItem carries a row together with its evaluated sort keys and its
// arrival number. The arrival number breaks ties, so a top-N sort returns
// exactly what a full sort followed by a LIMIT would.
type sortItem struct {
	row  Row
	keys []types.Value
	seq  int
}

func (s *sortOp) load() error {
	var items []sortItem
	var h *topHeap
	if s.limit >= 0 {
		h = &topHeap{less: s.less}
	}

	seq := 0
	for {
		row, ok, err := s.input.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		item := sortItem{row: row, seq: seq}
		seq++
		item.keys = make([]types.Value, len(s.keys))
		for i, k := range s.keys {
			v, err := Eval(k.Expr, s.env, row)
			if err != nil {
				return err
			}
			item.keys[i] = v
		}

		if h == nil {
			items = append(items, item)
			continue
		}
		// Bounded top-N: keep at most limit items, with the worst of them
		// at the root so it can be evicted in constant time.
		if int64(len(h.items)) < s.limit {
			heap.Push(h, item)
			continue
		}
		if s.limit > 0 && s.less(item, h.items[0]) {
			h.items[0] = item
			heap.Fix(h, 0)
		}
	}

	if h != nil {
		items = h.items
	}
	sort.SliceStable(items, func(i, j int) bool { return s.less(items[i], items[j]) })
	s.rows = make([]Row, len(items))
	for i, item := range items {
		s.rows[i] = item.row
	}
	return nil
}

// less orders two items by the ORDER BY keys, using the same total order as
// the key encoding, so NULLs come first ascending and last descending.
func (s *sortOp) less(a, b sortItem) bool {
	for i, k := range s.keys {
		c := types.Order(a.keys[i], b.keys[i])
		if k.Desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
	}
	return a.seq < b.seq
}

// topHeap is a max-heap with respect to the sort order: its root is the
// item that would come last, and therefore the one to drop.
type topHeap struct {
	items []sortItem
	less  func(a, b sortItem) bool
}

func (h *topHeap) Len() int           { return len(h.items) }
func (h *topHeap) Less(i, j int) bool { return h.less(h.items[j], h.items[i]) }
func (h *topHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *topHeap) Push(x any)         { h.items = append(h.items, x.(sortItem)) }

// Pop removes the last item; container/heap calls it after moving the root
// there. keelsql never pops in practice — the top-N loop overwrites the
// root and calls Fix — but heap.Interface requires it.
func (h *topHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

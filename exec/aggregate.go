package exec

import (
	"fmt"
	"sort"

	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/types"
)

// aggregate is a hash aggregation: it drains its input, bucketing rows by
// the encoded GROUP BY values, and emits one row per bucket.
//
// Because the bucket key is the order-preserving encoding of the group
// values, sorting the keys as byte strings sorts the groups by value. That
// makes the output order deterministic without a separate sort, which is
// what lets a test assert on it.
//
// With no GROUP BY there is exactly one bucket, and it exists even when the
// input is empty: `SELECT COUNT(*) FROM t` on an empty table returns 0,
// not nothing.
type aggregate struct {
	input Operator
	node  *plan.Aggregate

	rows   []Row
	pos    int
	loaded bool
}

type aggGroup struct {
	key    string
	values []types.Value
	accs   []accumulator
}

func (a *aggregate) Next() (Row, bool, error) {
	if !a.loaded {
		if err := a.load(); err != nil {
			return nil, false, err
		}
		a.loaded = true
	}
	if a.pos >= len(a.rows) {
		return nil, false, nil
	}
	row := a.rows[a.pos]
	a.pos++
	return row, true, nil
}

func (a *aggregate) Close() error { return a.input.Close() }

func (a *aggregate) load() error {
	groups := map[string]*aggGroup{}
	var order []string

	for {
		row, ok, err := a.input.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		values := make([]types.Value, len(a.node.GroupBy))
		for i, g := range a.node.GroupBy {
			v, err := Eval(g, a.node.Env, row)
			if err != nil {
				return err
			}
			values[i] = v
		}
		key := string(keycodec.Key(values...))

		g, seen := groups[key]
		if !seen {
			accs, err := newAccumulators(a.node.Calls)
			if err != nil {
				return err
			}
			g = &aggGroup{key: key, values: values, accs: accs}
			groups[key] = g
			order = append(order, key)
		}

		for i, call := range a.node.Calls {
			arg := types.Null()
			if call.Arg != nil {
				if arg, err = Eval(call.Arg, a.node.Env, row); err != nil {
					return err
				}
			}
			if err := g.accs[i].add(arg); err != nil {
				return err
			}
		}
	}

	// An ungrouped aggregation always produces one row.
	if len(a.node.GroupBy) == 0 && len(groups) == 0 {
		accs, err := newAccumulators(a.node.Calls)
		if err != nil {
			return err
		}
		key := ""
		groups[key] = &aggGroup{key: key, accs: accs}
		order = append(order, key)
	}

	sort.Strings(order)
	a.rows = make([]Row, 0, len(order))
	for _, key := range order {
		g := groups[key]
		row := make(Row, 0, len(g.values)+len(g.accs))
		row = append(row, g.values...)
		for _, acc := range g.accs {
			v, err := acc.result()
			if err != nil {
				return err
			}
			row = append(row, v)
		}
		a.rows = append(a.rows, row)
	}
	return nil
}

// ---------------------------------------------------------------------
// accumulators
// ---------------------------------------------------------------------

// An accumulator folds one column of one group.
//
// Every aggregate except COUNT(*) ignores NULL inputs, which is why
// AVG over a column with holes divides by the number of values present
// rather than by the number of rows.
type accumulator interface {
	add(v types.Value) error
	result() (types.Value, error)
}

func newAccumulators(calls []plan.AggCall) ([]accumulator, error) {
	out := make([]accumulator, len(calls))
	for i, call := range calls {
		acc, err := newAccumulator(call)
		if err != nil {
			return nil, err
		}
		out[i] = acc
	}
	return out, nil
}

func newAccumulator(call plan.AggCall) (accumulator, error) {
	switch call.Func {
	case "COUNT":
		return &countAcc{all: call.Arg == nil}, nil
	case "SUM":
		return &sumAcc{}, nil
	case "AVG":
		return &avgAcc{}, nil
	case "MIN":
		return &extremeAcc{want: -1}, nil
	case "MAX":
		return &extremeAcc{want: 1}, nil
	}
	return nil, fmt.Errorf("keelsql: unknown aggregate %s", call.Func)
}

// countAcc counts rows (COUNT(*)) or non-NULL values (COUNT(x)).
type countAcc struct {
	all bool
	n   int64
}

func (c *countAcc) add(v types.Value) error {
	if c.all || !v.IsNull() {
		c.n++
	}
	return nil
}

func (c *countAcc) result() (types.Value, error) { return types.Int(c.n), nil }

// sumAcc adds the non-NULL values. The sum of nothing is NULL, not zero —
// SQL distinguishes "no rows" from "rows that added up to zero".
type sumAcc struct {
	total types.Value
	any   bool
}

func (s *sumAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if !v.IsNumeric() {
		return fmt.Errorf("%w: SUM wants numbers, got %s", types.ErrType, v.Kind())
	}
	if !s.any {
		s.total, s.any = v, true
		return nil
	}
	sum, err := types.Add(s.total, v)
	if err != nil {
		return err
	}
	s.total = sum
	return nil
}

func (s *sumAcc) result() (types.Value, error) {
	if !s.any {
		return types.Null(), nil
	}
	return s.total, nil
}

// avgAcc keeps a running total and a count and divides at the end, always
// in floating point.
type avgAcc struct {
	total float64
	n     int64
}

func (a *avgAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if !v.IsNumeric() {
		return fmt.Errorf("%w: AVG wants numbers, got %s", types.ErrType, v.Kind())
	}
	a.total += v.AsFloat()
	a.n++
	return nil
}

func (a *avgAcc) result() (types.Value, error) {
	if a.n == 0 {
		return types.Null(), nil
	}
	return types.Float(a.total / float64(a.n)), nil
}

// extremeAcc keeps the smallest or largest value seen, using the same total
// order as ORDER BY.
type extremeAcc struct {
	want  int // -1 for MIN, 1 for MAX
	best  types.Value
	found bool
}

func (e *extremeAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if !e.found {
		e.best, e.found = v, true
		return nil
	}
	if c := types.Order(v, e.best); (e.want < 0 && c < 0) || (e.want > 0 && c > 0) {
		e.best = v
	}
	return nil
}

func (e *extremeAcc) result() (types.Value, error) {
	if !e.found {
		return types.Null(), nil
	}
	return e.best, nil
}

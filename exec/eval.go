// Package exec runs physical plans.
//
// The model is the classic volcano iterator: every operator exposes
//
//	Next() (Row, bool, error)
//
// and pulls from its input when it needs another row. Nothing materialises
// an intermediate result unless it has to — a scan, a filter, a projection
// and a limit stream one row at a time, and only the sort, the aggregation
// and DISTINCT buffer anything.
package exec

import (
	"fmt"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/types"
)

// A Row is one tuple. Its layout is whatever the operator below produced:
// a stored row for a scan, the SELECT list for a projection, the group
// values followed by the aggregate results for an aggregation.
type Row = []types.Value

// An Operator is one node of the running plan.
type Operator interface {
	// Next returns the next row. The boolean is false when the operator is
	// exhausted, at which point Row is nil.
	Next() (Row, bool, error)
	// Close releases whatever the operator holds — in practice, iterators
	// pinned inside keelstore. It is safe to call more than once.
	Close() error
}

// Eval evaluates a scalar expression against a row.
//
// Before anything else it checks whether the environment already binds the
// expression as a whole. That single lookup is what makes aggregates work:
// above a GROUP BY the row *is* the group's values and results, so
// `COUNT(*)` resolves to a position rather than being computed again.
func Eval(e ast.Expr, env *plan.Env, row Row) (types.Value, error) {
	if e == nil {
		return types.Null(), nil
	}
	if env != nil && len(env.Exprs) > 0 {
		if pos, ok := env.Exprs[e.String()]; ok {
			return at(row, pos)
		}
	}

	switch n := e.(type) {
	case *ast.Literal:
		return n.Value, nil

	case *ast.Column:
		if env == nil {
			return types.Null(), fmt.Errorf("keelsql: no columns are in scope here")
		}
		pos, ok := env.Columns[n.Name]
		if !ok {
			return types.Null(), fmt.Errorf("keelsql: column %q is not available here", n.Name)
		}
		return at(row, pos)

	case *ast.Unary:
		return evalUnary(n, env, row)

	case *ast.Binary:
		return evalBinary(n, env, row)

	case *ast.IsNull, *ast.Between, *ast.In, *ast.Like:
		t, err := EvalPredicate(e, env, row)
		if err != nil {
			return types.Null(), err
		}
		return t.Value(), nil

	case *ast.FuncCall:
		return types.Null(), fmt.Errorf("keelsql: aggregate %s is only allowed in a grouped query", n.String())

	case *ast.Star:
		return types.Null(), fmt.Errorf("keelsql: * cannot be used here")
	}
	return types.Null(), fmt.Errorf("keelsql: cannot evaluate %T", e)
}

func at(row Row, pos int) (types.Value, error) {
	if pos < 0 || pos >= len(row) {
		return types.Null(), fmt.Errorf("keelsql: row has %d columns, wanted position %d", len(row), pos)
	}
	return row[pos], nil
}

func evalUnary(n *ast.Unary, env *plan.Env, row Row) (types.Value, error) {
	v, err := Eval(n.Expr, env, row)
	if err != nil {
		return types.Null(), err
	}
	switch n.Op {
	case "NOT":
		t, err := types.Truth(v)
		if err != nil {
			return types.Null(), err
		}
		return t.Not().Value(), nil
	case "-":
		return types.Neg(v)
	case "+":
		return v, nil
	}
	return types.Null(), fmt.Errorf("keelsql: unknown unary operator %q", n.Op)
}

func evalBinary(n *ast.Binary, env *plan.Env, row Row) (types.Value, error) {
	switch n.Op {
	case "AND", "OR", "=", "==", "!=", "<>", "<", "<=", ">", ">=":
		t, err := EvalPredicate(n, env, row)
		if err != nil {
			return types.Null(), err
		}
		return t.Value(), nil
	}

	left, err := Eval(n.Left, env, row)
	if err != nil {
		return types.Null(), err
	}
	right, err := Eval(n.Right, env, row)
	if err != nil {
		return types.Null(), err
	}
	switch n.Op {
	case "+":
		return types.Add(left, right)
	case "-":
		return types.Sub(left, right)
	case "*":
		return types.Mul(left, right)
	case "/":
		return types.Div(left, right)
	case "%":
		return types.Mod(left, right)
	}
	return types.Null(), fmt.Errorf("keelsql: unknown operator %q", n.Op)
}

// EvalPredicate evaluates a condition under SQL's three-valued logic. A nil
// expression — a query with no WHERE clause — is TRUE.
//
// The short-circuits are the ones Kleene logic allows and no more:
// `FALSE AND anything` is FALSE without looking at the right operand, and
// `TRUE OR anything` is TRUE. UNKNOWN never short-circuits, because the
// other operand can still decide the result.
func EvalPredicate(e ast.Expr, env *plan.Env, row Row) (types.Bool3, error) {
	if e == nil {
		return types.True, nil
	}

	switch n := e.(type) {
	case *ast.Binary:
		switch n.Op {
		case "AND":
			left, err := EvalPredicate(n.Left, env, row)
			if err != nil {
				return types.Unknown, err
			}
			if left == types.False {
				return types.False, nil
			}
			right, err := EvalPredicate(n.Right, env, row)
			if err != nil {
				return types.Unknown, err
			}
			return left.And(right), nil

		case "OR":
			left, err := EvalPredicate(n.Left, env, row)
			if err != nil {
				return types.Unknown, err
			}
			if left == types.True {
				return types.True, nil
			}
			right, err := EvalPredicate(n.Right, env, row)
			if err != nil {
				return types.Unknown, err
			}
			return left.Or(right), nil

		case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
			left, err := Eval(n.Left, env, row)
			if err != nil {
				return types.Unknown, err
			}
			right, err := Eval(n.Right, env, row)
			if err != nil {
				return types.Unknown, err
			}
			return types.CompareOp(n.Op, left, right)
		}

	case *ast.Unary:
		if n.Op == "NOT" {
			t, err := EvalPredicate(n.Expr, env, row)
			if err != nil {
				return types.Unknown, err
			}
			return t.Not(), nil
		}

	case *ast.IsNull:
		// IS NULL is the one test that never returns UNKNOWN: it asks
		// about the value's presence, not about its content.
		v, err := Eval(n.Expr, env, row)
		if err != nil {
			return types.Unknown, err
		}
		return types.B3(v.IsNull() != n.Not), nil

	case *ast.Between:
		v, err := Eval(n.Expr, env, row)
		if err != nil {
			return types.Unknown, err
		}
		lo, err := Eval(n.Lo, env, row)
		if err != nil {
			return types.Unknown, err
		}
		hi, err := Eval(n.Hi, env, row)
		if err != nil {
			return types.Unknown, err
		}
		ge, err := types.CompareOp(">=", v, lo)
		if err != nil {
			return types.Unknown, err
		}
		le, err := types.CompareOp("<=", v, hi)
		if err != nil {
			return types.Unknown, err
		}
		out := ge.And(le)
		if n.Not {
			out = out.Not()
		}
		return out, nil

	case *ast.In:
		// x IN (a, b) is x = a OR x = b, three-valued logic included: with
		// a NULL in the list a non-match is UNKNOWN, not FALSE.
		v, err := Eval(n.Expr, env, row)
		if err != nil {
			return types.Unknown, err
		}
		out := types.False
		for _, item := range n.List {
			candidate, err := Eval(item, env, row)
			if err != nil {
				return types.Unknown, err
			}
			eq, err := types.CompareOp("=", v, candidate)
			if err != nil {
				return types.Unknown, err
			}
			out = out.Or(eq)
			if out == types.True {
				break
			}
		}
		if n.Not {
			out = out.Not()
		}
		return out, nil

	case *ast.Like:
		v, err := Eval(n.Expr, env, row)
		if err != nil {
			return types.Unknown, err
		}
		pattern, err := Eval(n.Pattern, env, row)
		if err != nil {
			return types.Unknown, err
		}
		out, err := types.LikeValue(v, pattern)
		if err != nil {
			return types.Unknown, err
		}
		if n.Not {
			out = out.Not()
		}
		return out, nil
	}

	// Anything else has to evaluate to a boolean on its own.
	v, err := Eval(e, env, row)
	if err != nil {
		return types.Unknown, err
	}
	return types.Truth(v)
}

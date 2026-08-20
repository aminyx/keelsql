package types

import "fmt"

func wrapType(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrType, fmt.Sprintf(format, args...))
}

// Add, Sub, Mul, Div and Mod implement SQL arithmetic. Every one of them
// propagates NULL: if either operand is NULL the result is NULL, without
// looking at the other operand's kind.
//
// Two integers stay integers, which means integer division truncates
// towards zero, as it does in PostgreSQL. Mixing an integer with a float
// widens to float.

// Add returns a + b.
func Add(a, b Value) (Value, error) {
	return arith(a, b, "+",
		func(x, y int64) int64 { return x + y },
		func(x, y float64) float64 { return x + y })
}

// Sub returns a - b.
func Sub(a, b Value) (Value, error) {
	return arith(a, b, "-",
		func(x, y int64) int64 { return x - y },
		func(x, y float64) float64 { return x - y })
}

// Mul returns a * b.
func Mul(a, b Value) (Value, error) {
	return arith(a, b, "*",
		func(x, y int64) int64 { return x * y },
		func(x, y float64) float64 { return x * y })
}

// Div returns a / b. Integer division truncates; a zero divisor is an
// error rather than NULL.
func Div(a, b Value) (Value, error) {
	if a.IsNull() || b.IsNull() {
		return Null(), nil
	}
	if err := checkNumeric(a, b, "/"); err != nil {
		return Value{}, err
	}
	if a.Kind() == KindInt && b.Kind() == KindInt {
		if b.AsInt() == 0 {
			return Value{}, ErrDivideByZero
		}
		return Int(a.AsInt() / b.AsInt()), nil
	}
	if b.AsFloat() == 0 {
		return Value{}, ErrDivideByZero
	}
	return Float(a.AsFloat() / b.AsFloat()), nil
}

// Mod returns a % b. Both operands must be integers.
func Mod(a, b Value) (Value, error) {
	if a.IsNull() || b.IsNull() {
		return Null(), nil
	}
	if a.Kind() != KindInt || b.Kind() != KindInt {
		return Value{}, wrapType("operator %% wants two INTs, got %s and %s", a.Kind(), b.Kind())
	}
	if b.AsInt() == 0 {
		return Value{}, ErrDivideByZero
	}
	return Int(a.AsInt() % b.AsInt()), nil
}

// Neg returns -a.
func Neg(a Value) (Value, error) {
	switch a.Kind() {
	case KindNull:
		return Null(), nil
	case KindInt:
		return Int(-a.AsInt()), nil
	case KindFloat:
		return Float(-a.AsFloat()), nil
	}
	return Value{}, wrapType("operator - wants a number, got %s", a.Kind())
}

func arith(a, b Value, op string, fi func(x, y int64) int64, ff func(x, y float64) float64) (Value, error) {
	if a.IsNull() || b.IsNull() {
		return Null(), nil
	}
	if err := checkNumeric(a, b, op); err != nil {
		return Value{}, err
	}
	if a.Kind() == KindInt && b.Kind() == KindInt {
		return Int(fi(a.AsInt(), b.AsInt())), nil
	}
	return Float(ff(a.AsFloat(), b.AsFloat())), nil
}

func checkNumeric(a, b Value, op string) error {
	if !a.IsNumeric() || !b.IsNumeric() {
		return wrapType("operator %s wants numbers, got %s and %s", op, a.Kind(), b.Kind())
	}
	return nil
}

// Like matches s against a SQL LIKE pattern, in which '%' stands for any
// run of characters (including none) and '_' stands for exactly one
// character. Matching is done over runes, so a multi-byte character counts
// as one '_'.
//
// The implementation is the linear-space greedy scan rather than naive
// recursion: it remembers the position of the last '%' and backtracks to it
// on a mismatch, which keeps a pattern such as '%a%a%a%b' from blowing up
// exponentially.
func Like(s, pattern string) bool {
	text := []rune(s)
	pat := []rune(pattern)

	var ti, pi int
	star, mark := -1, 0
	for ti < len(text) {
		switch {
		case pi < len(pat) && (pat[pi] == '_' || pat[pi] == text[ti]):
			ti++
			pi++
		case pi < len(pat) && pat[pi] == '%':
			star = pi
			pi++
			mark = ti
		case star >= 0:
			// Retry the last '%', letting it swallow one more rune.
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '%' {
		pi++
	}
	return pi == len(pat)
}

// LikeValue applies LIKE with SQL's NULL rules: if either side is NULL the
// result is UNKNOWN.
func LikeValue(s, pattern Value) (Bool3, error) {
	if s.IsNull() || pattern.IsNull() {
		return Unknown, nil
	}
	if s.Kind() != KindText || pattern.Kind() != KindText {
		return Unknown, wrapType("LIKE wants TEXT operands, got %s and %s", s.Kind(), pattern.Kind())
	}
	return B3(Like(s.AsText(), pattern.AsText())), nil
}

// CompareOp applies one of the six comparison operators with three-valued
// semantics: a comparison against NULL is UNKNOWN, never TRUE or FALSE.
func CompareOp(op string, a, b Value) (Bool3, error) {
	if a.IsNull() || b.IsNull() {
		return Unknown, nil
	}
	c, err := Compare(a, b)
	if err != nil {
		return Unknown, err
	}
	switch op {
	case "=", "==":
		return B3(c == 0), nil
	case "!=", "<>":
		return B3(c != 0), nil
	case "<":
		return B3(c < 0), nil
	case "<=":
		return B3(c <= 0), nil
	case ">":
		return B3(c > 0), nil
	case ">=":
		return B3(c >= 0), nil
	}
	return Unknown, fmt.Errorf("keelsql: unknown comparison operator %q", op)
}

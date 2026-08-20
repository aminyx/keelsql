// Package types holds keelsql's value model: the five value kinds a column
// can hold, the comparison rules that order them, and the three-valued
// logic that SQL's WHERE clause is built on.
//
// A Value is immutable and comparable by construction: it is a small
// struct, never a pointer, so rows are plain slices of values and copying a
// row copies its data.
package types

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind enumerates the value kinds keelsql understands. The numeric order of
// the constants is also the order used when values of different kinds have
// to be sorted against each other, so NULL sorts before everything.
type Kind uint8

// The value kinds. KindNull is the kind of the NULL value; the other four
// double as the column types that CREATE TABLE accepts.
const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindFloat
	KindText
)

// String returns the SQL spelling of the kind.
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "NULL"
	case KindBool:
		return "BOOL"
	case KindInt:
		return "INT"
	case KindFloat:
		return "FLOAT"
	case KindText:
		return "TEXT"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// Errors reported by the value operations.
var (
	// ErrType is returned when an operation is applied to operands whose
	// kinds it cannot combine, such as 'abc' + 1.
	ErrType = errors.New("keelsql: type mismatch")

	// ErrDivideByZero is returned by Div and Mod with a zero divisor.
	// SQL treats this as an error rather than as NULL.
	ErrDivideByZero = errors.New("keelsql: division by zero")
)

// MarshalText makes a Kind serialise as its SQL name, so the persisted
// catalog reads as "INT" rather than as "2".
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText parses the name written by MarshalText.
func (k *Kind) UnmarshalText(text []byte) error {
	name := string(text)
	if strings.EqualFold(name, "NULL") {
		*k = KindNull
		return nil
	}
	parsed, ok := ParseKind(name)
	if !ok {
		return fmt.Errorf("keelsql: unknown column type %q", name)
	}
	*k = parsed
	return nil
}

// ParseKind maps a CREATE TABLE type name to a Kind. The name is matched
// case-insensitively and the usual aliases are accepted.
func ParseKind(name string) (Kind, bool) {
	switch strings.ToUpper(name) {
	case "INT", "INTEGER", "BIGINT":
		return KindInt, true
	case "FLOAT", "REAL", "DOUBLE":
		return KindFloat, true
	case "TEXT", "STRING", "VARCHAR":
		return KindText, true
	case "BOOL", "BOOLEAN":
		return KindBool, true
	}
	return KindNull, false
}

// A Value is a single SQL datum: NULL, a boolean, a 64-bit integer, a
// 64-bit float or a string. The zero Value is NULL.
type Value struct {
	kind Kind
	i    int64
	f    float64
	s    string
}

// Null returns the NULL value.
func Null() Value { return Value{} }

// Int returns an integer value.
func Int(i int64) Value { return Value{kind: KindInt, i: i} }

// Float returns a floating-point value.
func Float(f float64) Value { return Value{kind: KindFloat, f: f} }

// Text returns a string value.
func Text(s string) Value { return Value{kind: KindText, s: s} }

// Bool returns a boolean value.
func Bool(b bool) Value {
	v := Value{kind: KindBool}
	if b {
		v.i = 1
	}
	return v
}

// Kind reports the value's kind.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether the value is NULL.
func (v Value) IsNull() bool { return v.kind == KindNull }

// AsInt returns the integer payload. It is only meaningful for KindInt.
func (v Value) AsInt() int64 { return v.i }

// AsFloat returns the float payload, widening an integer if necessary.
func (v Value) AsFloat() float64 {
	if v.kind == KindInt {
		return float64(v.i)
	}
	return v.f
}

// AsText returns the string payload. It is only meaningful for KindText.
func (v Value) AsText() string { return v.s }

// AsBool returns the boolean payload. It is only meaningful for KindBool.
func (v Value) AsBool() bool { return v.i != 0 }

// IsNumeric reports whether the value is an integer or a float.
func (v Value) IsNumeric() bool { return v.kind == KindInt || v.kind == KindFloat }

// String renders the value the way the CLI prints it in a result table:
// NULL has no quotes, and neither do strings.
func (v Value) String() string {
	switch v.kind {
	case KindNull:
		return "NULL"
	case KindBool:
		if v.i != 0 {
			return "true"
		}
		return "false"
	case KindInt:
		return strconv.FormatInt(v.i, 10)
	case KindFloat:
		return formatFloat(v.f)
	case KindText:
		return v.s
	}
	return "?"
}

// SQL renders the value as a SQL literal, so that quoting round-trips.
func (v Value) SQL() string {
	if v.kind == KindText {
		return "'" + strings.ReplaceAll(v.s, "'", "''") + "'"
	}
	return v.String()
}

// formatFloat prints a float in the shortest form that parses back to the
// same value, but always with a decimal point so that a FLOAT column never
// looks like an INT column in query output.
func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// Compare orders two non-NULL values of comparable kinds. Integers and
// floats compare numerically across kinds; every other mixed pair is a type
// error. Callers must handle NULL themselves: SQL says a comparison with
// NULL is unknown, not an ordering.
func Compare(a, b Value) (int, error) {
	if a.IsNull() || b.IsNull() {
		return 0, fmt.Errorf("%w: NULL has no ordering", ErrType)
	}
	switch {
	case a.kind == KindInt && b.kind == KindInt:
		return cmpInt(a.i, b.i), nil
	case a.IsNumeric() && b.IsNumeric():
		return cmpFloat(a.AsFloat(), b.AsFloat()), nil
	case a.kind == KindText && b.kind == KindText:
		return strings.Compare(a.s, b.s), nil
	case a.kind == KindBool && b.kind == KindBool:
		return cmpInt(a.i, b.i), nil
	}
	return 0, fmt.Errorf("%w: cannot compare %s with %s", ErrType, a.kind, b.kind)
}

// Order is the total order keelsql sorts by: NULL first, then by kind in
// the order the Kind constants are declared, then by value within the kind.
// ORDER BY, GROUP BY, MIN, MAX and DISTINCT all use it.
//
// The same order is what the key encoding in package keycodec reproduces
// byte for byte, and that identity is what makes an index scan a sorted
// scan. It is why Order sorts by kind rather than comparing an INT against
// a FLOAT numerically the way Compare does: an encoded value carries its
// kind in its first byte, so the bytes cannot interleave two kinds however
// their numbers relate.
//
// The two rules never disagree in practice, because a column has one
// declared type: every value in one column, and therefore every value in
// one index, is of one kind (or NULL, which sorts first under both).
func Order(a, b Value) int {
	if a.kind != b.kind {
		return cmpInt(int64(a.kind), int64(b.kind))
	}
	switch a.kind {
	case KindNull:
		return 0
	case KindBool, KindInt:
		return cmpInt(a.i, b.i)
	case KindFloat:
		return cmpFloat(a.f, b.f)
	case KindText:
		return strings.Compare(a.s, b.s)
	}
	return 0
}

// Equal reports whether two values are identical in both kind and content,
// treating NULL as equal to NULL. It is not SQL's `=`, which is unknown for
// NULL and numeric across INT and FLOAT; use Compare for that.
func Equal(a, b Value) bool { return Order(a, b) == 0 }

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	case a == b:
		return 0
	}
	// At least one operand is NaN. Order NaN after everything so that the
	// comparison stays a total order.
	switch {
	case math.IsNaN(a) && math.IsNaN(b):
		return 0
	case math.IsNaN(a):
		return 1
	default:
		return -1
	}
}

// Coerce converts a value to a column's declared kind for storage. Only the
// widening INT -> FLOAT conversion is implicit; everything else must match
// exactly. NULL passes through unchanged and is checked separately against
// the column's NOT NULL constraint.
func Coerce(v Value, want Kind) (Value, error) {
	if v.IsNull() || v.kind == want {
		return v, nil
	}
	if v.kind == KindInt && want == KindFloat {
		return Float(float64(v.i)), nil
	}
	return Value{}, fmt.Errorf("%w: cannot store %s in a %s column", ErrType, v.kind, want)
}

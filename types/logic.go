package types

// Bool3 is SQL's three-valued boolean: TRUE, FALSE, or UNKNOWN. A
// comparison with NULL produces UNKNOWN, and only TRUE keeps a row in a
// WHERE clause.
//
// The constants are ordered False < Unknown < True on purpose: in Kleene's
// three-valued logic AND is the minimum of its operands, OR is the maximum,
// and NOT is the reflection around Unknown. The implementations below are
// exactly that, which is why they cannot disagree with the truth tables.
type Bool3 uint8

// The three logical values.
const (
	False   Bool3 = 0
	Unknown Bool3 = 1
	True    Bool3 = 2
)

// B3 lifts a Go bool into the three-valued domain.
func B3(b bool) Bool3 {
	if b {
		return True
	}
	return False
}

// And is the three-valued conjunction: the minimum of the two operands.
// FALSE AND UNKNOWN is FALSE, because the result is false whatever the
// unknown operand turns out to be.
func (t Bool3) And(u Bool3) Bool3 {
	if u < t {
		return u
	}
	return t
}

// Or is the three-valued disjunction: the maximum of the two operands.
// TRUE OR UNKNOWN is TRUE, for the mirror-image reason.
func (t Bool3) Or(u Bool3) Bool3 {
	if u > t {
		return u
	}
	return t
}

// Not negates: TRUE becomes FALSE, FALSE becomes TRUE, and UNKNOWN stays
// UNKNOWN. NOT does not resolve an unknown into a known.
func (t Bool3) Not() Bool3 { return True - t }

// IsTrue reports whether the value is TRUE. This is the predicate a WHERE
// clause applies: rows whose condition is FALSE and rows whose condition is
// UNKNOWN are both discarded.
func (t Bool3) IsTrue() bool { return t == True }

// String renders the value as SQL spells it.
func (t Bool3) String() string {
	switch t {
	case False:
		return "FALSE"
	case True:
		return "TRUE"
	default:
		return "UNKNOWN"
	}
}

// Value converts the logical value back into a Value: UNKNOWN becomes NULL.
func (t Bool3) Value() Value {
	if t == Unknown {
		return Null()
	}
	return Bool(t == True)
}

// Truth reads a Value as a condition. NULL is UNKNOWN, a boolean is itself,
// and anything else is a type error — keelsql does not treat 0 or the empty
// string as false.
func Truth(v Value) (Bool3, error) {
	switch v.Kind() {
	case KindNull:
		return Unknown, nil
	case KindBool:
		return B3(v.AsBool()), nil
	}
	return Unknown, wrapType("a %s is not a condition", v.Kind())
}

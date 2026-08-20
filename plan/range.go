package plan

import (
	"strings"

	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/types"
)

// A Bound is one end of a scan range. Set distinguishes "no bound" from a
// bound whose value happens to be the zero Value (NULL).
type Bound struct {
	Value     types.Value
	Inclusive bool
	Set       bool
}

// A Range is the interval of a sorted column that a scan has to visit. It
// is the output of predicate pushdown: `WHERE pk >= 10 AND pk < 20` becomes
// the half-open range [10, 20).
type Range struct {
	Lo, Hi Bound
}

// Bounded reports whether the range constrains anything at all.
func (r Range) Bounded() bool { return r.Lo.Set || r.Hi.Set }

// IsPoint reports whether the range is a single value, which is what an
// equality predicate produces.
func (r Range) IsPoint() bool {
	if !r.Lo.Set || !r.Hi.Set || !r.Lo.Inclusive || !r.Hi.Inclusive {
		return false
	}
	return types.Equal(r.Lo.Value, r.Hi.Value)
}

// String renders the range in interval notation: a square bracket for an
// inclusive end, a round one for an exclusive end or an open side, so an
// equality reads as [5, 5] and `a > 3` reads as (3, +inf).
func (r Range) String() string {
	var sb strings.Builder
	switch {
	case !r.Lo.Set:
		sb.WriteString("(-inf")
	case r.Lo.Inclusive:
		sb.WriteString("[" + r.Lo.Value.SQL())
	default:
		sb.WriteString("(" + r.Lo.Value.SQL())
	}
	sb.WriteString(", ")
	switch {
	case !r.Hi.Set:
		sb.WriteString("+inf)")
	case r.Hi.Inclusive:
		sb.WriteString(r.Hi.Value.SQL() + "]")
	default:
		sb.WriteString(r.Hi.Value.SQL() + ")")
	}
	return sb.String()
}

// Empty reports whether the range cannot contain anything, which happens
// when contradictory predicates are combined: `a > 5 AND a < 3`.
func (r Range) Empty() bool {
	if !r.Lo.Set || !r.Hi.Set {
		return false
	}
	c := types.Order(r.Lo.Value, r.Hi.Value)
	if c > 0 {
		return true
	}
	return c == 0 && (!r.Lo.Inclusive || !r.Hi.Inclusive)
}

// Intersect narrows the range with another, keeping the tighter of each
// bound. It is how several predicates on one column combine.
func (r Range) Intersect(o Range) Range {
	out := r
	if o.Lo.Set && (!out.Lo.Set || tighterLo(o.Lo, out.Lo)) {
		out.Lo = o.Lo
	}
	if o.Hi.Set && (!out.Hi.Set || tighterHi(o.Hi, out.Hi)) {
		out.Hi = o.Hi
	}
	return out
}

func tighterLo(a, b Bound) bool {
	c := types.Order(a.Value, b.Value)
	if c != 0 {
		return c > 0
	}
	return !a.Inclusive && b.Inclusive
}

func tighterHi(a, b Bound) bool {
	c := types.Order(a.Value, b.Value)
	if c != 0 {
		return c < 0
	}
	return !a.Inclusive && b.Inclusive
}

// Selectivity is the planner's guess at the fraction of rows a range keeps.
// keelsql keeps no statistics — no row counts, no histograms — so these are
// fixed guesses, not estimates. They only have to be ordered correctly:
// a point lookup beats a bounded range, which beats an open-ended one,
// which beats reading the table.
func (r Range) Selectivity() float64 {
	switch {
	case r.IsPoint():
		return 0.001
	case r.Lo.Set && r.Hi.Set:
		return 0.05
	case r.Bounded():
		return 0.3
	}
	return 1
}

// KeyBytes turns the range into the half-open byte range [start, end) that
// keelstore's iterator wants, given the prefix the scanned keys share.
//
// The same code serves a primary-key scan, whose keys are exactly
// prefix+value, and a secondary index scan, whose keys are
// prefix+value+primary key. That works because an exclusive bound is
// expressed with keycodec.PrefixEnd, which steps past *every* key that
// starts with the encoded value, whatever follows it.
func (r Range) KeyBytes(prefix []byte) (start, end []byte) {
	start = append([]byte(nil), prefix...)
	if r.Lo.Set {
		lo := keycodec.Encode(append([]byte(nil), prefix...), r.Lo.Value)
		if r.Lo.Inclusive {
			start = lo
		} else {
			start = keycodec.PrefixEnd(lo)
		}
	}

	end = keycodec.PrefixEnd(prefix)
	if r.Hi.Set {
		hi := keycodec.Encode(append([]byte(nil), prefix...), r.Hi.Value)
		if r.Hi.Inclusive {
			end = keycodec.PrefixEnd(hi)
		} else {
			end = hi
		}
	}
	return start, end
}

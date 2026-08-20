// Package lexer turns SQL text into tokens.
//
// It is written by hand — no generator, no regular expressions — and it
// keeps a byte offset, line and column for every token so that a parse
// error can point at the place in the query that caused it.
package lexer

import (
	"fmt"
	"strings"
)

// Kind classifies a token.
type Kind uint8

// The token kinds.
const (
	EOF     Kind = iota // end of input
	Ident               // an identifier: users, "MixedCase"
	Keyword             // a reserved word, always stored upper-cased
	Int                 // an integer literal
	Float               // a floating-point literal
	String              // a quoted string literal, escapes already resolved
	Punct               // an operator or a separator
)

// String names the kind for error messages.
func (k Kind) String() string {
	switch k {
	case EOF:
		return "end of input"
	case Ident:
		return "identifier"
	case Keyword:
		return "keyword"
	case Int:
		return "integer"
	case Float:
		return "float"
	case String:
		return "string"
	case Punct:
		return "operator"
	}
	return "token"
}

// Pos is a location in the input. Line and Column are 1-based, Offset is a
// 0-based byte index, so Offset can slice the source directly.
type Pos struct {
	Line   int
	Column int
	Offset int
}

// String renders the position the way error messages spell it.
func (p Pos) String() string { return fmt.Sprintf("line %d, column %d", p.Line, p.Column) }

// A Token is one lexical unit.
//
// Text carries the token's meaning rather than its exact spelling:
// keywords are upper-cased, unquoted identifiers are lower-cased, string
// literals have their escapes resolved, and numbers keep their source form
// for the parser to convert.
type Token struct {
	Kind Kind
	Text string
	Pos  Pos
}

// Is reports whether the token is a keyword or operator with the given
// text. The text is compared as stored, so keywords must be passed
// upper-cased.
func (t Token) Is(text string) bool {
	return (t.Kind == Keyword || t.Kind == Punct) && t.Text == text
}

// Describe renders the token for an error message.
func (t Token) Describe() string {
	switch t.Kind {
	case EOF:
		return "end of input"
	case String:
		return fmt.Sprintf("string %q", t.Text)
	case Ident:
		return fmt.Sprintf("identifier %q", t.Text)
	case Keyword:
		return fmt.Sprintf("keyword %s", t.Text)
	}
	return fmt.Sprintf("%q", t.Text)
}

// String renders the token for tests and debugging.
func (t Token) String() string { return fmt.Sprintf("%s(%s)", t.Kind, t.Text) }

// keywords are the reserved words. Type names (INT, TEXT, …) and aggregate
// names (COUNT, SUM, …) are deliberately absent: they are ordinary
// identifiers that the parser recognises by position, so a column may still
// be called "count".
var keywords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		AND AS ASC BEGIN BETWEEN BY COMMIT CREATE DELETE DESC DISTINCT DROP
		EXISTS EXPLAIN FALSE FROM GROUP IF IN INDEX INSERT INTO IS KEY
		LIKE LIMIT NOT NULL OFFSET ON OR ORDER PRIMARY ROLLBACK SELECT SET
		TABLE TRANSACTION TRUE UPDATE VALUES WHERE`) {
		keywords[w] = true
	}
}

// IsKeyword reports whether word is reserved. The comparison is
// case-insensitive, as SQL requires.
func IsKeyword(word string) bool { return keywords[strings.ToUpper(word)] }

// An Error is a lexical error together with the position that produced it.
type Error struct {
	Msg string
	Pos Pos
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("keelsql: syntax error at %s: %s", e.Pos, e.Msg)
}

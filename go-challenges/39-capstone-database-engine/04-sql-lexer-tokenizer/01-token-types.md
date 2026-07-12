# Exercise 1: Token Types and the Token Struct

Every lexer rests on one humble object: the token. Before a single byte of SQL can be scanned you must decide what a token *is* — what categories exist, what each one carries, and how the rest of the engine asks "is this a keyword?" without a map lookup. This exercise builds that foundation: a `TokenType` enumeration that brackets its keyword block with unexported sentinels, a five-field `Token` struct that records position for error reporting, and the `IsKeyword` range check the parser leans on.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             TokenType, the token constant block, IsKeyword, Token, String
cmd/
  demo/
    main.go          construct a few tokens and print their debug form and keyword-ness
token_test.go        IsKeyword brackets the keyword block; String format; type identity
```

- Files: `token.go`, `cmd/demo/main.go`, `token_test.go`.
- Implement: the `TokenType` constant block with `keywordStart`/`keywordEnd` sentinels, `(TokenType).IsKeyword`, the `Token` struct, and `(Token).String`.
- Test: `token_test.go` asserts `IsKeyword` is true for keywords and false for literals/operators/sentinels, and that `String` renders the documented `Token(type, lit, line:col)` form.
- Verify: `go test -run 'TestToken' -race ./...`

### Why a sentinel-bracketed enum, and why position lives in the token

A token type is just an integer, but the *layout* of those integers is a design decision. SQL has roughly fifty keywords, and the parser asks "is this token a keyword?" constantly — for example to reject `SELECT FROM FROM` or to decide whether a bareword can be a column name. The naive answer is a second `map[TokenType]bool`, but there is a cheaper one: lay the keyword constants out as one contiguous run in the `iota` block and bracket them with two unexported sentinels, `keywordStart` and `keywordEnd`. Then `IsKeyword` is a single range check, `t > keywordStart && t < keywordEnd`, with no allocation and no lookup. The sentinels are unexported because they are not real tokens; they exist only to mark the boundaries of the run, and nothing outside the package should compare against them.

The second decision is that every token carries its own position — `Pos` (byte offset), `Line`, and `Col` — rather than leaving the parser to recompute it. A lexer that drops position information forces every downstream error message into the useless "syntax error" form. Carrying `Line` and `Col` (both 1-based, the convention every editor and SQL client uses) means a parser or reporter can say `4:12: unexpected token` for free, and `Pos` lets a tool highlight the exact byte span in the source. The cost is twenty-four bytes per token, which is nothing next to the debuggability it buys.

`Literal` carries the token's text, with one deliberate asymmetry baked in here so the scanner can honor it later: for identifiers and literals it is the raw source text, but for keywords it is the *canonical uppercase* spelling. That choice lets every parse site compare `tok.Literal == "SELECT"` without re-folding case on each comparison; the scanner does the `strings.ToUpper` once, when it recognizes the keyword.

Create `token.go`:

```go
package lexer

import "fmt"

// TokenType identifies the category of a SQL token.
type TokenType int

const (
	// Special tokens.
	TokenIllegal TokenType = iota
	TokenError
	TokenEOF

	// Literals.
	TokenIdent  // unquoted identifier
	TokenQIdent // double-quoted identifier
	TokenInt    // integer literal: 42
	TokenFloat  // float literal: 3.14, 1.5e10
	TokenString // single-quoted string literal

	// Keywords — sentinels bracket the keyword block for IsKeyword.
	keywordStart
	TokenSelect
	TokenFrom
	TokenWhere
	TokenInsert
	TokenInto
	TokenValues
	TokenUpdate
	TokenSet
	TokenDelete
	TokenCreate
	TokenTable
	TokenDrop
	TokenIndex
	TokenOn
	TokenAnd
	TokenOr
	TokenNot
	TokenNull
	TokenTrue
	TokenFalse
	TokenOrder
	TokenBy
	TokenAsc
	TokenDesc
	TokenLimit
	TokenOffset
	TokenJoin
	TokenLeft
	TokenRight
	TokenInner
	TokenOuter
	TokenGroup
	TokenHaving
	TokenAs
	TokenDistinct
	TokenCount
	TokenSum
	TokenAvg
	TokenMin
	TokenMax
	TokenIn
	TokenBetween
	TokenLike
	TokenIs
	TokenExists
	TokenPrimary
	TokenKey
	TokenInteger
	TokenText
	TokenReal
	TokenBoolean
	TokenBegin
	TokenCommit
	TokenRollback
	keywordEnd

	// Operators.
	TokenPlus     // +
	TokenMinus    // -
	TokenAsterisk // *
	TokenSlash    // /
	TokenEq       // =
	TokenNeq      // != or <>
	TokenLt       // <
	TokenGt       // >
	TokenLtEq     // <=
	TokenGtEq     // >=

	// Punctuation.
	TokenLParen    // (
	TokenRParen    // )
	TokenComma     // ,
	TokenSemicolon // ;
	TokenDot       // .
	TokenColon     // :

	// Comments — only emitted when KeepComments is true.
	TokenLineComment  // -- ...
	TokenBlockComment // /* ... */
)

// IsKeyword reports whether t is a SQL keyword.
func (t TokenType) IsKeyword() bool {
	return t > keywordStart && t < keywordEnd
}

// Token is a single lexical unit in a SQL source string.
type Token struct {
	Type    TokenType
	Literal string // raw text for identifiers/literals; canonical uppercase for keywords
	Pos     int    // byte offset of the first character in the source
	Line    int    // 1-based line number
	Col     int    // 1-based column number
}

// String returns a debug representation.
func (tok Token) String() string {
	return fmt.Sprintf("Token(%d, %q, %d:%d)", tok.Type, tok.Literal, tok.Line, tok.Col)
}
```

`keywordStart` and `keywordEnd` are unexported sentinels. `IsKeyword` uses them to range-check in O(1) without a map lookup; because the keyword constants are emitted as one contiguous `iota` run, any future keyword added between the sentinels is covered automatically.

### The runnable demo

The demo constructs three tokens by hand — a keyword, an identifier, and an integer — and prints each one's debug string alongside the answer `IsKeyword` gives. It is the smallest program that exercises both methods and makes the keyword/non-keyword boundary visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lexer "example.com/token-types"
)

func main() {
	toks := []lexer.Token{
		{Type: lexer.TokenSelect, Literal: "SELECT", Line: 1, Col: 1},
		{Type: lexer.TokenIdent, Literal: "users", Line: 1, Col: 8},
		{Type: lexer.TokenInt, Literal: "42", Line: 1, Col: 14},
	}
	for _, tok := range toks {
		fmt.Printf("%-24s keyword=%v\n", tok.String(), tok.Type.IsKeyword())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Token(9, "SELECT", 1:1)  keyword=true
Token(3, "users", 1:8)   keyword=false
Token(5, "42", 1:14)     keyword=false
```

The numeric type values fall out of the `iota` block: `TokenIdent` is 3, `TokenInt` is 5, and `TokenSelect` is 9 because the eight special-and-literal constants plus the `keywordStart` sentinel precede it.

### Tests

The tests pin the two properties the rest of the engine relies on. `TestTokenIsKeyword` walks a representative keyword and a representative non-keyword from each surrounding region — special, literal, operator, punctuation — and asserts the range check classifies each correctly, including that the sentinels themselves report false. `TestTokenString` pins the exact `String` format, since debug output and some test assertions elsewhere depend on it byte-for-byte.

Create `token_test.go`:

```go
package lexer

import "testing"

func TestTokenIsKeyword(t *testing.T) {
	t.Parallel()

	keywords := []TokenType{
		TokenSelect, TokenFrom, TokenWhere, TokenJoin, TokenRollback,
	}
	for _, tt := range keywords {
		if !tt.IsKeyword() {
			t.Errorf("IsKeyword(%d) = false, want true", tt)
		}
	}

	nonKeywords := []TokenType{
		TokenIllegal, TokenError, TokenEOF,
		TokenIdent, TokenQIdent, TokenInt, TokenFloat, TokenString,
		TokenPlus, TokenEq, TokenLParen, TokenSemicolon, TokenDot,
		keywordStart, keywordEnd,
	}
	for _, tt := range nonKeywords {
		if tt.IsKeyword() {
			t.Errorf("IsKeyword(%d) = true, want false", tt)
		}
	}
}

func TestTokenString(t *testing.T) {
	t.Parallel()

	tok := Token{Type: TokenSelect, Literal: "SELECT", Pos: 0, Line: 1, Col: 1}
	got := tok.String()
	want := `Token(9, "SELECT", 1:1)`
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestTokenTypeIdentity(t *testing.T) {
	t.Parallel()

	// The keyword run is contiguous and bracketed: every constant strictly
	// between the sentinels is a keyword, and the sentinels are not.
	if !(keywordStart < TokenSelect && TokenRollback < keywordEnd) {
		t.Fatalf("keyword block is not bracketed by the sentinels")
	}
	if TokenIdent == TokenInt {
		t.Fatalf("distinct token types collapsed to the same value")
	}
}
```

## Review

The token model is sound when `IsKeyword` and the constant layout agree. Every constant emitted strictly between `keywordStart` and `keywordEnd` must report `IsKeyword() == true`, every constant outside that run — specials, literals, operators, punctuation, and the sentinels themselves — must report false, and adding a keyword inside the run must require no change to `IsKeyword`. Confirm `String` renders the exact `Token(type, lit, line:col)` form, since downstream debug output depends on it, and that `Literal` is documented to hold raw text for identifiers and the canonical uppercase spelling for keywords — the asymmetry the scanner will honor in later exercises.

The common mistake here is reaching for a second `map[TokenType]bool` to answer `IsKeyword`. That works but costs an allocation-backed lookup and, worse, a second source of truth that drifts the day someone adds a keyword constant but forgets the map entry. The contiguous-run-plus-sentinels layout makes the constant block itself the single source of truth: a keyword is a keyword precisely because of where it sits in the `iota` sequence.

## Resources

- [Go Specification: Iota](https://go.dev/ref/spec#Iota) — how the `iota` constant generator produces the contiguous integer run this token block depends on.
- [pkg.go.dev: go/token](https://pkg.go.dev/go/token) — the standard library's own token-type enumeration for Go source, the direct analogue of this design.
- [pkg.go.dev: fmt](https://pkg.go.dev/fmt) — `fmt.Sprintf` and the `%q` verb used to render the token's quoted literal in `String`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-scanner-and-keywords.md](02-scanner-and-keywords.md)
